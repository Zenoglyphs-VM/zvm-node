package orderbook

import (
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// exchangeVM deploys the compiled ZVMExchange into an in-memory EVM, so the Go
// side of the order book is checked against the contract that settles orders
// rather than against a second reading of the spec.
type exchangeVM struct {
	t    *testing.T
	art  *bench.Artifact
	cfg  *runtime.Config
	addr common.Address
}

// loadArtifacts is apps.LoadArtifacts, read from the file: package apps uses
// this one, so importing it here would be a cycle.
func loadArtifacts(t *testing.T) *bench.Artifacts {
	t.Helper()
	arts, err := bench.LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../apps/artifacts/apps.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := arts.AddCombined(raw); err != nil {
		t.Fatal(err)
	}
	return arts
}

func newExchangeVM(t *testing.T) *exchangeVM {
	t.Helper()
	arts := loadArtifacts(t)
	art := arts.MustGet("ZVMExchange")
	owner := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	init, err := art.Deploy(owner, owner, big.NewInt(0))
	if err != nil {
		t.Fatal(err)
	}
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	// Every fork on, and this chain's id: OpenZeppelin's EIP712 caches the
	// domain with block.chainid at construction.
	chain := *params.MergedTestChainConfig
	chain.ChainID = big.NewInt(7340470)
	cfg := &runtime.Config{State: statedb, GasLimit: 30_000_000, Value: new(big.Int), ChainConfig: &chain,
		BlockNumber: big.NewInt(1), Time: 1_800_000_000}
	_, addr, _, err := runtime.Create(init, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &exchangeVM{t: t, art: art, cfg: cfg, addr: addr}
}

func (x *exchangeVM) call(method string, args ...any) []any {
	x.t.Helper()
	data, err := x.art.Call(method, args...)
	if err != nil {
		x.t.Fatal(err)
	}
	out, _, err := runtime.Call(x.addr, data, x.cfg)
	if err != nil {
		x.t.Fatalf("%s: %v", method, err)
	}
	res, err := x.art.ABI.Unpack(method, out)
	if err != nil {
		x.t.Fatalf("%s: %v", method, err)
	}
	return res
}

func sampleOrder(trader common.Address, token int64) *Order {
	return &Order{
		Trader: trader, Side: SideSell, Collection: common.HexToAddress("0x00000000000000000000000000000000000c0c0c"),
		TokenID: U(token), Amount: U(1), Price: B(new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18))),
		ListingTime: 1, ExpirationTime: 4_000_000_000, Nonce: U(0), Salt: U(token * 7919),
	}
}

func TestHashingMatchesTheExchange(t *testing.T) {
	x := newExchangeVM(t)
	key, _ := crypto.GenerateKey()
	o := sampleOrder(crypto.PubkeyToAddress(key.PublicKey), 3)
	o.PaymentToken = common.HexToAddress("0x0000000000000000000000000000000000000fee")

	if got := x.call("hashOrder", o.ABI())[0].([32]byte); common.Hash(got) != o.Hash() {
		t.Fatalf("order hash: contract %x, Go %s", got, o.Hash())
	}
	domain := Domain(7340470, x.addr)
	if got := x.call("domainSeparator")[0].([32]byte); common.Hash(got) != domain {
		t.Fatalf("domain: contract %x, Go %s", got, domain)
	}
	if got := x.call("orderDigest", o.Hash())[0].([32]byte); common.Hash(got) != OrderDigest(domain, o.Hash()) {
		t.Fatal("order digest differs")
	}
	root := common.HexToHash("0x1234")
	if got := x.call("rootDigest", root)[0].([32]byte); common.Hash(got) != RootDigest(domain, root) {
		t.Fatal("root digest differs")
	}
}

// A good signature gets past the signature check and stops at the next one —
// the seller does not hold a token in a collection that does not exist — while
// a bad one stops at the signature. That tells the two apart without deploying
// an ERC-721.
func TestSignaturesVerifyOnTheExchange(t *testing.T) {
	x := newExchangeVM(t)
	domain := Domain(7340470, x.addr)
	key, _ := crypto.GenerateKey()
	trader := crypto.PubkeyToAddress(key.PublicKey)
	code := func(in *Input) uint8 {
		t.Helper()
		return x.call("checkOrder", in.ABI())[0].(uint8)
	}

	single, err := Sign(key, domain, sampleOrder(trader, 1))
	if err != nil {
		t.Fatal(err)
	}
	if c := code(single); c != CodeNotHeld {
		t.Fatalf("a correctly signed listing should fail only on custody, got %d (%s)", c, CodeText(c))
	}
	if got, ok := Recover(domain, single); !ok || got != trader {
		t.Fatalf("recovered %s, want %s", got, trader)
	}
	tampered := *single
	tampered.Order.Price = U(1)
	if c := code(&tampered); c != CodeBadSignature {
		t.Fatalf("a changed price must break the signature, got %d", c)
	}

	// Bulk: seven orders, one signature. Seven is odd at two levels, so the
	// carried-up node is exercised as well as the pairs.
	var orders []*Order
	for i := int64(1); i <= 7; i++ {
		orders = append(orders, sampleOrder(trader, 100+i))
	}
	bulk, err := SignBulk(key, domain, orders)
	if err != nil {
		t.Fatal(err)
	}
	for i, in := range bulk {
		if c := code(in); c != CodeNotHeld {
			t.Fatalf("bulk order %d: got %d (%s)", i, c, CodeText(c))
		}
		if ProcessProof(in.Order.Hash(), in.Proof) != *in.Root {
			t.Fatalf("bulk order %d: proof does not reach the root", i)
		}
	}
	// A proof for one order does not authorise another.
	swapped := *bulk[0]
	swapped.Proof = bulk[1].Proof
	if c := code(&swapped); c != CodeBadSignature {
		t.Fatalf("a proof from another leaf must fail, got %d", c)
	}
	// An order approved on chain needs no signature — but only once approved.
	onchain := &Input{Order: *sampleOrder(trader, 9), Kind: KindOnChain, Signature: []byte{}, Proof: []common.Hash{}}
	if c := code(onchain); c != CodeBadSignature {
		t.Fatalf("an unapproved on-chain order must fail, got %d", c)
	}
}

func TestTreeProofs(t *testing.T) {
	for n := 1; n <= 9; n++ {
		leaves := make([]common.Hash, n)
		for i := range leaves {
			leaves[i] = crypto.Keccak256Hash([]byte{byte(n), byte(i)})
		}
		root, proofs := Tree(leaves)
		for i, leaf := range leaves {
			if ProcessProof(leaf, proofs[i]) != root {
				t.Fatalf("n=%d leaf %d does not prove to the root", n, i)
			}
		}
	}
}

func TestCheckRefusesWhatTheExchangeWould(t *testing.T) {
	trader := common.HexToAddress("0x01")
	for name, mutate := range map[string]func(*Order){
		"a listing of two":          func(o *Order) { o.Amount = U(2) },
		"a collection-wide listing": func(o *Order) { o.AnyToken = true },
		"a bid in ZNN":              func(o *Order) { o.Side = SideBuy },
		"no price":                  func(o *Order) { o.Price = U(0) },
		"already expired":           func(o *Order) { o.ExpirationTime = o.ListingTime },
		"no trader":                 func(o *Order) { o.Trader = common.Address{} },
	} {
		o := sampleOrder(trader, 1)
		mutate(o)
		if o.Check() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := sampleOrder(trader, 1).Check(); err != nil {
		t.Fatalf("a good listing was refused: %v", err)
	}
}
