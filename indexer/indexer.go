// Package indexer turns derived ZVM blocks into the tables the explorer
// reads: transactions with the lane and the NoM block they came from, logs,
// token transfers, contracts and bridge movements — and, from the same logs,
// Uniswap pools and their trades (dex.go) and NFTs and their market (nft.go).
//
// It is a reader. Nothing here decides consensus; if the index is lost it is
// rebuilt from op-geth and the derivation store.
package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
)

var (
	topicTransfer       = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	topicTransferSingle = crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))
	topicTransferBatch  = crypto.Keccak256Hash([]byte("TransferBatch(address,address,address,uint256[],uint256[])"))

	topicWithdrawalInitiated = crypto.Keccak256Hash([]byte("WithdrawalInitiated(uint256,address,address,bytes10,uint256)"))
	topicWithdrawalPaid      = crypto.Keccak256Hash([]byte("WithdrawalPaid(uint256,bytes32)"))
	topicPayoutAnomaly       = crypto.Keccak256Hash([]byte("PayoutAnomaly(uint256,bytes32,uint8)"))
	topicQSRDeposited        = crypto.Keccak256Hash([]byte("QSRDeposited(bytes32,address,uint256)"))
)

const schema = `
CREATE TABLE IF NOT EXISTS blocks (
	number    INTEGER PRIMARY KEY,
	hash      TEXT    NOT NULL,
	parent    TEXT    NOT NULL,
	timestamp INTEGER NOT NULL,
	momentum  INTEGER NOT NULL,
	txs       INTEGER NOT NULL,
	user_txs  INTEGER NOT NULL,
	gas_used  INTEGER NOT NULL,
	gas_limit INTEGER NOT NULL,
	base_fee  TEXT    NOT NULL,
	size      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS blocks_momentum ON blocks(momentum);
-- Lets a time-bounded query (the last 24h of transactions, say) start from a
-- range scan instead of walking every block to find where the window begins.
CREATE INDEX IF NOT EXISTS blocks_timestamp ON blocks(timestamp);
CREATE TABLE IF NOT EXISTS txs (
	hash       TEXT PRIMARY KEY,
	block      INTEGER NOT NULL,
	idx        INTEGER NOT NULL,
	timestamp  INTEGER NOT NULL,
	type       INTEGER NOT NULL,
	lane       TEXT    NOT NULL,
	nom_hash   TEXT,
	from_addr  TEXT    NOT NULL,
	to_addr    TEXT,
	value      TEXT    NOT NULL,
	mint       TEXT,
	gas        INTEGER NOT NULL,
	gas_used   INTEGER NOT NULL,
	gas_price  TEXT    NOT NULL,
	fee        TEXT    NOT NULL,
	nonce      INTEGER NOT NULL,
	status     INTEGER NOT NULL,
	input_size INTEGER NOT NULL,
	selector   TEXT,
	contract   TEXT
);
CREATE INDEX IF NOT EXISTS txs_block ON txs(block DESC, idx);
CREATE INDEX IF NOT EXISTS txs_lane ON txs(lane, block DESC);
CREATE TABLE IF NOT EXISTS logs (
	tx      TEXT    NOT NULL,
	idx     INTEGER NOT NULL,
	block   INTEGER NOT NULL,
	address TEXT    NOT NULL,
	topic0  TEXT,
	topic1  TEXT,
	topic2  TEXT,
	topic3  TEXT,
	data    BLOB,
	PRIMARY KEY (tx, idx)
);
CREATE INDEX IF NOT EXISTS logs_address ON logs(address, block DESC);
CREATE INDEX IF NOT EXISTS logs_topic0 ON logs(topic0, block);
CREATE TABLE IF NOT EXISTS tokens (
	address    TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	name       TEXT,
	symbol     TEXT,
	decimals   INTEGER,
	first_block INTEGER NOT NULL,
	transfers  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS token_transfers (
	tx        TEXT    NOT NULL,
	idx       INTEGER NOT NULL,
	block     INTEGER NOT NULL,
	timestamp INTEGER NOT NULL,
	token     TEXT    NOT NULL,
	from_addr TEXT    NOT NULL,
	to_addr   TEXT    NOT NULL,
	value     TEXT,
	token_id  TEXT,
	PRIMARY KEY (tx, idx)
);
CREATE INDEX IF NOT EXISTS token_transfers_token ON token_transfers(token, block DESC);
CREATE INDEX IF NOT EXISTS token_transfers_from ON token_transfers(from_addr, block DESC);
CREATE INDEX IF NOT EXISTS token_transfers_to ON token_transfers(to_addr, block DESC);
CREATE TABLE IF NOT EXISTS contracts (
	address   TEXT PRIMARY KEY,
	creator   TEXT NOT NULL,
	tx        TEXT NOT NULL,
	block     INTEGER NOT NULL,
	code_size INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS bridge_flows (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	kind      TEXT    NOT NULL,
	wid       INTEGER,
	block     INTEGER NOT NULL,
	timestamp INTEGER NOT NULL,
	l2_tx     TEXT    NOT NULL,
	account   TEXT,
	token     TEXT,
	amount    TEXT,
	nom_block TEXT,
	note      TEXT
);
CREATE INDEX IF NOT EXISTS bridge_flows_block ON bridge_flows(block DESC);
CREATE INDEX IF NOT EXISTS bridge_flows_wid ON bridge_flows(wid);
CREATE TABLE IF NOT EXISTS index_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
`

type Indexer struct {
	db  *sql.DB
	eth *ethclient.Client
	cfg *rollup.Config
	log *slog.Logger

	signer types.Signer

	plugins []Plugin

	mu      sync.Mutex
	pairs   map[common.Address]*pairInfo
	dirty   map[string]bool
	candles *candleCache

	// Shared-database state must rewind inside the index transaction. The
	// post-commit callbacks are for notifications or state outside that DB.
	beforeRewindCommit []func(*sql.Tx, uint64) error
	afterRewind        []func(l2Number uint64)
}

// BeforeRewindCommit registers a participant in the shared database rewind.
// It must use the provided transaction for local SQL only, without committing
// it or opening another write transaction. An error aborts the entire rewind.
func (ix *Indexer) BeforeRewindCommit(fn func(*sql.Tx, uint64) error) {
	ix.beforeRewindCommit = append(ix.beforeRewindCommit, fn)
}

// AfterRewind registers a post-commit notification. Persistent state in the
// shared database belongs in BeforeRewindCommit so a refusal can roll back both.
func (ix *Indexer) AfterRewind(fn func(l2Number uint64)) {
	ix.afterRewind = append(ix.afterRewind, fn)
}

// ReconcileExtensionState closes an older startup gap where the index committed
// its rewind before an extension did. Participants use one local transaction
// and the persisted index head. Bind them before calling this method, and keep
// followers and extension workers idle until it succeeds. This does not validate
// that head against an independent chain or send post-rewind notifications.
func (ix *Indexer) ReconcileExtensionState(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ix.db == nil {
		return errors.New("extension recovery requires an initialized index")
	}
	if len(ix.beforeRewindCommit) == 0 {
		return nil
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(tx)
	defer rollback()
	var height sql.NullInt64
	if err := tx.QueryRow("SELECT MAX(number) FROM blocks").Scan(&height); err != nil {
		return err
	}
	if height.Valid && height.Int64 < 0 {
		return errors.New("invalid persisted index height during extension recovery")
	}
	for _, fn := range ix.beforeRewindCommit {
		if err := fn(tx, uint64(height.Int64)); err != nil {
			return fmt.Errorf("reconciling extension state: %w", err)
		}
	}
	return commit()
}

// parentMismatch is IndexBlock refusing a block whose parent is not the block
// the index holds below it: the chain was reorganised under the index and the
// rows below are an orphan's.
type parentMismatch struct {
	number uint64
	parent common.Hash
	stored string
}

func (e *parentMismatch) Error() string {
	return fmt.Sprintf("block %d's parent is %s, but the index holds %s at %d", e.number, e.parent.Hex(), e.stored, e.number-1)
}

// Schema is every table the index owns, in the order they are created. It is
// exported so that a package reading these tables — the markets extension —
// can build the real thing in a test rather than a hand-copied approximation
// of it, which is the version that stays right for about a month.
func Schema() []string { return []string{schema, dexSchema, aggregateSchema} }

func New(db *store.DB, eth *ethclient.Client, cfg *rollup.Config, log *slog.Logger, plugins ...Plugin) (*Indexer, error) {
	for _, s := range Schema() {
		if _, err := db.SQL().Exec(s); err != nil {
			return nil, fmt.Errorf("migrating the index: %w", err)
		}
	}
	// addr_txs held a row per address per transaction to answer two questions
	// an index on `txs` and one counter answer better (D21). It is dropped
	// rather than left behind: the index is derived, and the space is the
	// point.
	if _, err := db.SQL().Exec("DROP TABLE IF EXISTS addr_txs"); err != nil {
		return nil, fmt.Errorf("migrating the index: %w", err)
	}
	// chain_stats predates the `basis` column below on a node that ran the
	// brief window where max_tps counted every submitted transaction, reverts
	// included; SQLite has no "ADD COLUMN IF NOT EXISTS".
	if _, err := db.SQL().Exec("ALTER TABLE chain_stats ADD COLUMN basis TEXT NOT NULL DEFAULT ''"); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrating the index: %w", err)
	}
	ix := &Indexer{
		db: db.SQL(), eth: eth, cfg: cfg, log: log,
		signer:  types.LatestSignerForChainID(cfg.L2ChainIDBig()),
		plugins: plugins,
		pairs:   map[common.Address]*pairInfo{},
		candles: newCandleCache(),
	}
	// A plugin is attached before its schema runs, so one that has to read the
	// chain to decide on a migration can.
	for _, p := range plugins {
		if err := p.Attach(ix); err != nil {
			return nil, fmt.Errorf("attaching the %s index: %w", p.Name(), err)
		}
		if _, err := db.SQL().Exec(p.Schema()); err != nil {
			return nil, fmt.Errorf("migrating the %s index: %w", p.Name(), err)
		}
	}
	if err := ix.backfillAggregates(); err != nil {
		return nil, fmt.Errorf("building the index's aggregates: %w", err)
	}
	return ix, nil
}

// backfillAggregates fills the precomputed tables on a node whose index
// predates them. Each is derived from rows that are already there, so this is
// a one-off pass rather than a re-index.
func (ix *Indexer) backfillAggregates() error {
	var addrs, candles int64
	ix.db.QueryRow("SELECT COUNT(*) FROM addr_stats").Scan(&addrs)
	ix.db.QueryRow("SELECT COUNT(*) FROM dex_candles").Scan(&candles)
	// basis, not a row count: an older node already has one row here, computed
	// under whatever max_tps used to count. A stale basis needs the same
	// rebuild as no row at all, not just "is there something there".
	var chainStatsBasis string
	ix.db.QueryRow("SELECT basis FROM chain_stats WHERE id = 1").Scan(&chainStatsBasis)
	var txs, events, blocks int64
	ix.db.QueryRow("SELECT COUNT(*) FROM txs").Scan(&txs)
	ix.db.QueryRow("SELECT COUNT(*) FROM dex_events").Scan(&events)
	ix.db.QueryRow("SELECT COUNT(*) FROM blocks").Scan(&blocks)
	var stalePlugins []Plugin
	for _, p := range ix.plugins {
		if p.Stale(ix.db) {
			stalePlugins = append(stalePlugins, p)
		}
	}
	// The empty chain needs the same zero/basis row as a rebuilt chain. Without
	// it, a zero-throughput block can create a different record on first index
	// than after restart or rewind, although the surviving blocks are identical.
	if (addrs > 0 || txs == 0) && (candles > 0 || events == 0) && len(stalePlugins) == 0 && chainStatsBasis == maxTPSBasis {
		return nil
	}
	// Stale and Head read through the database pool. Resolve them before the
	// write transaction holds a connection, including on a one-connection DB.
	var head uint64
	if len(stalePlugins) > 0 {
		var err error
		head, err = ix.Head()
		if err != nil {
			return err
		}
	}
	dbtx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(dbtx)
	defer rollback()
	if addrs == 0 && txs > 0 {
		ix.log.Info("building address counters from the transactions already indexed", "txs", txs)
		if err := rebuildAddrStats(dbtx); err != nil {
			return err
		}
	}
	if candles == 0 && events > 0 {
		ix.log.Info("building candles from the pool events already indexed", "events", events)
		if err := ix.rebuildCandles(dbtx, 0); err != nil {
			return err
		}
	}
	for _, p := range stalePlugins {
		ix.log.Info("rebuilding a plugin's derived tables", "plugin", p.Name())
		if err := p.Rebuild(dbtx, head); err != nil {
			return fmt.Errorf("rebuilding the %s index: %w", p.Name(), err)
		}
	}
	if chainStatsBasis != maxTPSBasis {
		if blocks > 0 {
			ix.log.Info("finding the chain's highest transactions-per-second block", "blocks", blocks)
		}
		if err := rebuildMaxTPS(dbtx); err != nil {
			return err
		}
	}
	return commit()
}

func (ix *Indexer) Head() (uint64, error) {
	var v sql.NullInt64
	if err := ix.db.QueryRow("SELECT MAX(number) FROM blocks").Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return uint64(v.Int64), nil
}

// Follow indexes everything from the store's head backwards to what is
// already indexed, then keeps up as blocks are derived.
//
// It goes by block number, so on its own it cannot see a reorg: a block
// replaced at a height already indexed would keep its orphan's rows for good.
// Two checks close that. Every block must extend the stored block below it
// (IndexBlock), and once at start the stored chain is checked for a link that
// already broke — which is how an index written before the first check repairs
// itself. Either one finding an orphan rewinds to where the index and the
// chain last agree, the way Blockscout and The Graph detect a reorg: by the
// parent hash, not by being told.
func (ix *Indexer) Follow(ctx context.Context, chainHead func() uint64) {
	repaired := false
	for {
		ix.catchUp(ctx)
		// Retried until it succeeds: the node starts following before the
		// engine necessarily answers.
		if !repaired {
			if err := ix.repairBrokenLink(ctx); err != nil {
				ix.log.Warn("checking the index's stored chain", "err", err)
			} else {
				repaired = true
			}
		}
		head := chainHead()
		indexed, err := ix.Head()
		if err != nil {
			ix.log.Warn("reading the index head", "err", err)
		}
		for n := indexed + 1; n <= head; n++ {
			if err := ix.IndexBlock(ctx, n); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				var pm *parentMismatch
				if errors.As(err, &pm) {
					ix.log.Warn("the chain reorganised under the index", "err", err)
					if err := ix.rewindToFork(ctx, n-1); err != nil {
						ix.log.Warn("rewinding the index to the fork", "err", err)
					}
					break
				}
				ix.log.Warn("indexing block", "number", n, "err", err)
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// repairBrokenLink finds the lowest stored block whose parent is not the
// stored block below it and, if there is one, rewinds to the fork. A broken
// link means one side of it is an orphan the index never dropped; walking down
// from below the link against the chain says which.
func (ix *Indexer) repairBrokenLink(ctx context.Context) error {
	var broken sql.NullInt64
	if err := ix.db.QueryRow(`SELECT MIN(b.number) FROM blocks b
		JOIN blocks p ON p.number = b.number - 1 WHERE b.parent != p.hash`).Scan(&broken); err != nil {
		return err
	}
	if !broken.Valid {
		return nil
	}
	ix.log.Warn("the index holds a block that does not link to the one below it", "number", broken.Int64)
	return ix.rewindToFork(ctx, uint64(broken.Int64)-1)
}

// rewindToFork walks down from `from` to the highest block the index and the
// chain agree on, and rewinds the index to it.
func (ix *Indexer) rewindToFork(ctx context.Context, from uint64) error {
	fork := from
	for ; fork > 0; fork-- {
		var stored string
		err := ix.db.QueryRow("SELECT hash FROM blocks WHERE number = ?", fork).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		hdr, err := ix.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(fork))
		if err != nil {
			return err
		}
		if hdr.Hash().Hex() == stored {
			break
		}
	}
	ix.log.Warn("rewinding the index to where it and the chain agree", "to", fork, "checkedFrom", from)
	return ix.Rewind(fork)
}

// catchUp fills a derived table from logs already indexed — the tables added
// after this chain had history — and replays an application contract's own
// logs the first time the node is told where that contract is.
func (ix *Indexer) catchUp(ctx context.Context) {
	dexIn, dexArgs := TopicList(dexTopics)
	type job struct {
		Job
		handle func(context.Context, *sql.Tx, LogRow) error
	}
	jobs := []job{{Job{Key: "dex_v1", Where: "l.topic0 IN (" + dexIn + ")", Args: dexArgs}, ix.indexDexLog}}
	for _, p := range ix.plugins {
		for _, j := range p.Jobs() {
			jobs = append(jobs, job{j, p.Handle})
		}
	}
	for _, j := range jobs {
		if err := ix.backfill(ctx, j.Key, j.Where, j.Args, j.handle); err != nil && ctx.Err() == nil {
			ix.log.Warn("backfilling", "job", j.Key, "err", err)
		}
	}
}

// Rewind drops everything above number: after an L1 reorg, when the deriver
// says so, or when the index finds an orphan itself. Then the AfterRewind
// hooks run.
func (ix *Indexer) Rewind(number uint64) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(tx)
	defer rollback()
	// The removed range still names the affected tokens here. Recompute only
	// their counters from the surviving prefix, without copying the history.
	if _, err := tx.Exec(`UPDATE tokens SET transfers = (
		SELECT COUNT(*) FROM token_transfers t WHERE t.token = tokens.address AND t.block <= ?)
		WHERE address IN (SELECT token FROM token_transfers WHERE block > ?)`, number, number); err != nil {
		return err
	}
	for _, stmt := range []string{
		"DELETE FROM blocks WHERE number > ?", "DELETE FROM txs WHERE block > ?",
		"DELETE FROM logs WHERE block > ?",
		"DELETE FROM token_transfers WHERE block > ?", "DELETE FROM contracts WHERE block > ?",
		"DELETE FROM bridge_flows WHERE block > ?", "DELETE FROM tokens WHERE first_block > ?",
		"DELETE FROM dex_events WHERE block > ?", "DELETE FROM dex_pairs WHERE block > ?",
	} {
		if _, err := tx.Exec(stmt, number); err != nil {
			return err
		}
	}
	for _, p := range ix.plugins {
		for _, stmt := range p.Deletes() {
			if _, err := tx.Exec(stmt, number); err != nil {
				return fmt.Errorf("rewinding the %s index: %w", p.Name(), err)
			}
		}
	}
	// Reserves and order statuses are state, not rows: put them back to what
	// the surviving events say.
	for _, stmt := range []string{
		`UPDATE dex_pairs SET
			reserve0 = COALESCE((SELECT e.reserve0 FROM dex_events e WHERE e.pair = dex_pairs.pair ORDER BY e.block DESC, e.idx DESC LIMIT 1), '0'),
			reserve1 = COALESCE((SELECT e.reserve1 FROM dex_events e WHERE e.pair = dex_pairs.pair ORDER BY e.block DESC, e.idx DESC LIMIT 1), '0'),
			reserve_block = COALESCE((SELECT e.block FROM dex_events e WHERE e.pair = dex_pairs.pair ORDER BY e.block DESC, e.idx DESC LIMIT 1), 0)
			WHERE reserve_block > ?`,
	} {
		if _, err := tx.Exec(stmt, number); err != nil {
			return err
		}
	}
	for _, p := range ix.plugins {
		for _, stmt := range p.Restores() {
			if _, err := tx.Exec(stmt, number); err != nil {
				return fmt.Errorf("rewinding the %s index: %w", p.Name(), err)
			}
		}
	}
	// The precomputed tables are the one thing a rewind cannot simply delete
	// rows from: a candle mixes blocks, and a collection's totals are a sum.
	// Both are derived, so they are computed again from what survived.
	if err := ix.rebuildCandles(tx, number+1); err != nil {
		return err
	}
	if err := rebuildAddrStats(tx); err != nil {
		return err
	}
	// Only when the deleted range could hold the current record: an older
	// max stays true no matter what happened after it, and a full rescan on
	// every rewind would make a common, cheap operation an expensive one.
	var maxTPSBlock int64
	if err := tx.QueryRow("SELECT max_tps_block FROM chain_stats WHERE id = 1").Scan(&maxTPSBlock); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if uint64(maxTPSBlock) > number {
		if err := rebuildMaxTPS(tx); err != nil {
			return err
		}
	}
	for _, p := range ix.plugins {
		if err := p.Rebuild(tx, number); err != nil {
			return fmt.Errorf("rebuilding the %s index: %w", p.Name(), err)
		}
	}
	for _, fn := range ix.beforeRewindCommit {
		if err := fn(tx, number); err != nil {
			return fmt.Errorf("rewinding extension state: %w", err)
		}
	}
	if err := commit(); err != nil {
		return err
	}
	ix.forgetPairs()
	for _, fn := range ix.afterRewind {
		fn(number)
	}
	return nil
}

func (ix *Indexer) IndexBlock(ctx context.Context, number uint64) error {
	blk, err := ix.eth.BlockByNumber(ctx, new(big.Int).SetUint64(number))
	if err != nil {
		return err
	}
	// The block has to extend what the index holds below it. Block 23,790 on
	// chain 69 is why: NoM replaced its momentum, the deriver rewound, and in
	// the few milliseconds before the deriver moved its own head this loop
	// re-read the orphan, which op-reth still served by number (it does not
	// unwind on a forkchoice to an ancestor). The replacement landed at a
	// height already indexed and was never read; the orphan's transaction
	// stayed in the explorer, a hash no node would return.
	if number > 1 {
		var stored string
		err := ix.db.QueryRow("SELECT hash FROM blocks WHERE number = ?", number-1).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && stored != blk.ParentHash().Hex() {
			return &parentMismatch{number: number, parent: blk.ParentHash(), stored: stored}
		}
	}
	// By hash, not number: in a reorg the two calls could otherwise answer for
	// different blocks at the same height.
	receipts, err := ix.eth.BlockReceipts(ctx, rpc.BlockNumberOrHashWithHash(blk.Hash(), false))
	if err != nil {
		return err
	}
	if len(receipts) != len(blk.Transactions()) {
		return fmt.Errorf("block %d: %d receipts for %d transactions", number, len(receipts), len(blk.Transactions()))
	}
	momentum := ix.cfg.MomentumForL2Block(number)

	dbtx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(dbtx)
	defer rollback()

	baseFee := blk.BaseFee()
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	// successTxs additionally drops what reverted: a block stuffed with
	// failing calls did not carry that many transactions anywhere, and a
	// throughput record should not be one someone could set by spamming
	// reverts.
	userTxs, successTxs := 0, 0
	for i, txn := range blk.Transactions() {
		if txn.Type() != types.DepositTxType || txn.From() != rollup.L1InfoDepositor {
			userTxs++
			if receipts[i].Status == types.ReceiptStatusSuccessful {
				successTxs++
			}
		}
	}
	if _, err := dbtx.Exec(`INSERT OR REPLACE INTO blocks
		(number, hash, parent, timestamp, momentum, txs, user_txs, gas_used, gas_limit, base_fee, size)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		number, blk.Hash().Hex(), blk.ParentHash().Hex(), blk.Time(), momentum, len(blk.Transactions()), userTxs,
		blk.GasUsed(), blk.GasLimit(), baseFee.String(), blk.Size()); err != nil {
		return err
	}
	if err := bumpMaxTPS(dbtx, number, int64(blk.Time()), successTxs); err != nil {
		return err
	}

	for i, tx := range blk.Transactions() {
		r := receipts[i]
		lane, nomHash := ix.origin(dbtx, tx.Hash())
		from := common.Address{}
		if tx.Type() == types.DepositTxType {
			from = tx.From()
		} else if f, err := types.Sender(ix.signer, tx); err == nil {
			from = f
		}
		var to *string
		if tx.To() != nil {
			s := tx.To().Hex()
			to = &s
		}
		var mint *string
		if tx.Type() == types.DepositTxType && tx.Mint() != nil {
			s := tx.Mint().String()
			mint = &s
		}
		var selector *string
		if len(tx.Data()) >= 4 && tx.To() != nil {
			s := "0x" + common.Bytes2Hex(tx.Data()[:4])
			selector = &s
		}
		var contract *string
		if r.ContractAddress != (common.Address{}) {
			s := r.ContractAddress.Hex()
			contract = &s
			code, err := ix.eth.CodeAt(ctx, r.ContractAddress, blk.Number())
			if err == nil {
				if _, err := dbtx.Exec(`INSERT OR REPLACE INTO contracts (address, creator, tx, block, code_size) VALUES (?,?,?,?,?)`,
					s, from.Hex(), tx.Hash().Hex(), number, len(code)); err != nil {
					return err
				}
			}
		}
		gasPrice := tx.EffectiveGasTipValue(baseFee)
		if tx.Type() != types.DepositTxType {
			gasPrice = new(big.Int).Add(gasPrice, baseFee)
		} else {
			gasPrice = new(big.Int)
		}
		fee := new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(r.GasUsed))
		if _, err := dbtx.Exec(`INSERT OR REPLACE INTO txs
			(hash, block, idx, timestamp, type, lane, nom_hash, from_addr, to_addr, value, mint, gas, gas_used, gas_price, fee, nonce, status, input_size, selector, contract)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			tx.Hash().Hex(), number, i, blk.Time(), tx.Type(), lane, nomHash, from.Hex(), to, tx.Value().String(), mint,
			tx.Gas(), r.GasUsed, gasPrice.String(), fee.String(), tx.Nonce(), r.Status, len(tx.Data()), selector, contract); err != nil {
			return err
		}
		parties := map[string]bool{from.Hex(): true}
		if to != nil {
			parties[*to] = true
		}
		if contract != nil {
			parties[*contract] = true
		}
		for addr := range parties {
			if err := bumpAddrStats(dbtx, addr, number); err != nil {
				return err
			}
		}
		for _, l := range r.Logs {
			if err := ix.indexLog(ctx, dbtx, blk, tx, from, l); err != nil {
				return err
			}
		}
	}
	// Whatever this block changed about a collection, recompute once here
	// rather than once per reader (D21).
	if err := ix.afterBlock(dbtx, number); err != nil {
		return err
	}
	return commit()
}

func (ix *Indexer) origin(dbtx *sql.Tx, hash common.Hash) (string, *string) {
	var lane, nom string
	err := dbtx.QueryRow("SELECT lane, nom_hash FROM tx_origin WHERE tx_hash = ?", hash.Hex()).Scan(&lane, &nom)
	if err != nil {
		return "unknown", nil
	}
	return lane, &nom
}

func topicAt(l *types.Log, i int) *string {
	if len(l.Topics) <= i {
		return nil
	}
	s := l.Topics[i].Hex()
	return &s
}

func (ix *Indexer) indexLog(ctx context.Context, dbtx *sql.Tx, blk *types.Block, tx *types.Transaction, from common.Address, l *types.Log) error {
	if _, err := dbtx.Exec(`INSERT OR REPLACE INTO logs (tx, idx, block, address, topic0, topic1, topic2, topic3, data) VALUES (?,?,?,?,?,?,?,?,?)`,
		tx.Hash().Hex(), l.Index, blk.NumberU64(), l.Address.Hex(), topicAt(l, 0), topicAt(l, 1), topicAt(l, 2), topicAt(l, 3), l.Data); err != nil {
		return err
	}
	if len(l.Topics) == 0 {
		return nil
	}
	row := rowOf(blk, tx, from, l)
	switch l.Topics[0] {
	case topicTransfer:
		switch {
		case len(l.Topics) == 3 && len(l.Data) == 32: // ERC-20
			value := new(big.Int).SetBytes(l.Data)
			return ix.recordTransfer(ctx, dbtx, blk, tx, l, int64(l.Index), "erc20",
				common.BytesToAddress(l.Topics[1][:]), common.BytesToAddress(l.Topics[2][:]), value.String(), nil)
		case len(l.Topics) == 4: // ERC-721
			id := new(big.Int).SetBytes(l.Topics[3][:]).String()
			if err := ix.recordTransfer(ctx, dbtx, blk, tx, l, int64(l.Index), "erc721",
				common.BytesToAddress(l.Topics[1][:]), common.BytesToAddress(l.Topics[2][:]), "", &id); err != nil {
				return err
			}
			return ix.offer(ctx, dbtx, row)
		}
	case topicTransferSingle:
		if len(l.Topics) == 4 && len(l.Data) == 64 {
			id := new(big.Int).SetBytes(l.Data[:32]).String()
			value := new(big.Int).SetBytes(l.Data[32:]).String()
			return ix.recordTransfer(ctx, dbtx, blk, tx, l, int64(l.Index), "erc1155",
				common.BytesToAddress(l.Topics[2][:]), common.BytesToAddress(l.Topics[3][:]), value, &id)
		}
	case topicTransferBatch:
		ids, values, ok := decodeTransferBatch(l.Data)
		if len(l.Topics) == 4 && ok {
			// One row per id, so holdings add up like a TransferSingle. The
			// rows share a log, so each takes a sub-index above any real log
			// index: order within the batch is kept, and ERC-1155 amounts
			// only add and subtract, so their place among other logs of the
			// same block does not change a balance.
			from, to := common.BytesToAddress(l.Topics[2][:]), common.BytesToAddress(l.Topics[3][:])
			for i := range ids {
				id := ids[i].String()
				if err := ix.recordTransfer(ctx, dbtx, blk, tx, l, batchIdx(l.Index, i), "erc1155", from, to, values[i].String(), &id); err != nil {
					return err
				}
			}
			return nil
		}
	case topicPairCreated, topicSync, topicSwap, topicMint, topicBurn:
		return ix.indexDexLog(ctx, dbtx, row)
	}
	if l.Address == rollup.BridgeAddr {
		return ix.indexBridgeLog(dbtx, blk, tx, l)
	}
	return ix.offer(ctx, dbtx, row)
}

// offer hands a log to every plugin: the catch-all at the end of indexLog,
// plus the ERC-721 branch, which a plugin may want even though the chain's own
// index has already consumed it.
func (ix *Indexer) offer(ctx context.Context, dbtx *sql.Tx, r LogRow) error {
	for _, p := range ix.plugins {
		if err := p.Handle(ctx, dbtx, r); err != nil {
			return fmt.Errorf("%s index: %w", p.Name(), err)
		}
	}
	return nil
}

// afterBlock lets every plugin finish whatever it batched across the block's
// logs, inside the same transaction.
func (ix *Indexer) afterBlock(dbtx *sql.Tx, number uint64) error {
	for _, p := range ix.plugins {
		if err := p.AfterBlock(dbtx, number); err != nil {
			return fmt.Errorf("%s index: %w", p.Name(), err)
		}
	}
	return nil
}

func (ix *Indexer) recordTransfer(ctx context.Context, dbtx *sql.Tx, blk *types.Block, tx *types.Transaction, l *types.Log,
	idx int64, kind string, from, to common.Address, value string, tokenID *string) error {
	if err := ix.ensureToken(ctx, dbtx, l.Address, kind, blk.NumberU64()); err != nil {
		return err
	}
	if _, err := dbtx.Exec(`INSERT OR REPLACE INTO token_transfers (tx, idx, block, timestamp, token, from_addr, to_addr, value, token_id)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		tx.Hash().Hex(), idx, blk.NumberU64(), blk.Time(), l.Address.Hex(), from.Hex(), to.Hex(), value, tokenID); err != nil {
		return err
	}
	_, err := dbtx.Exec("UPDATE tokens SET transfers = transfers + 1 WHERE address = ?", l.Address.Hex())
	return err
}

// batchIdx is the token_transfers index of entry i of a TransferBatch log.
func batchIdx(logIndex uint, i int) int64 {
	return int64(logIndex+1)<<20 | int64(i)
}

// decodeTransferBatch reads TransferBatch's data: (uint256[] ids, uint256[] values).
// A malformed or mismatched pair is refused rather than half-recorded.
func decodeTransferBatch(data []byte) (ids, values []*big.Int, ok bool) {
	array := func(head int) ([]*big.Int, bool) {
		if len(data) < head+32 {
			return nil, false
		}
		off := new(big.Int).SetBytes(data[head : head+32])
		if !off.IsUint64() || off.Uint64()+32 > uint64(len(data)) {
			return nil, false
		}
		at := off.Uint64()
		n := new(big.Int).SetBytes(data[at : at+32])
		if !n.IsUint64() || n.Uint64() > uint64(len(data))/32 || at+32+n.Uint64()*32 > uint64(len(data)) {
			return nil, false
		}
		out := make([]*big.Int, n.Uint64())
		for i := range out {
			start := at + 32 + uint64(i)*32
			out[i] = new(big.Int).SetBytes(data[start : start+32])
		}
		return out, true
	}
	ids, okIDs := array(0)
	values, okValues := array(32)
	if !okIDs || !okValues || len(ids) != len(values) {
		return nil, nil, false
	}
	return ids, values, true
}

// ensureToken records a token the first time it moves, reading its metadata
// from the contract.
func (ix *Indexer) ensureToken(ctx context.Context, dbtx *sql.Tx, addr common.Address, kind string, block uint64) error {
	var known string
	err := dbtx.QueryRow("SELECT address FROM tokens WHERE address = ?", addr.Hex()).Scan(&known)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	name := ix.callString(ctx, addr, "name()")
	symbol := ix.callString(ctx, addr, "symbol()")
	decimals := ix.callUint8(ctx, addr, "decimals()")
	_, err = dbtx.Exec(`INSERT OR REPLACE INTO tokens (address, kind, name, symbol, decimals, first_block) VALUES (?,?,?,?,?,?)`,
		addr.Hex(), kind, name, symbol, decimals, block)
	return err
}

// What the index lets a contract spend answering it. name(), tokenURI() and
// the rest are code whoever deployed the contract wrote, run inside this
// node's indexing transaction, and op-reth's own ceiling for an eth_call is
// -rpc.gascap: 300M on chain 69, seconds of CPU for one call. A token whose
// name() spent that, emitted by thousands of cheap contracts, held the index
// back as long as its author liked; a name() that returned megabytes stored
// megabytes, and served them in every token list.
const (
	metadataGas  = 1_000_000  // name(), symbol(), decimals(), factory()
	uriGas       = 20_000_000 // tokenURI(), contractURI(): fully on-chain art renders here
	maxTextBytes = 256        // a name or a symbol
	// MaxURIBytes is the longest URI kept. A data: URI carries its document,
	// and metadata past 64 KB is not read anyway (nft.resolveMeta).
	MaxURIBytes = 128 << 10
)

func (ix *Indexer) call(ctx context.Context, addr common.Address, sig string) []byte {
	out, err := ix.eth.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: crypto.Keccak256([]byte(sig))[:4], Gas: metadataGas}, nil)
	if err != nil {
		return nil
	}
	return out
}

// callString decodes an ABI string return, or a bytes32 one (older tokens),
// clipped to a length a name or a symbol has any use for.
func (ix *Indexer) callString(ctx context.Context, addr common.Address, sig string) string {
	return ClipText(DecodeABIString(ix.call(ctx, addr, sig)), maxTextBytes)
}

// ClipText cuts s to at most n bytes without splitting a character, and
// drops byte sequences that are not UTF-8: a contract's string is whatever
// bytes it returned.
func ClipText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func DecodeABIString(out []byte) string {
	switch {
	case len(out) == 32:
		return strings.TrimRight(string(out), "\x00")
	case len(out) >= 64:
		length := new(big.Int).SetBytes(out[32:64]).Uint64()
		if 64+length <= uint64(len(out)) {
			return string(out[64 : 64+length])
		}
	}
	return ""
}

func (ix *Indexer) callUint8(ctx context.Context, addr common.Address, sig string) int {
	out := ix.call(ctx, addr, sig)
	if len(out) != 32 {
		return 0
	}
	// decimals() is a uint8. Anything larger was read as an int64 and could
	// come out negative, which later scaled every amount by 10^-n.
	v := new(big.Int).SetBytes(out)
	if !v.IsUint64() || v.Uint64() > 255 {
		return 0
	}
	return int(v.Uint64())
}

func (ix *Indexer) indexBridgeLog(dbtx *sql.Tx, blk *types.Block, tx *types.Transaction, l *types.Log) error {
	insert := func(kind string, wid *uint64, account, token, amount, nomBlock, note string) error {
		_, err := dbtx.Exec(`INSERT INTO bridge_flows (kind, wid, block, timestamp, l2_tx, account, token, amount, nom_block, note)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			kind, wid, blk.NumberU64(), blk.Time(), tx.Hash().Hex(), account, token, amount, nomBlock, note)
		return err
	}
	switch l.Topics[0] {
	case topicWithdrawalInitiated:
		if len(l.Topics) != 4 || len(l.Data) < 64 {
			return nil
		}
		id := new(big.Int).SetBytes(l.Topics[1][:]).Uint64()
		return insert("withdrawal", &id, common.BytesToAddress(l.Topics[2][:]).Hex(),
			"0x"+common.Bytes2Hex(l.Data[:10]), new(big.Int).SetBytes(l.Data[32:64]).String(), "",
			common.BytesToAddress(l.Topics[3][:]).Hex())
	case topicWithdrawalPaid:
		if len(l.Topics) != 3 {
			return nil
		}
		id := new(big.Int).SetBytes(l.Topics[1][:]).Uint64()
		return insert("payout", &id, "", "", "", l.Topics[2].Hex(), "")
	case topicPayoutAnomaly:
		if len(l.Topics) != 3 || len(l.Data) < 32 {
			return nil
		}
		id := new(big.Int).SetBytes(l.Topics[1][:]).Uint64()
		reason := new(big.Int).SetBytes(l.Data[:32]).Uint64()
		return insert("anomaly", &id, "", "", "", l.Topics[2].Hex(), fmt.Sprintf("reason %d", reason))
	case topicQSRDeposited:
		if len(l.Topics) != 3 || len(l.Data) < 32 {
			return nil
		}
		return insert("deposit", nil, common.BytesToAddress(l.Topics[2][:]).Hex(), "QSR",
			new(big.Int).SetBytes(l.Data[:32]).String(), l.Topics[1].Hex(), "")
	}
	return nil
}
