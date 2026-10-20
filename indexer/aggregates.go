package indexer

import (
	"database/sql"
	"errors"
	"math"
	"math/big"
	"sync"
)

// Precomputed aggregates (DECISIONS.md D21, option 1).
//
// Four explorer endpoints slowed 5–15× as the chain grew, and every one of
// them for the same reason: it aggregated its answer from scratch on each
// request. Candles were built by scanning a pool's whole event window; a
// collection's item count, owner count, sales and volume were six correlated
// subqueries over every token and sale it had; an address's transaction count
// was a COUNT(*) over a row per address per transaction.
//
// So the aggregates are kept as the index is written instead:
//
//   - **dex_candles**, one row per pool, interval and bucket, folded in as
//     each event is recorded. Stored in the pool's own orientation — token1 per
//     token0 — because which side is "the price" is a question the reader asks,
//     not a property of the trade.
//   - **nft_collection_stats**, recomputed once per block for the collections
//     that block touched, rather than once per request for all of them. A
//     collection nothing happened to costs nothing.
//   - **addr_stats**, a running count per address, which replaces the
//     `addr_txs` table entirely: that table stored a row per address per
//     transaction (373 B of the 1,121 B each transfer cost the index) to answer
//     two questions that an index on `txs` and one counter answer better.
//
// All of it is derived: `zvm node` can drop the index and rebuild it from
// op-geth and the derivation store, which is what makes this safe to change.

// CandleIntervals are the bucket sizes the explorer offers, in seconds.
var CandleIntervals = []int64{60, 300, 900, 3600, 14400, 86400}

// CandleRetention is how many buckets of each interval are kept. The API's
// largest window is 1,000 buckets, so this leaves room to spare; older buckets
// are dropped rather than kept forever, and `dex_events` still has the history.
const CandleRetention = 2_000

const aggregateSchema = `
CREATE TABLE IF NOT EXISTS dex_candles (
	pair       TEXT    NOT NULL,
	interval   INTEGER NOT NULL,
	bucket     INTEGER NOT NULL,
	open       REAL    NOT NULL,
	high       REAL    NOT NULL,
	low        REAL    NOT NULL,
	close      REAL    NOT NULL,
	vol0       REAL    NOT NULL,
	vol1       REAL    NOT NULL,
	trades     INTEGER NOT NULL,
	last_block INTEGER NOT NULL,
	PRIMARY KEY (pair, interval, bucket)
);
CREATE INDEX IF NOT EXISTS dex_candles_recent ON dex_candles(pair, interval, bucket DESC);
CREATE INDEX IF NOT EXISTS dex_candles_block ON dex_candles(last_block);

CREATE TABLE IF NOT EXISTS nft_collection_stats (
	collection    TEXT PRIMARY KEY,
	items         INTEGER NOT NULL,
	owners        INTEGER NOT NULL,
	sales         INTEGER NOT NULL,
	volume        REAL    NOT NULL,
	updated_block INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS addr_stats (
	addr        TEXT PRIMARY KEY,
	txs         INTEGER NOT NULL,
	first_block INTEGER NOT NULL,
	last_block  INTEGER NOT NULL
);

-- The single highest transactions-per-second any one block has carried,
-- across the chain's whole history: not a window a reader picks, so it is
-- kept as one row rather than recomputed from however much of the chain the
-- caller happened to ask for.
-- basis names what max_tps_txs counts, so a later change to that definition
-- (as happened once already: it started as every submitted transaction and
-- became only the ones that did not revert) can tell its own old rows from
-- ones already computed the new way, and correct just those.
CREATE TABLE IF NOT EXISTS chain_stats (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	max_tps       REAL    NOT NULL DEFAULT 0,
	max_tps_block INTEGER NOT NULL DEFAULT 0,
	max_tps_txs   INTEGER NOT NULL DEFAULT 0,
	max_tps_at    INTEGER NOT NULL DEFAULT 0,
	basis         TEXT    NOT NULL DEFAULT ''
);

-- What addr_txs was for, at a fraction of its size.
CREATE INDEX IF NOT EXISTS txs_from ON txs(from_addr, block DESC, idx DESC);
CREATE INDEX IF NOT EXISTS txs_to ON txs(to_addr, block DESC, idx DESC);
CREATE INDEX IF NOT EXISTS txs_contract ON txs(contract);
`

// candleCache remembers each pool's last price, so opening a new bucket costs
// no query. The indexer is the only writer, so a plain map behind its own lock
// is enough.
type candleCache struct {
	mu   sync.Mutex
	last map[string]float64
}

func newCandleCache() *candleCache { return &candleCache{last: map[string]float64{}} }

func (c *candleCache) get(pair string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.last[pair]
	return v, ok
}

func (c *candleCache) set(pair string, price float64) {
	c.mu.Lock()
	c.last[pair] = price
	c.mu.Unlock()
}

func (c *candleCache) reset() {
	c.mu.Lock()
	c.last = map[string]float64{}
	c.mu.Unlock()
}

// foldCandles folds one pool event into every interval's bucket. price is the
// pool's own price (token1 per token0); vol0 and vol1 are the amounts traded,
// in whole tokens, and are counted only for a buy or a sell — adding and
// removing liquidity is not volume.
func (ix *Indexer) foldCandles(dbtx *sql.Tx, pair string, ts int64, block uint64, price, vol0, vol1 float64, trade bool) error {
	if price <= 0 || math.IsInf(price, 0) || math.IsNaN(price) {
		return nil
	}
	open, ok := ix.candles.get(pair)
	if !ok {
		open = ix.lastCandleClose(dbtx, pair, price)
	}
	if !trade {
		vol0, vol1 = 0, 0
	}
	n := 0
	if trade {
		n = 1
	}
	for _, interval := range CandleIntervals {
		bucket := ts / interval * interval
		if _, err := dbtx.Exec(`INSERT INTO dex_candles
			(pair, interval, bucket, open, high, low, close, vol0, vol1, trades, last_block)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(pair, interval, bucket) DO UPDATE SET
				high = MAX(dex_candles.high, excluded.close),
				low = MIN(dex_candles.low, excluded.close),
				close = excluded.close,
				vol0 = dex_candles.vol0 + excluded.vol0,
				vol1 = dex_candles.vol1 + excluded.vol1,
				trades = dex_candles.trades + excluded.trades,
				last_block = excluded.last_block`,
			pair, interval, bucket, open, math.Max(open, price), math.Min(open, price), price,
			vol0, vol1, n, block); err != nil {
			return err
		}
	}
	ix.candles.set(pair, price)
	return nil
}

// lastCandleClose is the price a new bucket opens at: the close of the newest
// bucket this pool has, or the event's own price when it has none.
func (ix *Indexer) lastCandleClose(dbtx *sql.Tx, pair string, fallback float64) float64 {
	var v sql.NullFloat64
	err := dbtx.QueryRow(`SELECT close FROM dex_candles WHERE pair = ? AND interval = ?
		ORDER BY bucket DESC LIMIT 1`, pair, CandleIntervals[0]).Scan(&v)
	if err != nil || !v.Valid || v.Float64 <= 0 {
		return fallback
	}
	return v.Float64
}

// pruneCandles drops buckets past the retention window, oldest first.
func (ix *Indexer) pruneCandles(dbtx *sql.Tx, pair string) error {
	for _, interval := range CandleIntervals {
		if _, err := dbtx.Exec(`DELETE FROM dex_candles WHERE pair = ? AND interval = ? AND bucket < (
			SELECT MIN(bucket) FROM (SELECT bucket FROM dex_candles WHERE pair = ? AND interval = ?
				ORDER BY bucket DESC LIMIT ?))`,
			pair, interval, pair, interval, CandleRetention); err != nil {
			return err
		}
	}
	return nil
}

// rebuildCandles recomputes every bucket an event at or after fromBlock
// touched. It is what a rewind needs — the rows above the rewind point are
// gone, and the buckets that held them have to be folded again from what is
// left — and what a first run after this table was added needs, with
// fromBlock 0.
func (ix *Indexer) rebuildCandles(dbtx *sql.Tx, fromBlock uint64) error {
	ix.candles.reset()
	if _, err := dbtx.Exec(`DELETE FROM dex_candles WHERE last_block >= ?`, fromBlock); err != nil {
		return err
	}
	// The buckets that survive keep their contents; anything from the oldest
	// deleted bucket onwards is folded again in event order.
	var from sql.NullInt64
	if err := dbtx.QueryRow(`SELECT MIN(bucket) FROM dex_candles`).Scan(&from); err != nil {
		return err
	}
	rows, err := dbtx.Query(`SELECT pair, timestamp, block, kind, price, amount0, amount1 FROM dex_events
		WHERE price IS NOT NULL AND block >= ? ORDER BY block, idx`, fromBlock)
	if err != nil {
		return err
	}
	type ev struct {
		pair   string
		ts     int64
		block  uint64
		kind   string
		price  float64
		a0, a1 string
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.pair, &e.ts, &e.block, &e.kind, &e.price, &e.a0, &e.a1); err != nil {
			rows.Close()
			return err
		}
		evs = append(evs, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range evs {
		p := ix.pairDecimals(dbtx, e.pair)
		v0, v1 := floatString(e.a0, p[0]), floatString(e.a1, p[1])
		if err := ix.foldCandles(dbtx, e.pair, e.ts, e.block, e.price, v0, v1, e.kind == KindBuy || e.kind == KindSell); err != nil {
			return err
		}
	}
	return nil
}

// pairDecimals is a pool's token decimals, for turning base units into whole
// tokens when a candle's volume is folded.
func (ix *Indexer) pairDecimals(dbtx *sql.Tx, pair string) [2]int {
	var d0, d1 sql.NullInt64
	dbtx.QueryRow(`SELECT (SELECT decimals FROM tokens WHERE address = p.token0),
		(SELECT decimals FROM tokens WHERE address = p.token1) FROM dex_pairs p WHERE p.pair = ?`, pair).Scan(&d0, &d1)
	out := [2]int{18, 18}
	if d0.Valid {
		out[0] = int(d0.Int64)
	}
	if d1.Valid {
		out[1] = int(d1.Int64)
	}
	return out
}

// markCollection remembers that a block changed something about a collection,
// so its statistics are recomputed once when the block is done rather than
// once per reader.
// refreshCollectionStats recomputes one collection's totals. This is the work
// `/api/nft/collections` used to do for every collection on every request.
// refreshDirtyCollections recomputes whatever this block touched.
// rebuildCollectionStats recomputes every collection, for a first run after
// this table was added and after a rewind.
// bumpAddrStats records that an address took part in a transaction. It
// replaces the addr_txs row that used to be written for the same fact.
func bumpAddrStats(dbtx *sql.Tx, addr string, block uint64) error {
	_, err := dbtx.Exec(`INSERT INTO addr_stats (addr, txs, first_block, last_block) VALUES (?,1,?,?)
		ON CONFLICT(addr) DO UPDATE SET txs = addr_stats.txs + 1,
			last_block = MAX(addr_stats.last_block, excluded.last_block)`, addr, block, block)
	return err
}

// bumpMaxTPS folds one new block into chain_stats. successTxs is what
// actually landed — a reverted call burned gas but moved nothing, so it does
// not count toward a throughput record any more than it counts as a transfer
// anywhere else in the explorer. A block derives from exactly one momentum
// (D2), so the gap to the block before it is that momentum's own duration.
// The first block ever indexed has no predecessor and is skipped.
func bumpMaxTPS(dbtx *sql.Tx, number uint64, timestamp int64, successTxs int) error {
	if number == 0 {
		return nil
	}
	var prevTimestamp int64
	err := dbtx.QueryRow("SELECT timestamp FROM blocks WHERE number = ?", number-1).Scan(&prevTimestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	dt := timestamp - prevTimestamp
	if dt <= 0 {
		return nil
	}
	tps := float64(successTxs) / float64(dt)
	_, err = dbtx.Exec(`INSERT INTO chain_stats (id, max_tps, max_tps_block, max_tps_txs, max_tps_at, basis) VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET max_tps = excluded.max_tps, max_tps_block = excluded.max_tps_block,
			max_tps_txs = excluded.max_tps_txs, max_tps_at = excluded.max_tps_at, basis = excluded.basis
		WHERE excluded.max_tps > chain_stats.max_tps`, tps, number, successTxs, timestamp, maxTPSBasis)
	return err
}

// maxTPSBasis marks a chain_stats row as computed the current way. Bump it
// whenever what max_tps_txs counts changes, so backfillAggregates can tell an
// old row from a corrected one and fix just that.
const maxTPSBasis = "success"

// maxTPSExcluded is the one transaction every block carries that is never a
// user's: OP Stack's L1Block deposit, from the fixed depositor account. It is
// excluded from user_txs at index time (indexer.go) and from this rebuild the
// same way, by type and sender rather than by lane, since that is the actual
// rule user_txs applies.
const (
	depositTxType           = 126 // types.DepositTxType
	l1InfoDepositor         = "0xDeaDDEaDDeAdDeAdDEAdDEaddeAddEAdDEAd0001"
	receiptStatusSuccessful = 1 // types.ReceiptStatusSuccessful
)

// rebuildMaxTPS recomputes chain_stats.max_tps from every block that
// survives — used on a first run and after a rewind deletes the block that
// set the current record. Each block's count comes from `txs` itself, status
// and all, rather than the `blocks.user_txs` column, which counts every
// submitted transaction whether or not it reverted.
func rebuildMaxTPS(dbtx *sql.Tx) error {
	rows, err := dbtx.Query(`SELECT b.number, b.timestamp, COUNT(t.hash)
		FROM blocks b LEFT JOIN txs t ON t.block = b.number AND t.status = ?
			AND NOT (t.type = ? AND t.from_addr = ?)
		GROUP BY b.number ORDER BY b.number`, receiptStatusSuccessful, depositTxType, l1InfoDepositor)
	if err != nil {
		return err
	}
	defer rows.Close()
	var best struct {
		tps         float64
		number, txs int64
		at          int64
	}
	var prevNumber uint64
	var prevTimestamp int64
	first := true
	for rows.Next() {
		var number uint64
		var timestamp, successTxs int64
		if err := rows.Scan(&number, &timestamp, &successTxs); err != nil {
			return err
		}
		if !first && number == prevNumber+1 {
			if dt := timestamp - prevTimestamp; dt > 0 {
				if tps := float64(successTxs) / float64(dt); tps > best.tps {
					best.tps, best.number, best.txs, best.at = tps, int64(number), successTxs, timestamp
				}
			}
		}
		prevNumber, prevTimestamp, first = number, timestamp, false
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = dbtx.Exec(`INSERT INTO chain_stats (id, max_tps, max_tps_block, max_tps_txs, max_tps_at, basis) VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET max_tps = excluded.max_tps, max_tps_block = excluded.max_tps_block,
			max_tps_txs = excluded.max_tps_txs, max_tps_at = excluded.max_tps_at, basis = excluded.basis`,
		best.tps, best.number, best.txs, best.at, maxTPSBasis)
	return err
}

// rebuildAddrStats recomputes the counters from `txs`, which is where the
// facts are. Used on a rewind and on a first run.
func rebuildAddrStats(dbtx *sql.Tx) error {
	if _, err := dbtx.Exec(`DELETE FROM addr_stats`); err != nil {
		return err
	}
	_, err := dbtx.Exec(`INSERT INTO addr_stats (addr, txs, first_block, last_block)
		SELECT addr, COUNT(*), MIN(block), MAX(block) FROM (
			SELECT from_addr AS addr, block FROM txs
			UNION ALL SELECT to_addr, block FROM txs WHERE to_addr IS NOT NULL
			UNION ALL SELECT contract, block FROM txs WHERE contract IS NOT NULL
				AND (to_addr IS NULL OR contract != to_addr)
		) GROUP BY addr`)
	return err
}

// zeroAddress is the burn address a token leaves the supply through, and the
// currency marker for native ZNN in the marketplace tables.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// floatString turns a base-unit amount into whole tokens.
func floatString(s string, decimals int) float64 {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return 0
	}
	return FloatUnits(v, decimals)
}
