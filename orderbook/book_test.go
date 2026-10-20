package orderbook

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
	_ "modernc.org/sqlite"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// simChain is a chain with the exchange, a collection, wrapped ZNN and
// Multicall3 on it, and two funded wallets.
type simChain struct {
	t        *testing.T
	sim      *simulated.Backend
	arts     *bench.Artifacts
	seller   *ecdsa.PrivateKey
	buyer    *ecdsa.PrivateKey
	exchange common.Address
	nft      common.Address
	wznn     common.Address
	nonces   map[common.Address]uint64
}

var chainID = big.NewInt(1337)

func newSimChain(t *testing.T) *simChain {
	t.Helper()
	arts := loadArtifacts(t)
	// The same bytes genesis puts at this address, read from the package that
	// builds genesis rather than from a path into another repo.
	mc, err := l2.PredeployCode("Multicall3")
	if err != nil {
		t.Fatal(err)
	}
	seller, _ := crypto.GenerateKey()
	buyer, _ := crypto.GenerateKey()
	rich := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e18))
	sim := simulated.NewBackend(types.GenesisAlloc{
		crypto.PubkeyToAddress(seller.PublicKey): {Balance: rich},
		crypto.PubkeyToAddress(buyer.PublicKey):  {Balance: rich},
		multicall3:                               {Code: mc, Balance: new(big.Int)},
	})
	t.Cleanup(func() { sim.Close() })
	c := &simChain{t: t, sim: sim, arts: arts, seller: seller, buyer: buyer, nonces: map[common.Address]uint64{}}
	owner := crypto.PubkeyToAddress(seller.PublicKey)
	c.exchange = c.deploy(seller, "ZVMExchange", owner, owner, big.NewInt(0))
	c.nft = c.deploy(seller, "ZVMCollection")
	c.wznn = c.deploy(buyer, "WZNN")
	return c
}

func (c *simChain) send(key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) *types.Receipt {
	c.t.Helper()
	from := crypto.PubkeyToAddress(key.PublicKey)
	tx := types.MustSignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: c.nonces[from], GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(100e9),
		Gas: 8_000_000, To: to, Value: value, Data: data,
	})
	c.nonces[from]++
	if err := c.sim.Client().SendTransaction(context.Background(), tx); err != nil {
		c.t.Fatal(err)
	}
	c.sim.Commit()
	r, err := c.sim.Client().TransactionReceipt(context.Background(), tx.Hash())
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c *simChain) deploy(key *ecdsa.PrivateKey, name string, args ...any) common.Address {
	c.t.Helper()
	init, err := c.arts.MustGet(name).Deploy(args...)
	if err != nil {
		c.t.Fatal(err)
	}
	r := c.send(key, nil, new(big.Int), init)
	if r.Status != types.ReceiptStatusSuccessful {
		c.t.Fatalf("deploying %s failed", name)
	}
	return r.ContractAddress
}

func (c *simChain) call(key *ecdsa.PrivateKey, to common.Address, artifact, method string, value *big.Int, args ...any) *types.Receipt {
	c.t.Helper()
	data, err := c.arts.MustGet(artifact).Call(method, args...)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.send(key, &to, value, data)
}

func (c *simChain) must(r *types.Receipt, what string) {
	c.t.Helper()
	if r.Status != types.ReceiptStatusSuccessful {
		c.t.Fatalf("%s reverted", what)
	}
}

func (c *simChain) now() uint64 {
	h, err := c.sim.Client().HeaderByNumber(context.Background(), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	return h.Time
}

func (c *simChain) book() (*Book, *sql.DB) {
	c.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(c.t.TempDir(), "orders.db")))
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { db.Close() })
	b, err := New(Config{DB: db, Eth: c.sim.Client(), ChainID: chainID.Uint64(),
		Exchange: func() common.Address { return c.exchange }, Artifact: c.arts.MustGet("ZVMExchange"),
		Recheck: time.Nanosecond})
	if err != nil {
		c.t.Fatal(err)
	}
	return b, db
}

func status(t *testing.T, db *sql.DB, h common.Hash) (string, int64) {
	t.Helper()
	var s string
	var filled int64
	if err := db.QueryRow("SELECT status, filled FROM nft_orders WHERE hash = ?", h.Hex()).Scan(&s, &filled); err != nil {
		t.Fatalf("order %s: %v", h, err)
	}
	return s, filled
}

func TestBookFollowsTheExchange(t *testing.T) {
	c := newSimChain(t)
	ctx := context.Background()
	b, db := c.book()
	domain, _, _ := b.Domain()
	seller := crypto.PubkeyToAddress(c.seller.PublicKey)
	buyer := crypto.PubkeyToAddress(c.buyer.PublicKey)
	c.must(c.call(c.seller, c.nft, "ZVMCollection", "mintBatch", nil, seller,
		[]string{"a", "b", "c", "d", "e", "f"}), "mint")
	now := c.now()

	listing := func(id int64, price int64) *Order {
		return &Order{Trader: seller, Side: SideSell, Collection: c.nft, TokenID: U(id), Amount: U(1),
			Price: B(new(big.Int).Mul(big.NewInt(price), big.NewInt(1e18))), ListingTime: now - 10,
			ExpirationTime: now + 86_400, Nonce: U(0), Salt: U(id)}
	}

	// Signed before approving the exchange: refused, and said why.
	first, err := Sign(c.seller, domain, listing(1, 5))
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Submit(ctx, []*Input{first})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].OK || res[0].Code != CodeNotHeld {
		t.Fatalf("an unapproved listing should be refused as not held, got %+v", res[0])
	}
	c.must(c.call(c.seller, c.nft, "ZVMCollection", "setApprovalForAll", nil, c.exchange, true), "approve")
	if res, _ = b.Submit(ctx, []*Input{first}); !res[0].OK {
		t.Fatalf("an approved listing was refused: %+v", res[0])
	}

	// A forged signature is refused without asking the chain.
	forged, _ := Sign(c.buyer, domain, listing(2, 1))
	if res, _ = b.Submit(ctx, []*Input{forged}); res[0].OK || res[0].Code != CodeBadSignature {
		t.Fatalf("a listing signed by someone else was accepted: %+v", res[0])
	}

	// Three more in one signature.
	bulk, err := SignBulk(c.seller, domain, []*Order{listing(2, 6), listing(3, 7), listing(4, 8)})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ = b.Submit(ctx, bulk); !res[0].OK || !res[1].OK || !res[2].OK {
		t.Fatalf("bulk listing refused: %+v", res)
	}

	// The buyer fills the first listing on chain; the book notices.
	c.must(c.call(c.buyer, c.exchange, "ZVMExchange", "buy", listing(1, 5).Price.Big(), first.ABI()), "buy")
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if s, filled := status(t, db, first.Order.Hash()); s != StatusFilled || filled != 1 {
		t.Fatalf("the bought listing is %s with %d filled", s, filled)
	}

	// Moving a token away closes its listing at the next sweep, with no
	// exchange event to say so.
	c.must(c.call(c.seller, c.nft, "ZVMCollection", "transferFrom", nil, seller, buyer, big.NewInt(4)), "transfer")
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	var code int
	db.QueryRow("SELECT code FROM nft_orders WHERE hash = ?", bulk[2].Order.Hash().Hex()).Scan(&code)
	if code != int(CodeNotHeld) {
		t.Fatalf("a listing whose token moved should read not held, got %d", code)
	}

	// A collection bid for two items, paid in wrapped ZNN.
	c.must(c.call(c.buyer, c.wznn, "WZNN", "deposit", new(big.Int).Mul(big.NewInt(20), big.NewInt(1e18))), "wrap")
	c.must(c.call(c.buyer, c.wznn, "WZNN", "approve", nil, c.exchange, new(big.Int).Mul(big.NewInt(20), big.NewInt(1e18))), "allow")
	bid := &Order{Trader: buyer, Side: SideBuy, Collection: c.nft, TokenID: U(0), AnyToken: true, Amount: U(2),
		PaymentToken: c.wznn, Price: B(new(big.Int).Mul(big.NewInt(4), big.NewInt(1e18))),
		ListingTime: now - 10, ExpirationTime: now + 86_400, Nonce: U(0), Salt: U(99)}
	signedBid, _ := Sign(c.buyer, domain, bid)
	if res, _ = b.Submit(ctx, []*Input{signedBid}); !res[0].OK {
		t.Fatalf("bid refused: %+v", res[0])
	}
	c.must(c.call(c.seller, c.exchange, "ZVMExchange", "acceptBid", nil, signedBid.ABI(), big.NewInt(5)), "accept")
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if s, filled := status(t, db, bid.Hash()); s != StatusOpen || filled != 1 {
		t.Fatalf("a half-filled bid is %s with %d filled", s, filled)
	}

	// An order approved on chain reaches the book from the log alone.
	// Token 2 is still the seller's (its bulk listing was never bought); a
	// different price makes this a different order.
	onchain := listing(2, 9)
	c.must(c.call(c.seller, c.exchange, "ZVMExchange", "approveOrders", nil, []ABIOrder{onchain.ABI()}), "approveOrders")
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(t, db, onchain.Hash()); s != StatusOpen {
		t.Fatalf("an on-chain approval should be an open order, is %s", s)
	}
	in, _, gotCode, err := b.Get(onchain.Hash())
	if err != nil || in.Kind != KindOnChain || gotCode != CodeOK {
		t.Fatalf("on-chain order: kind %d code %d err %v", in.Kind, gotCode, err)
	}

	// Bumping the nonce cancels everything the seller signed.
	c.must(c.call(c.seller, c.exchange, "ZVMExchange", "incrementNonce", nil), "incrementNonce")
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, in := range bulk[:2] {
		if s, _ := status(t, db, in.Order.Hash()); s != StatusCancelled {
			t.Fatalf("after a nonce bump a bulk listing is %s", s)
		}
	}
	if s, _ := status(t, db, onchain.Hash()); s != StatusCancelled {
		t.Fatalf("after a nonce bump the on-chain listing is %s", s)
	}
	// The buyer's bid is not the seller's to cancel.
	if s, _ := status(t, db, bid.Hash()); s != StatusOpen {
		t.Fatalf("the buyer's bid is %s after the seller's nonce bump", s)
	}
}

// Orders cost nothing to sign, and every open one is re-checked on each sweep,
// so the book has to be bounded: per trader, and for everyone together.
func TestBookIsBounded(t *testing.T) {
	c := newSimChain(t)
	ctx := context.Background()
	b, _ := c.book()
	domain, _, _ := b.Domain()
	seller := crypto.PubkeyToAddress(c.seller.PublicKey)
	c.must(c.call(c.seller, c.nft, "ZVMCollection", "mintBatch", nil, seller, []string{"a", "b", "c", "d"}), "mint")
	c.must(c.call(c.seller, c.nft, "ZVMCollection", "setApprovalForAll", nil, c.exchange, true), "approve")
	now := c.now()
	list := func(id int64) *Input {
		o := &Order{Trader: seller, Side: SideSell, Collection: c.nft, TokenID: U(id), Amount: U(1),
			Price: B(big.NewInt(1e18)), ListingTime: now - 10, ExpirationTime: now + 86_400, Nonce: U(0), Salt: U(id)}
		in, err := Sign(c.seller, domain, o)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}

	b.cfg.MaxOpenPerTrader = 2
	res, err := b.Submit(ctx, []*Input{list(0), list(1), list(2)})
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK || !res[1].OK || res[2].OK {
		t.Fatalf("with a per-trader cap of 2, three listings got %+v", res)
	}
	// Handing the book an order it already holds takes no new room.
	if res, _ := b.Submit(ctx, []*Input{list(0)}); !res[0].OK {
		t.Fatalf("resubmitting a held order was refused: %+v", res[0])
	}

	b.cfg.MaxOpenPerTrader, b.cfg.MaxOpen = 100, 2
	if res, _ := b.Submit(ctx, []*Input{list(3)}); res[0].OK || res[0].Reason != "this node's order book is full" {
		t.Fatalf("a full book took another order: %+v", res[0])
	}
}
