package indexer

import (
	"context"
	"database/sql"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Uniswap V2 pools and their trades, from the pair contracts' own events. Any
// factory's PairCreated is recorded; the price layer decides which factories it
// trusts to price a token.
//
// Each Swap, Mint and Burn is stored with the pool's reserves after it, which
// the pair announces in the Sync it emits immediately before. The price a chart
// draws is that reserve ratio — the pool's price after the trade, as Uniswap's
// own analytics chart it — and the amounts are what the trade moved.

var (
	topicPairCreated = crypto.Keccak256Hash([]byte("PairCreated(address,address,address,uint256)"))
	topicSync        = crypto.Keccak256Hash([]byte("Sync(uint112,uint112)"))
	topicSwap        = crypto.Keccak256Hash([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
	topicMint        = crypto.Keccak256Hash([]byte("Mint(address,uint256,uint256)"))
	topicBurn        = crypto.Keccak256Hash([]byte("Burn(address,uint256,uint256,address)"))

	dexTopics = []common.Hash{topicPairCreated, topicSync, topicSwap, topicMint, topicBurn}
)

const dexSchema = `
CREATE TABLE IF NOT EXISTS dex_pairs (
	pair          TEXT PRIMARY KEY,
	factory       TEXT    NOT NULL,
	token0        TEXT    NOT NULL,
	token1        TEXT    NOT NULL,
	block         INTEGER NOT NULL,
	timestamp     INTEGER NOT NULL,
	reserve0      TEXT    NOT NULL DEFAULT '0',
	reserve1      TEXT    NOT NULL DEFAULT '0',
	reserve_block INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS dex_pairs_token0 ON dex_pairs(token0);
CREATE INDEX IF NOT EXISTS dex_pairs_token1 ON dex_pairs(token1);
CREATE TABLE IF NOT EXISTS dex_events (
	pair      TEXT    NOT NULL,
	block     INTEGER NOT NULL,
	idx       INTEGER NOT NULL,
	timestamp INTEGER NOT NULL,
	tx        TEXT    NOT NULL,
	kind      TEXT    NOT NULL,
	account   TEXT    NOT NULL,
	amount0   TEXT    NOT NULL,
	amount1   TEXT    NOT NULL,
	reserve0  TEXT    NOT NULL,
	reserve1  TEXT    NOT NULL,
	price     REAL,
	PRIMARY KEY (pair, block, idx)
);
CREATE INDEX IF NOT EXISTS dex_events_time ON dex_events(pair, timestamp);
CREATE INDEX IF NOT EXISTS dex_events_block ON dex_events(block);
`

// Event kinds. A buy takes token0 out of the pool, a sell puts it in.
const (
	KindBuy    = "buy"
	KindSell   = "sell"
	KindAdd    = "add"
	KindRemove = "remove"
	KindSync   = "sync"
)

type pairInfo struct {
	token0, token1 common.Address
	dec0, dec1     int
}

// pairFor returns the pool at addr, or nil if addr is not a known pair. Both
// answers are cached; PairCreated and a rewind clear them.
func (ix *Indexer) pairFor(dbtx *sql.Tx, addr common.Address) (*pairInfo, error) {
	ix.mu.Lock()
	p, ok := ix.pairs[addr]
	ix.mu.Unlock()
	if ok {
		return p, nil
	}
	var t0, t1 string
	var d0, d1 int
	err := dbtx.QueryRow(`SELECT p.token0, p.token1, COALESCE(a.decimals, 18), COALESCE(b.decimals, 18)
		FROM dex_pairs p LEFT JOIN tokens a ON a.address = p.token0 LEFT JOIN tokens b ON b.address = p.token1
		WHERE p.pair = ?`, addr.Hex()).Scan(&t0, &t1, &d0, &d1)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		p = nil
	case err != nil:
		return nil, err
	default:
		p = &pairInfo{token0: common.HexToAddress(t0), token1: common.HexToAddress(t1), dec0: d0, dec1: d1}
	}
	ix.mu.Lock()
	ix.pairs[addr] = p
	ix.mu.Unlock()
	return p, nil
}

func (ix *Indexer) forgetPairs() {
	ix.mu.Lock()
	ix.pairs = map[common.Address]*pairInfo{}
	ix.mu.Unlock()
}

func (ix *Indexer) indexDexLog(ctx context.Context, dbtx *sql.Tx, r LogRow) error {
	if len(r.Topics) == 0 {
		return nil
	}
	if r.Topics[0] == topicPairCreated {
		if len(r.Topics) != 3 || len(r.Data) < 32 {
			return nil
		}
		token0, token1, pair := r.Addr(1), r.Addr(2), r.WordAddr(0)
		// Any contract can emit PairCreated, and a pair's address is known
		// before it exists (CREATE2 from the factory and the two tokens). The
		// row is first-claim-wins, so an announcement from anything but the
		// pair's own factory could take a pool before it was created, naming
		// its own factory and tokens: the real pool then never priced, and its
		// trades were read against the wrong tokens. A Uniswap V2 pair keeps
		// its deployer in factory(); only that factory's word counts.
		if ix.pairFactory(ctx, pair) != r.Address {
			return nil
		}
		for _, t := range []common.Address{token0, token1} {
			if err := ix.ensureToken(ctx, dbtx, t, "erc20", r.Block); err != nil {
				return err
			}
		}
		ix.mu.Lock()
		delete(ix.pairs, pair)
		ix.mu.Unlock()
		_, err := dbtx.Exec(`INSERT OR IGNORE INTO dex_pairs (pair, factory, token0, token1, block, timestamp) VALUES (?,?,?,?,?,?)`,
			pair.Hex(), r.Address.Hex(), token0.Hex(), token1.Hex(), r.Block, r.Time)
		return err
	}

	p, err := ix.pairFor(dbtx, r.Address)
	if err != nil || p == nil {
		return err
	}
	switch r.Topics[0] {
	case topicSync:
		if len(r.Topics) != 1 || len(r.Data) != 64 {
			return nil
		}
		r0, r1 := r.Word(0), r.Word(1)
		if _, err := dbtx.Exec(`UPDATE dex_pairs SET reserve0 = ?, reserve1 = ?, reserve_block = ? WHERE pair = ?`,
			r0.String(), r1.String(), r.Block, r.Address.Hex()); err != nil {
			return err
		}
		zero := new(big.Int)
		return ix.putDexEvent(dbtx, r, p, KindSync, zero, zero, r0, r1)
	case topicSwap:
		if len(r.Topics) != 3 || len(r.Data) != 128 {
			return nil
		}
		in0, in1, out0, out1 := r.Word(0), r.Word(1), r.Word(2), r.Word(3)
		kind := KindSell
		if out0.Cmp(in0) > 0 {
			kind = KindBuy
		}
		return ix.putTrade(dbtx, r, p, kind, absDiff(in0, out0), absDiff(in1, out1))
	case topicMint:
		if len(r.Topics) != 2 || len(r.Data) != 64 {
			return nil
		}
		return ix.putTrade(dbtx, r, p, KindAdd, r.Word(0), r.Word(1))
	case topicBurn:
		if len(r.Topics) != 3 || len(r.Data) != 64 {
			return nil
		}
		return ix.putTrade(dbtx, r, p, KindRemove, r.Word(0), r.Word(1))
	}
	return nil
}

// putTrade records a swap, mint or burn at the reserves the Sync just before it
// set, and removes that Sync's own row: the two are one event.
func (ix *Indexer) putTrade(dbtx *sql.Tx, r LogRow, p *pairInfo, kind string, a0, a1 *big.Int) error {
	var s0, s1 string
	if err := dbtx.QueryRow(`SELECT reserve0, reserve1 FROM dex_pairs WHERE pair = ?`, r.Address.Hex()).Scan(&s0, &s1); err != nil {
		return err
	}
	if r.Index > 0 {
		if _, err := dbtx.Exec(`DELETE FROM dex_events WHERE pair = ? AND block = ? AND idx = ? AND kind = ?`,
			r.Address.Hex(), r.Block, r.Index-1, KindSync); err != nil {
			return err
		}
	}
	r0, _ := new(big.Int).SetString(s0, 10)
	r1, _ := new(big.Int).SetString(s1, 10)
	return ix.putDexEvent(dbtx, r, p, kind, a0, a1, r0, r1)
}

func (ix *Indexer) putDexEvent(dbtx *sql.Tx, r LogRow, p *pairInfo, kind string, a0, a1, r0, r1 *big.Int) error {
	var price any
	if r0 != nil && r1 != nil && r0.Sign() > 0 && r1.Sign() > 0 {
		price = FloatUnits(r1, p.dec1) / FloatUnits(r0, p.dec0)
	}
	if _, err := dbtx.Exec(`INSERT OR REPLACE INTO dex_events
		(pair, block, idx, timestamp, tx, kind, account, amount0, amount1, reserve0, reserve1, price)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Address.Hex(), r.Block, r.Index, r.Time, r.Tx, kind, r.From.Hex(), a0.String(), a1.String(), r0.String(), r1.String(), price); err != nil {
		return err
	}
	// Fold it into the candles now, so a chart is a read rather than a scan.
	f, ok := price.(float64)
	if !ok {
		return nil
	}
	return ix.foldCandles(dbtx, r.Address.Hex(), int64(r.Time), r.Block, f,
		FloatUnits(a0, p.dec0), FloatUnits(a1, p.dec1), kind == KindBuy || kind == KindSell)
}

// pairFactory is what a pair says deployed it, or the zero address when it
// says nothing: no code yet, or not a Uniswap V2 pair.
func (ix *Indexer) pairFactory(ctx context.Context, pair common.Address) common.Address {
	out := ix.call(ctx, pair, "factory()")
	if len(out) != 32 {
		return common.Address{}
	}
	return common.BytesToAddress(out)
}

func absDiff(a, b *big.Int) *big.Int {
	d := new(big.Int).Sub(a, b)
	return d.Abs(d)
}
