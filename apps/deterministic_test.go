package apps

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// An application address must be a pure function of published values: the
// committed artifacts, the salt, and the recipe (D32).
func TestPlanIsDeterministic(t *testing.T) {
	arts, err := LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	owner := common.Address(devnet.Key(1).Address)
	a, err := BuildPlan(arts, DefaultRecipe(owner, 100_000, 30))
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildPlan(arts, DefaultRecipe(owner, 100_000, 30))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range a.All() {
		if c.Address != b.All()[i].Address {
			t.Fatalf("%s is not deterministic", c.Name)
		}
		if c.Address == (common.Address{}) {
			t.Fatalf("%s has no address", c.Name)
		}
		// The address must be what CREATE2 says, from the deployer in genesis.
		want := rollup.Create2Address(rollup.Create2Deployer, c.Salt, c.InitCode)
		if c.Address != want {
			t.Fatalf("%s: %s is not CREATE2(%s, %s)", c.Name, c.Address, rollup.Create2Deployer, c.Salt)
		}
	}

	// A different owner is a different marketplace, at a different address:
	// that is what stops one being passed off as the other.
	other, err := BuildPlan(arts, DefaultRecipe(common.HexToAddress("0xdead"), 100_000, 30))
	if err != nil {
		t.Fatal(err)
	}
	if other.Exchange.Address == a.Exchange.Address {
		t.Fatal("two owners produced one exchange address")
	}
	if other.Collection.Address != a.Collection.Address {
		t.Fatal("the collection implementation takes no arguments, so it should not move")
	}

	// And the factory's address follows the implementation it is given.
	if a.Factory.Address == other.Factory.Address && a.Collection.Address != other.Collection.Address {
		t.Fatal("the factory should depend on the implementation")
	}

	// A recipe with no reward rate is refused rather than deployed.
	if _, err := BuildPlan(arts, Recipe{Owner: owner, RewardToken: rollup.QSRTokenAddr, RewardRate: big.NewInt(0)}); err == nil {
		t.Fatal("a zero reward rate was accepted")
	}
}

func TestMatchesRejectsAnotherDeployment(t *testing.T) {
	arts, err := LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	from := crypto.PubkeyToAddress(devnet.EVMKey(bench.FunderEVMKey).PublicKey)
	plan, err := BuildPlan(arts, DefaultRecipe(from, 100_000, 30))
	if err != nil {
		t.Fatal(err)
	}
	good := &Deployment{
		CollectionImpl: plan.Collection.Address, Exchange: plan.Exchange.Address,
		CollectionFactory: plan.Factory.Address, Farm: plan.Farm.Address,
	}
	if !plan.Matches(good) {
		t.Fatal("the plan should match its own addresses")
	}
	good.Exchange = common.HexToAddress("0xbeef")
	if plan.Matches(good) {
		t.Fatal("a file naming somebody else's exchange must not match")
	}
	if plan.Matches(nil) {
		t.Fatal("nil matched")
	}
}

// The fee strategy is planned only when the recipe names a swap, and adding it
// must not move any address that already exists. Chain 69's marketplace was
// deployed before the strategy did, and it has to keep matching its own file.
func TestFeeStrategyIsAdditive(t *testing.T) {
	arts, err := LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	owner := common.Address(devnet.Key(1).Address)
	base := DefaultRecipe(owner, 100_000, 30)

	without, err := BuildPlan(arts, base)
	if err != nil {
		t.Fatal(err)
	}
	if without.HasFeeStrategy {
		t.Fatal("a recipe with no swap should plan no fee strategy")
	}
	if len(without.All()) != 4 {
		t.Fatalf("plan has %d contracts, want the original four", len(without.All()))
	}

	withSwap := base
	withSwap.Router = common.HexToAddress("0x0000000000000000000000000000000000000a01")
	withSwap.SwapFactory = common.HexToAddress("0x0000000000000000000000000000000000000a02")
	withSwap.WETH = common.HexToAddress("0x0000000000000000000000000000000000000a03")
	with, err := BuildPlan(arts, withSwap)
	if err != nil {
		t.Fatal(err)
	}
	if !with.HasFeeStrategy {
		t.Fatal("a recipe naming a router should plan a fee strategy")
	}
	if with.FeeStrategy.Address == (common.Address{}) {
		t.Fatal("the fee strategy has no address")
	}
	for i, c := range without.All() {
		if with.All()[i].Address != c.Address {
			t.Fatalf("%s moved when the fee strategy was added: %s -> %s", c.Name, c.Address, with.All()[i].Address)
		}
	}
	// The strategy's constructor reads the farm, so it must land after it.
	rounds := with.Rounds()
	if len(rounds) != 2 || len(rounds[1]) != 1 || rounds[1][0].Address != with.FeeStrategy.Address {
		t.Fatalf("the fee strategy is not in a round of its own after the farm: %v", rounds)
	}
	var n int
	for _, r := range rounds {
		n += len(r)
	}
	if n != len(with.All()) {
		t.Fatalf("the rounds hold %d contracts, the plan %d", n, len(with.All()))
	}

	// It reads the farm and the pools, so a different farm or a different swap is
	// a different strategy — the same rule every other address here follows.
	other := withSwap
	other.Router = common.HexToAddress("0xdead")
	moved, err := BuildPlan(arts, other)
	if err != nil {
		t.Fatal(err)
	}
	if moved.FeeStrategy.Address == with.FeeStrategy.Address {
		t.Fatal("two routers produced one strategy address")
	}

	// A deployment file written before the strategy existed still matches.
	old := &Deployment{
		CollectionImpl: with.Collection.Address, Exchange: with.Exchange.Address,
		CollectionFactory: with.Factory.Address, Farm: with.Farm.Address,
	}
	if !with.Matches(old) {
		t.Fatal("a pre-strategy deployment file should still match this plan")
	}
	wrong := *old
	wrong.FeeStrategy = common.HexToAddress("0xbeef")
	if with.Matches(&wrong) {
		t.Fatal("a file naming the wrong strategy should not match")
	}
}
