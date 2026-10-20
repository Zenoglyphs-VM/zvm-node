// Package index keeps the ZVM's NFT tables: collections, their tokens and
// their owners, and the sales the exchange settled.
//
// It is an indexer.Plugin rather than a service of its own, which is a
// deliberate constraint. Ownership here is derived from the same Transfer logs
// the chain's index reads, so the two have to move together: an L1 reorg
// rewinds both inside one transaction, and a separate process indexing the
// same logs would, between its commit and the node's, hold owners for blocks
// that no longer exist.
//
// Unfilled orders are not here. They are signatures, not chain state, so they
// live in the order book (../orderbook), which this index may be rebuilt
// underneath without losing (D28).
package nft

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
)

// Index is the plugin. Apps is asked on every pass rather than read once, so a
// deployment file written while the node runs is picked up — and replayed, in
// the jobs below.
type Index struct {
	host  indexer.Host
	apps  AppsFunc
	media MetadataResolver

	mu    sync.Mutex
	dirty map[string]bool

	// What this block's metadata reads have cost so far (metadataBudget).
	budgetBlock uint64
	spent       time.Duration
	warned      bool
}

// metadataBudget is how long one block's tokenURI and contractURI reads may
// take together. A Transfer is cheap to emit — a contract can log thousands in
// a block without owning anything — and each first sighting of a token costs
// this node a tokenURI call, so a block of fake mints held the whole index
// back for as long as its author paid for. Past the budget a token is indexed
// without its metadata; a MetadataUpdate reads it again.
var metadataBudget = 3 * time.Second

// readURI makes a URI call inside the block's budget, or answers "" once the
// budget is spent.
func (x *Index) readURI(ctx context.Context, block uint64, addr common.Address, calldata []byte) string {
	x.mu.Lock()
	if x.budgetBlock != block {
		x.budgetBlock, x.spent, x.warned = block, 0, false
	}
	if x.spent >= metadataBudget {
		warn := !x.warned
		x.warned = true
		x.mu.Unlock()
		if warn {
			x.log().Warn("metadata reads for this block took their whole budget; the rest are indexed without it",
				"block", block, "budget", metadataBudget)
		}
		return ""
	}
	x.mu.Unlock()
	start := time.Now()
	uri := x.host.CallURI(ctx, addr, calldata)
	x.mu.Lock()
	x.spent += time.Since(start)
	x.mu.Unlock()
	return uri
}

// Config is what the node's main hands this plugin.
type Config struct {
	// Apps names the exchange and the collection factory. Their events are
	// trusted only from these addresses — anyone can emit an OrderFilled.
	Apps AppsFunc
	// Media is somewhere to read token metadata from without fetching
	// anything: the node's own store. Optional.
	Media MetadataResolver
}

func New(cfg Config) *Index {
	x := &Index{apps: cfg.Apps, media: cfg.Media, dirty: map[string]bool{}}
	if x.apps == nil {
		x.apps = func() Apps { return Apps{} }
	}
	return x
}

func (x *Index) Name() string { return "nft" }

func (x *Index) Attach(h indexer.Host) error { x.host = h; return nil }

func (x *Index) Schema() string { return nftSchema }

func (x *Index) Handle(ctx context.Context, dbtx *sql.Tx, r indexer.LogRow) error {
	return x.indexNFTLog(ctx, dbtx, r)
}

// Jobs backfills the NFT tables over logs already indexed, and replays the
// exchange's and factory's own logs the first time the node is told where they
// are — which is why the deployment is read here and not once at startup.
func (x *Index) Jobs() []indexer.Job {
	where, args := nftWhere()
	jobs := []indexer.Job{{Key: "nft_v1", Where: where, Args: args}}
	apps := x.apps()
	for _, a := range []common.Address{apps.Exchange, apps.CollectionFactory} {
		if a != (common.Address{}) {
			jobs = append(jobs, indexer.Job{Key: "app:" + a.Hex(), Where: "l.address = ?", Args: []any{a.Hex()}})
		}
	}
	return jobs
}

func (x *Index) Deletes() []string {
	return []string{
		"DELETE FROM nft_sales WHERE block > ?",
		"DELETE FROM nft_tokens WHERE minted_block > ?",
		"DELETE FROM nft_collections WHERE first_block > ?",
		"DELETE FROM nft_approvals WHERE block > ?",
	}
}

// Restores puts each surviving token's owner back to what the surviving
// transfers say, and drops approvals, which a reorg cannot be assumed to have
// preserved.
func (x *Index) Restores() []string {
	return []string{
		`UPDATE nft_tokens SET approved = NULL, owner = COALESCE((SELECT t.to_addr FROM token_transfers t
			WHERE t.token = nft_tokens.collection AND t.token_id = nft_tokens.token_id ORDER BY t.block DESC, t.idx DESC LIMIT 1), owner)
			WHERE updated_block > ?`,
	}
}

func (x *Index) AfterBlock(dbtx *sql.Tx, block uint64) error {
	return x.refreshDirtyCollections(dbtx, block)
}

// Rebuild recomputes every collection's totals, which a rewind cannot repair by
// deleting rows because a total is a sum over the range that survived.
func (x *Index) Rebuild(dbtx *sql.Tx, block uint64) error {
	return x.rebuildCollectionStats(dbtx, block)
}

// Stale is true for an index that has collections but no statistics — one
// built before this plugin's aggregate table existed.
func (x *Index) Stale(db *sql.DB) bool {
	var stats, collections int64
	db.QueryRow("SELECT COUNT(*) FROM nft_collection_stats").Scan(&stats)
	db.QueryRow("SELECT COUNT(*) FROM nft_collections").Scan(&collections)
	return stats == 0 && collections > 0
}

// The three things the host lends this plugin, named the way the code that
// moved here already spelled them.
func (x *Index) eth() *ethclient.Client { return x.host.Eth() }
func (x *Index) log() *slog.Logger      { return x.host.Log() }
func (x *Index) callString(ctx context.Context, addr common.Address, sig string) string {
	return x.host.CallString(ctx, addr, sig)
}
