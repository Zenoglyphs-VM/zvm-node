package indexer

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// A Plugin indexes something this chain does not define — an application's own
// events, a marketplace — into the same database, in the same transaction, as
// the chain's own index.
//
// Being in the same transaction is the whole point. An L1 reorg rewinds the
// chain's tables and everything derived from them at once; a plugin indexing
// the same logs from its own process would rewind separately, and between the
// two commits an application's rows would refer to blocks that no longer
// exist. So a plugin is a Go package the node is built with, not a service it
// talks to.
//
// Every method is called on the indexer's goroutine, inside its write
// transaction where one is passed.
type Plugin interface {
	// Name identifies the plugin in log lines.
	Name() string

	// Attach hands the plugin the indexer it is part of. It is called once by
	// New, before any schema runs, and is how a plugin gets an eth client
	// without being constructed with one.
	Attach(h Host) error

	// Schema is executed once at New and must be idempotent — it runs on every
	// start, against databases that may already have its tables.
	Schema() string

	// Handle is offered every log the indexer does not consume itself, plus
	// every ERC-721 Transfer. A log it has no interest in is not an error.
	Handle(ctx context.Context, dbtx *sql.Tx, r LogRow) error

	// Jobs are backfills over logs that are already indexed: a table added
	// after this chain had history, or an application contract the node has
	// just been told the address of. Each runs once per key and is fed to
	// Handle. Jobs is asked again on every pass, so a deployment file written
	// while the node runs is picked up and replayed.
	Jobs() []Job

	// Deletes are `DELETE FROM … WHERE <a block column> > ?` statements, run
	// on a rewind with the height that survived.
	Deletes() []string

	// Restores run after Deletes, and put state that is not a row — a current
	// owner, a current balance — back to what the surviving rows say. Each
	// takes the same height as its only parameter.
	Restores() []string

	// AfterBlock is called once per indexed block and once per backfill, with
	// that block's height, for work batched across a block's logs.
	AfterBlock(dbtx *sql.Tx, block uint64) error

	// Rebuild recomputes what a rewind cannot repair by deleting rows: a sum,
	// an average, anything that mixes blocks. It runs inside the rewind's
	// transaction, and at startup whenever Stale says so.
	Rebuild(dbtx *sql.Tx, block uint64) error

	// Stale reports whether this plugin's derived tables need a Rebuild before
	// the node starts serving — an index that predates them, or one built
	// under an older definition.
	Stale(db *sql.DB) bool
}

// A Job is one backfill: a WHERE clause over the indexed `logs l`, remembered
// under Key so it runs once.
type Job struct {
	Key   string
	Where string
	Args  []any
}

// A Host is what the indexer lends its plugins: the clients and the one
// contract-reading helper they would otherwise each reimplement.
type Host interface {
	Eth() *ethclient.Client
	Log() *slog.Logger
	// CallString makes an eth_call whose return value is a short string — a
	// name, a symbol — and yields "" for a contract that does not implement
	// it. The call's gas and the answer's length are both capped.
	CallString(ctx context.Context, addr common.Address, sig string) string
	// CallURI makes an eth_call whose return value is a URI, which may be a
	// data: URI holding a whole document: tokenURI(id), contractURI(). It has
	// a larger gas and size allowance than CallString, and yields "" for an
	// answer past MaxURIBytes rather than a truncated URI.
	CallURI(ctx context.Context, addr common.Address, calldata []byte) string
}

// Eth, Log, CallString and CallURI satisfy Host.
func (ix *Indexer) Eth() *ethclient.Client { return ix.eth }
func (ix *Indexer) Log() *slog.Logger      { return ix.log }
func (ix *Indexer) CallString(ctx context.Context, addr common.Address, sig string) string {
	return ix.callString(ctx, addr, sig)
}
func (ix *Indexer) CallURI(ctx context.Context, addr common.Address, calldata []byte) string {
	out, err := ix.eth.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: calldata, Gas: uriGas}, nil)
	if err != nil {
		return ""
	}
	uri := DecodeABIString(out)
	if len(uri) > MaxURIBytes {
		return ""
	}
	return strings.ToValidUTF8(uri, "")
}
