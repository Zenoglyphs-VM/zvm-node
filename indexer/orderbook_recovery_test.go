package indexer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/nodecmd"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// This is a declared stored payload. Its opaque signature is never generated,
// recovered, submitted or sent to an exchange. Rewind must preserve it exactly.
func storedRecoveryOrder(t *testing.T, db *sql.DB) common.Hash {
	t.Helper()
	root := common.Hash{31: 17}
	in := orderbook.Input{Order: orderbook.Order{
		Trader: common.Address{19: 52}, Side: orderbook.SideBuy, Collection: common.Address{19: 53},
		TokenID: orderbook.U(1), Amount: orderbook.U(10), Price: orderbook.U(1000),
		ListingTime: 0, ExpirationTime: 1 << 32, Nonce: orderbook.U(4), Salt: orderbook.U(7),
	}, Kind: orderbook.KindSingle, Signature: hexutil.Bytes{1, 2, 3}, Proof: []common.Hash{{31: 41}}, Root: &root}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	hash := in.Order.Hash()
	values := []any{hash.Hex(), (common.Address{19: 51}).Hex(), in.Order.Trader.Hex(), in.Order.Side,
		in.Order.Collection.Hex(), "1", 10, 10, (common.Address{}).Hex(), "1000", 0.001,
		0, 1 << 32, "4", in.Kind, root.Hex(), string(raw), orderbook.StatusFilled, orderbook.CodeFilled,
		1, 1010, 1020, 2, "declared-local-input"}
	_, err = db.Exec(`INSERT INTO nft_orders
		(hash,exchange,trader,side,collection,token_id,amount,filled,payment_token,price,price_num,
		 listing_time,expiration_time,nonce,kind,root,input,status,code,block,timestamp,checked_at,closed_block,source)
		VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")+`)`, values...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO nft_order_meta VALUES('scanned:fixture',2)"); err != nil {
		t.Fatal(err)
	}
	return hash
}

func storedRecoveryIntent(t *testing.T, db *sql.DB, hash common.Hash) string {
	t.Helper()
	row := db.QueryRow(`SELECT hash,exchange,trader,side,collection,token_id,amount,payment_token,price,price_num,
		listing_time,expiration_time,nonce,kind,root,input,block,timestamp,source FROM nft_orders WHERE hash=?`, hash.Hex())
	values := make([]any, 19)
	pointers := make([]any, len(values))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := row.Scan(pointers...); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func cursorRecoveryRefusal(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("CREATE TRIGGER n04_book_cursor_refusal BEFORE UPDATE ON nft_order_meta BEGIN SELECT RAISE(ABORT,'declared book cursor refusal'); END"); err != nil {
		t.Fatal(err)
	}
}

func TestOrderBookRewindKeepsOrderAndCursorAtomic(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0)
	_, db := indexer.NewRecoveryIndex(t, chain)
	book, err := orderbook.New(orderbook.Config{DB: db.SQL()})
	if err != nil {
		t.Fatal(err)
	}
	hash := storedRecoveryOrder(t, db.SQL())
	before := indexer.RecoveryState(t, db)
	intent := storedRecoveryIntent(t, db.SQL(), hash)
	cursorRecoveryRefusal(t, db.SQL())
	if err := book.Rewind(1); err == nil {
		t.Fatal("declared cursor write refusal was not reached")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
	if storedRecoveryIntent(t, db.SQL(), hash) != intent {
		t.Fatal("a refused rewind modified the original order")
	}
}

func bindRecoveryMarket(ix *indexer.Indexer, result nodecmd.Result) {
	result.BindRewind(ix)
}

func recoveryMarketIndex(t *testing.T) (*indexer.Indexer, *store.DB, func(string)) {
	t.Helper()
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0)
	chain.Put(2, 0)
	base, db := indexer.NewRecoveryIndex(t, chain)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &nodecmd.Market{}
	m.Flags(flag.NewFlagSet("declared-market-recovery", flag.ContinueOnError))
	cfg := &rollup.Config{L1StartHeight: 2, L2ChainID: 69}
	// Empty application/media flags keep the market RPC loop disabled. The
	// actual Market.Build result and Book database wiring still execute.
	result, err := m.Build(ctx, nodecmd.Env{Rollup: cfg, DB: db, Log: base.Log()})
	if err != nil {
		t.Fatal(err)
	}
	fault, fail := indexer.NewRecoveryFailure()
	ix, err := indexer.New(db, base.Eth(), cfg, base.Log(), append(result.Plugins, fault)...)
	if err != nil {
		t.Fatal(err)
	}
	bindRecoveryMarket(ix, result)
	nftIndexThrough(t, ix, 1, 2)
	return ix, db, fail
}

func TestIndexRewindDoesNotCommitWhenOrderBookRefuses(t *testing.T) {
	ix, db, _ := recoveryMarketIndex(t)
	storedRecoveryOrder(t, db.SQL())
	before := indexer.RecoveryState(t, db)
	cursorRecoveryRefusal(t, db.SQL())
	if err := ix.Rewind(1); err == nil {
		t.Fatal("the index committed despite a refused order-book rewind")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
}

func TestIndexAndOrderBookRewindPreserveOriginalIntent(t *testing.T) {
	ix, db, _ := recoveryMarketIndex(t)
	hash := storedRecoveryOrder(t, db.SQL())
	intent := storedRecoveryIntent(t, db.SQL(), hash)
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	if head, err := ix.Head(); err != nil || head != 1 {
		t.Fatalf("index head=%d, want 1: %v", head, err)
	}
	if storedRecoveryIntent(t, db.SQL(), hash) != intent {
		t.Fatal("rewind changed the original order hash, payload or acceptance fields")
	}
	var status string
	var checked int64
	var closed sql.NullInt64
	if err := db.SQL().QueryRow("SELECT status,checked_at,closed_block FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&status, &checked, &closed); err != nil {
		t.Fatal(err)
	}
	if status != orderbook.StatusOpen || checked != 0 || closed.Valid {
		t.Fatalf("rewind cache markers=%s/%d/%v, want open/0/no closure", status, checked, closed)
	}
	// Fill amounts, check codes and in-flight RPC observations are separate
	// acceptance cases; this test does not call the reopened order fillable.
}

func TestOrderBookWritesRollBackWithIndexCommitRefusal(t *testing.T) {
	ix, db, fail := recoveryMarketIndex(t)
	storedRecoveryOrder(t, db.SQL())
	before := indexer.RecoveryState(t, db)
	observed := false
	ix.BeforeRewindCommit(func(tx *sql.Tx, number uint64) error {
		var status string
		var cursor uint64
		if err := tx.QueryRow("SELECT status FROM nft_orders").Scan(&status); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT value FROM nft_order_meta WHERE key='scanned:fixture'").Scan(&cursor); err != nil {
			return err
		}
		if number != 1 || status != orderbook.StatusOpen || cursor != 1 {
			t.Fatalf("book had not rewound inside the index transaction: %d/%s/%d", number, status, cursor)
		}
		observed = true
		return nil
	})
	fail("commit")
	if err := ix.Rewind(1); err == nil {
		t.Fatal("declared index Commit refusal was not reached")
	}
	if !observed {
		t.Fatal("the order-book writes were never observed before Commit refusal")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
}
