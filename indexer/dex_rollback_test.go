package indexer

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// These are unsigned synthetic receipts over the existing read-only loopback
// port. The real DEX parser, candle aggregation and SQLite transaction run.
type n04DexFixture struct {
	chain                         *fakeChain
	factory, token0, token1, pair common.Address
}

func n04Dex() n04DexFixture {
	f := n04DexFixture{newFakeChain(), common.Address{19: 11}, common.Address{19: 12}, common.Address{19: 13}, common.Address{19: 14}}
	f.chain.answer(f.pair, "factory()", common.LeftPadBytes(f.factory[:], 32))
	for _, token := range []common.Address{f.token0, f.token1} {
		f.chain.answer(token, "decimals()", common.LeftPadBytes([]byte{18}, 32))
	}
	return f
}

func n04DexWords(values ...int64) []byte {
	var out []byte
	for _, value := range values {
		out = append(out, common.LeftPadBytes(big.NewInt(value).Bytes(), 32)...)
	}
	return out
}

func (f n04DexFixture) sync(price int64) *types.Log {
	return &types.Log{Address: f.pair, Topics: []common.Hash{topicSync}, Data: n04DexWords(100, 100*price)}
}

func (f n04DexFixture) block(number uint64, price int64, create bool) {
	var logs []*types.Log
	if create {
		logs = append(logs, pairCreated(f.factory, f.token0, f.token1, f.pair))
	}
	logs = append(logs, f.sync(price), &types.Log{
		Address: f.pair, Topics: []common.Hash{topicSwap, {31: 1}, {31: 2}}, Data: n04DexWords(0, 1, 1, 0),
	})
	f.chain.putWithLogs(number, 0, logs)
}

func n04DexIndex(t *testing.T, ix *Indexer, from, through uint64) {
	t.Helper()
	for n := from; n <= through; n++ {
		if err := ix.IndexBlock(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
}

func n04DexOpen(t *testing.T, ix *Indexer, pair common.Address, bucket int64, want float64) {
	t.Helper()
	var open float64
	if err := ix.db.QueryRow("SELECT open FROM dex_candles WHERE pair=? AND interval=60 AND bucket=?", pair.Hex(), bucket).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if math.Abs(open-want) > 1e-12 {
		t.Errorf("candle open=%v, want last committed price %v", open, want)
	}
}

func TestDexTransactionRetryUsesCommittedPrice(t *testing.T) {
	for _, phase := range []string{"after-block", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := n04Dex()
			f.block(1, 2, true)
			f.block(2, 5, false)
			plugin := &n04Plugin{}
			ix, db, _ := n04Index(t, f.chain, plugin)
			n04DexIndex(t, ix, 1, 1)
			prefix := n04IndexState(t, db)
			plugin.fail = phase
			if err := ix.IndexBlock(context.Background(), 2); err == nil {
				t.Fatal("declared transaction refusal was not reached")
			}
			n04IndexSame(t, n04IndexState(t, db), prefix)
			plugin.fail = ""
			n04DexIndex(t, ix, 2, 2)
			reference, referenceDB, _ := n04Index(t, f.chain, &n04Plugin{})
			n04DexIndex(t, reference, 1, 2)
			n04DexOpen(t, ix, f.pair, 1020, 2)
			n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
		})
	}
}

func TestDexFailedRewindKeepsCommittedPrice(t *testing.T) {
	for _, phase := range []string{"rebuild", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := n04Dex()
			f.block(1, 2, true)
			f.block(2, 5, false)
			for n := uint64(3); n < 8; n++ {
				f.chain.put(n, 0)
			}
			f.block(8, 7, false)
			plugin := &n04Plugin{}
			ix, db, _ := n04Index(t, f.chain, plugin)
			n04DexIndex(t, ix, 1, 2)
			prefix := n04IndexState(t, db)
			plugin.fail = phase
			if err := ix.Rewind(1); err == nil {
				t.Fatal("declared rewind refusal was not reached")
			}
			n04IndexSame(t, n04IndexState(t, db), prefix)
			plugin.fail = ""
			n04DexIndex(t, ix, 3, 8)
			reference, referenceDB, _ := n04Index(t, f.chain, &n04Plugin{})
			n04DexIndex(t, reference, 1, 8)
			n04DexOpen(t, ix, f.pair, 1080, 5)
			n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
		})
	}
}

func TestDexRolledBackPairIsNotRecognizedOnAnotherBranch(t *testing.T) {
	for _, phase := range []string{"after-block", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := n04Dex()
			f.chain.put(1, 0)
			f.block(2, 2, true)
			plugin := &n04Plugin{}
			ix, db, _ := n04Index(t, f.chain, plugin)
			n04DexIndex(t, ix, 1, 1)
			prefix := n04IndexState(t, db)
			plugin.fail = phase
			if err := ix.IndexBlock(context.Background(), 2); err == nil {
				t.Fatal("declared transaction refusal was not reached")
			}
			n04IndexSame(t, n04IndexState(t, db), prefix)
			// The replacement branch never created this pair. Its Sync must
			// remain an unknown-pool log, including on a warm indexer.
			f.chain.putWithLogs(2, 1, []*types.Log{f.sync(3)})
			plugin.fail = ""
			n04DexIndex(t, ix, 2, 2)
			reference, referenceDB, _ := n04Index(t, f.chain, &n04Plugin{})
			n04DexIndex(t, reference, 1, 2)
			n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
		})
	}
}

func TestDexCommitKeepsReusableCaches(t *testing.T) {
	f := n04Dex()
	f.block(1, 2, true)
	f.block(2, 5, false)
	ix, _, _ := n04Index(t, f.chain, &n04Plugin{})
	n04DexIndex(t, ix, 1, 2)
	if price, ok := ix.candles.get(f.pair.Hex()); !ok || math.Abs(price-5) > 1e-12 {
		t.Fatalf("committed last price was not retained: %v, %v", price, ok)
	}
	ix.mu.Lock()
	pair := ix.pairs[f.pair]
	ix.mu.Unlock()
	if pair == nil || pair.token0 != f.token0 || pair.token1 != f.token1 {
		t.Fatal("committed pair metadata was not retained")
	}
}
