package indexer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// This fixture runs the actual NFT plugin, index rewind transaction, book
// epoch invalidation, Market.Register routes and HTTP JSON encoder. The chain
// is the existing loopback geth-block/receipt fixture, not a live NoM node.
// Stored check observations below are declared controls, not exchange calls,
// signatures, browser actions or proof that an RPC recheck succeeded.
type a08HTTPFixture struct {
	index *indexer.Indexer
	db    *store.DB
	book  *orderbook.Book
	chain *indexer.RecoveryChain
	url   string
	input orderbook.Input
	hash  common.Hash
	fail  func(string)
}

func a08HTTPIndex(t *testing.T, chain *indexer.RecoveryChain) *a08HTTPFixture {
	t.Helper()
	ix, db, fail := nftRecoveryIndex(t, chain)
	book, err := orderbook.New(orderbook.Config{DB: db.SQL(), ChainID: 69, Exchange: func() common.Address { return nftExchange }})
	if err != nil {
		t.Fatal(err)
	}
	// This is the same shared-transaction callback used by Result.BindRewind.
	ix.BeforeRewindCommit(book.RewindTx)
	deployment := filepath.Join(t.TempDir(), "apps.json")
	if err := (&apps.Deployment{ChainID: 69, Exchange: nftExchange}).Save(deployment); err != nil {
		t.Fatal(err)
	}
	server := market.New(market.Config{DB: db.SQL(), Orders: book,
		Rollup: &rollup.Config{L2ChainID: 69}, Apps: &apps.File{Path: deployment, ChainID: 69}, Log: ix.Log()})
	mux := http.NewServeMux()
	server.Register(mux)
	web := httptest.NewServer(mux)
	t.Cleanup(web.Close)
	return &a08HTTPFixture{index: ix, db: db, book: book, chain: chain, url: web.URL, fail: fail}
}

func (f *a08HTTPFixture) get(t *testing.T, route string) json.RawMessage {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(f.url + route)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		t.Fatalf("bounded HTTP response: %v", err)
	}
	if response.StatusCode != http.StatusOK || !json.Valid(raw) {
		t.Fatalf("GET %s returned %d: %s", route, response.StatusCode, raw)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

func a08Object(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func a08List(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *a08HTTPFixture) storeListing(t *testing.T) {
	t.Helper()
	f.input = orderbook.Input{Order: orderbook.Order{Trader: nftAlice, Side: orderbook.SideSell, Collection: nftCollection,
		TokenID: orderbook.U(1), Amount: orderbook.U(1), Price: orderbook.U(1000), ListingTime: 0,
		ExpirationTime: 1 << 32, Nonce: orderbook.U(4), Salt: orderbook.U(7)},
		Kind: orderbook.KindSingle, Signature: hexutil.Bytes{1, 2, 3}, Proof: []common.Hash{}}
	raw, err := json.Marshal(f.input)
	if err != nil {
		t.Fatal(err)
	}
	f.hash = f.input.Order.Hash()
	values := []any{f.hash.Hex(), nftExchange.Hex(), nftAlice.Hex(), orderbook.SideSell, nftCollection.Hex(), "1", 1, 0,
		(common.Address{}).Hex(), "1000", 0.000000000000001, 0, 1 << 32, "4", orderbook.KindSingle,
		string(raw), orderbook.StatusOpen, orderbook.CodeUnchecked, 1, 1010, 0, "declared-HTTP-cache-control"}
	_, err = f.db.SQL().Exec(`INSERT INTO nft_orders
		(hash,exchange,trader,side,collection,token_id,amount,filled,payment_token,price,price_num,
		listing_time,expiration_time,nonce,kind,input,status,code,block,timestamp,checked_at,source)
		VALUES (`+strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")+`)`, values...)
	if err != nil {
		t.Fatal(err)
	}
	f.declareCurrentCheck(t)
}

// Deliberately calibrate an open CodeOK cache row even when indexed ownership
// or permissions disagree. Market projections must apply their independent
// index predicate. This does not model or qualify Book.Submit or RPC refresh.
func (f *a08HTTPFixture) declareCurrentCheck(t *testing.T) {
	t.Helper()
	head, err := f.index.Head()
	if err != nil {
		t.Fatal(err)
	}
	header, err := f.index.Eth().HeaderByNumber(context.Background(), new(big.Int).SetUint64(head))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.db.SQL().Exec(`UPDATE nft_orders SET status=?,code=?,checked_at=1,
		checked_epoch=(SELECT value FROM nft_order_meta WHERE key='rewind_epoch'),
		filled_epoch=(SELECT value FROM nft_order_meta WHERE key='rewind_epoch'),checked_block=?,checked_hash=? WHERE hash=?`,
		orderbook.StatusOpen, orderbook.CodeOK, head, header.Hash().Hex(), f.hash.Hex())
	if err != nil {
		t.Fatal(err)
	}
}

func (f *a08HTTPFixture) assertProjection(t *testing.T, want bool, owner common.Address) {
	t.Helper()
	listings := a08List(t, f.get(t, "/api/nft/listings?collection="+nftCollection.Hex()))
	item := a08Object(t, f.get(t, "/api/nft/items/"+nftCollection.Hex()+"/1"))
	itemListings := a08List(t, item["listings"])
	wantCount := 0
	if want {
		wantCount = 1
	}
	if len(listings) != wantCount || len(itemListings) != wantCount {
		t.Fatalf("visible listings=%d item listings=%d, want %d", len(listings), len(itemListings), wantCount)
	}
	var actualOwner string
	if err := json.Unmarshal(item["owner"], &actualOwner); err != nil || actualOwner != owner.Hex() {
		t.Fatalf("HTTP owner=%s, want %s: %v", actualOwner, owner.Hex(), err)
	}
	if want {
		for _, row := range []map[string]json.RawMessage{listings[0], itemListings[0]} {
			var hash, price string
			if err := json.Unmarshal(row["hash"], &hash); err != nil || hash != f.hash.Hex() {
				t.Fatalf("HTTP order hash=%s, want %s: %v", hash, f.hash.Hex(), err)
			}
			if err := json.Unmarshal(row["price"], &price); err != nil || price != "1000" {
				t.Fatalf("HTTP exact price=%s: %v", price, err)
			}
		}
	}
}

func (f *a08HTTPFixture) assertUnchecked(t *testing.T) {
	t.Helper()
	row := a08Object(t, f.get(t, "/api/nft/orders/"+f.hash.Hex()))
	var fillable bool
	var code uint8
	if err := json.Unmarshal(row["fillable"], &fillable); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(row["code"], &code); err != nil || fillable || code != orderbook.CodeUnchecked {
		t.Fatalf("HTTP rewind gate=%t/%d, want false/unchecked: %v", fillable, code, err)
	}
	var hash string
	var input orderbook.Input
	if err := json.Unmarshal(row["hash"], &hash); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(row["input"], &input); err != nil || hash != f.hash.Hex() || input.Order.Hash() != f.hash {
		t.Fatalf("rewind changed HTTP input identity: %v", err)
	}
	orders := a08List(t, f.get(t, "/api/nft/orders?collection="+nftCollection.Hex()))
	if len(orders) != 0 {
		t.Fatal("unchecked observations were exposed by the default book listing")
	}
}

func TestMarketHTTPReorgVisibility(t *testing.T) {
	type branch struct {
		name         string
		prefix       func() []*types.Log
		orphan       func() []*types.Log
		replacement  func() []*types.Log
		prefixOpen   bool
		orphanOpen   bool
		orphanOwner  common.Address
		replaceOpen  bool
		replaceOwner common.Address
	}
	mint := func() *types.Log { return nftTransfer(common.Address{}, nftAlice) }
	operator := func(value bool) *types.Log { return retainedOperator(nftAlice, nftExchange, value) }
	tokenApproval := func(to common.Address) *types.Log { return retainedApproval(nftAlice, to, common.Hash{31: 1}) }
	cases := []branch{
		{"owner-transfer", func() []*types.Log { return []*types.Log{mint(), operator(true)} },
			func() []*types.Log { return []*types.Log{nftTransfer(nftAlice, nftBob)} },
			func() []*types.Log { return []*types.Log{operator(false)} }, true, false, nftBob, false, nftAlice},
		{"token-approval-transfer", func() []*types.Log { return []*types.Log{mint(), tokenApproval(nftExchange)} },
			func() []*types.Log { return []*types.Log{nftTransfer(nftAlice, nftBob)} },
			func() []*types.Log {
				return []*types.Log{nftTransfer(nftAlice, nftBob), retainedApproval(nftBob, nftExchange, common.Hash{31: 1})}
			}, true, false, nftBob, false, nftBob},
		{"operator-grant", func() []*types.Log { return []*types.Log{mint(), operator(false)} },
			func() []*types.Log { return []*types.Log{operator(true)} },
			func() []*types.Log { return []*types.Log{operator(false)} }, false, true, nftAlice, false, nftAlice},
		{"operator-revoke", func() []*types.Log { return []*types.Log{mint(), operator(true)} },
			func() []*types.Log { return []*types.Log{operator(false)} },
			func() []*types.Log { return []*types.Log{operator(true)} }, true, false, nftAlice, true, nftAlice},
		{"token-revoke", func() []*types.Log { return []*types.Log{mint(), tokenApproval(nftExchange)} },
			func() []*types.Log { return []*types.Log{tokenApproval(common.Address{})} },
			func() []*types.Log { return []*types.Log{tokenApproval(common.Address{})} }, true, false, nftAlice, false, nftAlice},
		{"unrelated-owner", func() []*types.Log { return []*types.Log{mint(), operator(true)} },
			func() []*types.Log { return []*types.Log{retainedOperator(nftBob, nftExchange, false)} },
			func() []*types.Log { return []*types.Log{nftTransfer(nftAlice, nftBob)} }, true, true, nftAlice, false, nftBob},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, scenario.prefix()...)
			f := a08HTTPIndex(t, chain)
			nftIndexThrough(t, f.index, 1, 1)
			f.storeListing(t)
			intent := storedRecoveryIntent(t, f.db.SQL(), f.hash)
			f.assertProjection(t, scenario.prefixOpen, nftAlice)
			chain.Put(2, 0, scenario.orphan()...)
			nftIndexThrough(t, f.index, 2, 2)
			f.declareCurrentCheck(t)
			f.assertProjection(t, scenario.orphanOpen, scenario.orphanOwner)
			if err := f.index.Rewind(1); err != nil {
				t.Fatal(err)
			}
			f.assertUnchecked(t)
			f.assertProjection(t, false, nftAlice)
			if storedRecoveryIntent(t, f.db.SQL(), f.hash) != intent {
				t.Fatal("HTTP recovery changed the original stored intent")
			}
			f.declareCurrentCheck(t)
			f.assertProjection(t, scenario.prefixOpen, nftAlice)
			chain.Put(2, 1, scenario.replacement()...)
			nftIndexThrough(t, f.index, 2, 2)
			f.declareCurrentCheck(t)
			f.assertProjection(t, scenario.replaceOpen, scenario.replaceOwner)
			if storedRecoveryIntent(t, f.db.SQL(), f.hash) != intent {
				t.Fatal("replacement branch changed the original stored intent")
			}
		})
	}
}

func TestMarketHTTPRefusedRewind(t *testing.T) {
	for _, phase := range []string{"restore", "rebuild", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedOperator(nftAlice, nftExchange, true))
			chain.Put(2, 0)
			f := a08HTTPIndex(t, chain)
			nftIndexThrough(t, f.index, 1, 2)
			f.storeListing(t)
			f.assertProjection(t, true, nftAlice)
			before := indexer.RecoveryState(t, f.db)
			routes := []string{"/api/nft/listings?collection=" + nftCollection.Hex(), "/api/nft/items/" + nftCollection.Hex() + "/1", "/api/nft/orders/" + f.hash.Hex()}
			bodies := make([]json.RawMessage, len(routes))
			for i, route := range routes {
				bodies[i] = f.get(t, route)
			}
			f.fail(phase)
			if err := f.index.Rewind(1); err == nil {
				t.Fatalf("declared %s refusal was not reached", phase)
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, f.db), before)
			for i, route := range routes {
				if got := f.get(t, route); !bytes.Equal(got, bodies[i]) {
					t.Fatalf("refused %s changed %s: %s", phase, route, got)
				}
			}
			f.fail("")
			if err := f.index.Rewind(1); err != nil {
				t.Fatal(err)
			}
			f.assertUnchecked(t)
		})
	}
}
