package apps

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// Deterministic application addresses (DECISIONS.md D32).
//
// The explorer believes marketplace and factory events only from the addresses
// it has been told to believe, because any contract can emit an event called
// `Listed`. Until now that list was a file the operator wrote, which is fine on
// one machine and wrong everywhere else: two explorers with different files
// would show two different marketplaces, and nothing on chain would say which
// one is real.
//
// So the addresses become a pure function of published values — the compiled
// creation code committed to this repository, the constructor arguments below,
// and one salt per contract — deployed through the deterministic deployer that
// is **already a genesis predeploy** (`0x4e59b448…`, the same address Foundry
// and Safe use on every chain). Nothing has to be added to genesis, and
// `bench/apps.json` stops being the only place the addresses exist: it becomes
// a cache of numbers anyone can recompute with `zvm apps addresses`.
//
// The constructor arguments are part of the init code, so they are part of the
// address. That is the point: a marketplace owned by somebody else is a
// different contract at a different address, and cannot be passed off as this
// one.

// Published salts. Changing one moves an address, which is a migration, not a
// configuration change.
var (
	SaltCollection  = rollup.Salt("collection", 1)
	SaltExchange    = rollup.Salt("exchange", 1)
	SaltFactory     = rollup.Salt("collection-factory", 1)
	SaltFarm        = rollup.Salt("farm", 1)
	SaltFeeStrategy = rollup.Salt("farm-fee-strategy", 1)
)

// Recipe is everything the addresses are a function of, besides the compiled
// code. Two chains with the same recipe have the same application addresses.
type Recipe struct {
	// Owner receives the exchange's fees and governs the farm. On a devnet
	// it is the deployer; on a real chain it is whoever governs these
	// contracts, and it must be published with the rest of the recipe.
	Owner common.Address
	// FeeBP is the exchange's fee, in basis points.
	FeeBP int64
	// RewardToken and RewardRate are the farm's, fixed at construction.
	RewardToken common.Address
	RewardRate  *big.Int
	// Router, SwapFactory and WETH are the Uniswap V2 deployment the fee strategy
	// redirects from. Left zero, there is no strategy in the plan: a chain with no
	// swap has no swap fees, and the strategy's address is a function of the
	// deployment it reads, so it cannot be computed before that exists.
	Router      common.Address
	SwapFactory common.Address
	WETH        common.Address
}

// DefaultRecipe is the recipe `zvm apps setup` uses: the farm's rate is a
// budget spread over a number of days, which is how the setup has always
// computed it.
func DefaultRecipe(owner common.Address, rewardQSR, farmDays int64) Recipe {
	rate := new(big.Int).Div(qsrUnits(rewardQSR), big.NewInt(farmDays*86_400))
	if rate.Sign() == 0 {
		rate = big.NewInt(1)
	}
	return Recipe{Owner: owner, FeeBP: 0, RewardToken: rollup.QSRTokenAddr, RewardRate: rate}
}

// Planned is one contract's deployment: where it lands and what to send.
type Planned struct {
	Name     string
	Salt     common.Hash
	InitCode []byte
	Address  common.Address
}

// Calldata is what the deterministic deployer is called with.
func (p Planned) Calldata() []byte { return rollup.DeployCalldata(p.Salt, p.InitCode) }

// Plan is the four application contracts, in deployment order. The factory
// depends on the collection implementation's address, so it comes after it —
// and because that address is itself deterministic, the whole plan can be
// computed before anything is deployed.
type Plan struct {
	Recipe     Recipe
	Collection Planned
	Exchange   Planned
	Factory    Planned
	Farm       Planned
	// FeeStrategy is present only when the recipe names a Uniswap deployment. Its
	// constructor reads the farm, so it necessarily comes last.
	FeeStrategy    Planned
	HasFeeStrategy bool
	Deployer       common.Address
}

// All is the plan in deployment order.
func (p *Plan) All() []Planned {
	out := []Planned{p.Collection, p.Exchange, p.Factory, p.Farm}
	if p.HasFeeStrategy {
		out = append(out, p.FeeStrategy)
	}
	return out
}

// Rounds is All in the order a chain can take it. The fee strategy's
// constructor reads the farm's reward token, so its deploy reverts in the same
// block as the farm's, and on a fresh chain it goes in a round of its own.
func (p *Plan) Rounds() [][]Planned {
	rounds := [][]Planned{{p.Collection, p.Exchange, p.Factory, p.Farm}}
	if p.HasFeeStrategy {
		rounds = append(rounds, []Planned{p.FeeStrategy})
	}
	return rounds
}

// BuildPlan computes every application address from the committed artifacts
// and the recipe. It touches no chain.
func BuildPlan(arts *bench.Artifacts, r Recipe) (*Plan, error) {
	if r.RewardRate == nil || r.RewardRate.Sign() <= 0 {
		return nil, fmt.Errorf("the farm's reward rate must be positive")
	}
	p := &Plan{Recipe: r, Deployer: rollup.Create2Deployer}
	plan := func(name string, salt common.Hash, args ...any) (Planned, error) {
		art, err := arts.Get(name)
		if err != nil {
			return Planned{}, err
		}
		init, err := art.Deploy(args...)
		if err != nil {
			return Planned{}, err
		}
		return Planned{
			Name: name, Salt: salt, InitCode: init,
			Address: rollup.Create2Address(rollup.Create2Deployer, salt, init),
		}, nil
	}
	var err error
	if p.Collection, err = plan("ZVMCollection", SaltCollection); err != nil {
		return nil, err
	}
	if p.Exchange, err = plan("ZVMExchange", SaltExchange, r.Owner, r.Owner, big.NewInt(r.FeeBP)); err != nil {
		return nil, err
	}
	if p.Factory, err = plan("ZVMCollectionFactory", SaltFactory, p.Collection.Address); err != nil {
		return nil, err
	}
	if p.Farm, err = plan("ZVMFarm", SaltFarm, r.RewardToken, r.RewardRate, r.Owner); err != nil {
		return nil, err
	}
	if r.Router != (common.Address{}) && r.SwapFactory != (common.Address{}) && r.WETH != (common.Address{}) {
		if p.FeeStrategy, err = PlanFeeStrategy(arts, p.Farm.Address, r.Router, r.SwapFactory, r.WETH); err != nil {
			return nil, err
		}
		p.HasFeeStrategy = true
	}
	return p, nil
}

// PlanFeeStrategy computes the fee strategy's address from the four contracts it
// reads. It is separate from BuildPlan because a chain whose farm is already
// deployed can be given a strategy without reproducing the recipe that produced
// that farm: the farm's own address is the only part of the recipe the strategy
// depends on, and it can simply be read from the deployment.
func PlanFeeStrategy(arts *bench.Artifacts, farm, router, swapFactory, weth common.Address) (Planned, error) {
	art, err := arts.Get("ZVMFarmFeeStrategy")
	if err != nil {
		return Planned{}, err
	}
	init, err := art.Deploy(farm, router, swapFactory, weth)
	if err != nil {
		return Planned{}, err
	}
	return Planned{
		Name: "ZVMFarmFeeStrategy", Salt: SaltFeeStrategy, InitCode: init,
		Address: rollup.Create2Address(rollup.Create2Deployer, SaltFeeStrategy, init),
	}, nil
}

// Matches reports whether a deployment file names the addresses this plan
// computes. A file that does not is either for another recipe or somebody
// else's marketplace, and the node says so rather than believing it.
func (p *Plan) Matches(d *Deployment) bool {
	if d == nil ||
		d.CollectionImpl != p.Collection.Address ||
		d.Exchange != p.Exchange.Address ||
		d.CollectionFactory != p.Factory.Address ||
		d.Farm != p.Farm.Address {
		return false
	}
	// A deployment written before the fee strategy existed names none, and is
	// still this marketplace: the strategy moves no value that the other four
	// contracts depend on, so its absence is a missing feature, not a mismatch.
	if d.FeeStrategy != (common.Address{}) && p.HasFeeStrategy && d.FeeStrategy != p.FeeStrategy.Address {
		return false
	}
	return true
}

// Deployed reports which of the planned contracts already have code, so a
// setup can be re-run without redeploying anything. This is the property
// CREATE2 buys: the address does not move, so "already there" is answerable.
func (p *Plan) Deployed(ctx context.Context, eth *ethclient.Client) (map[common.Address]bool, error) {
	out := map[common.Address]bool{}
	for _, c := range p.All() {
		code, err := eth.CodeAt(ctx, c.Address, nil)
		if err != nil {
			return nil, err
		}
		out[c.Address] = len(code) > 0
	}
	return out, nil
}
