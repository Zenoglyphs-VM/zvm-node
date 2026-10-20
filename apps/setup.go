package apps

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/Zenoglyphs-VM/zvm-node/media"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// Deploying and seeding the application layer, through both lanes: the EVM
// funder posts through the relayer, and a Zenon key creates its own collection
// and sells out of it with nothing but account blocks — which is the claim the
// marketplace is built around.

type SetupConfig struct {
	Rollup  *rollup.Config
	Eth     *ethclient.Client
	Submit  *ethclient.Client
	L1      *znn.Client
	APIBase string
	Dex     *bench.Deployment
	// RewardQSR is the farm's whole budget, in whole QSR, spread over FarmDays.
	RewardQSR int64
	FarmDays  int64
	// PoolZNN is how much ZNN to put in the new ZNN/QSR pool.
	PoolZNN int64
	// FundZNN is the ZNN the deployer is topped up to through the vault, in whole
	// units. It was 100,000 and unconditional, which is the right size only on a
	// chain genesised with `zvm l1-genesis`; on a chain funded by hand it asks the
	// bench funder for more than it holds. Zero means that old default.
	FundZNN int64
	SkipNFT bool
	// OnDeployed is called once the contracts exist and before anything is
	// seeded, so the node can be told where the exchange is before it is
	// handed orders for it.
	OnDeployed func(*Deployment) error
	Log        *slog.Logger
}

const (
	oneZNN  = 1e18
	deadArt = "a glyph drawn from the hash of its own name"
)

type session struct {
	cfg  SetupConfig
	arts *bench.Artifacts
	send *bench.Sender
	log  *slog.Logger
}

func Setup(ctx context.Context, cfg SetupConfig) (*Deployment, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.RewardQSR == 0 {
		cfg.RewardQSR = 100_000
	}
	if cfg.FarmDays == 0 {
		cfg.FarmDays = 30
	}
	if cfg.PoolZNN == 0 {
		cfg.PoolZNN = 10_000
	}
	if cfg.FundZNN == 0 {
		cfg.FundZNN = 100_000
	}
	arts, err := LoadArtifacts()
	if err != nil {
		return nil, err
	}
	key := devnet.EVMKey(bench.FunderEVMKey)
	from := crypto.PubkeyToAddress(key.PublicKey)
	if err := fundFromVault(ctx, cfg, from, cfg.FundZNN); err != nil {
		return nil, err
	}
	sender, err := bench.NewSender(ctx, key, cfg.Eth, cfg.Submit, cfg.Rollup)
	if err != nil {
		return nil, err
	}
	x := &session{cfg: cfg, arts: arts, send: sender, log: cfg.Log}

	d := &Deployment{
		ChainID: cfg.Rollup.L2ChainID, Deployer: from, RewardToken: rollup.QSRTokenAddr,
		Deployed: time.Now().UTC(),
	}

	// The reward rate is fixed at construction; the budget then decides when
	// emission ends, and funding moves that end.
	rewardUnits := qsrUnits(cfg.RewardQSR)

	// Every application contract goes through the deterministic deployer that
	// is already in genesis, at an address computed from published values
	// (D32). Running this twice deploys nothing the second time.
	recipe := DefaultRecipe(from, cfg.RewardQSR, cfg.FarmDays)
	if cfg.Dex != nil {
		recipe.Router, recipe.SwapFactory, recipe.WETH = cfg.Dex.Router, cfg.Dex.Factory, cfg.Dex.WETH
	}
	plan, err := BuildPlan(arts, recipe)
	if err != nil {
		return nil, err
	}
	d.CollectionImpl, d.Exchange = plan.Collection.Address, plan.Exchange.Address
	d.CollectionFactory, d.Farm = plan.Factory.Address, plan.Farm.Address
	if plan.HasFeeStrategy {
		d.FeeStrategy = plan.FeeStrategy.Address
	}
	d.Salts = map[string]string{
		"collection": SaltCollection.Hex(), "exchange": SaltExchange.Hex(),
		"collectionFactory": SaltFactory.Hex(), "farm": SaltFarm.Hex(),
	}
	if plan.HasFeeStrategy {
		d.Salts["farmFeeStrategy"] = SaltFeeStrategy.Hex()
	}
	if cfg.Dex != nil {
		d.WrappedZNN = cfg.Dex.WETH
	}
	d.Owner, d.RewardRate = from, plan.Recipe.RewardRate.String()
	if err := x.deployPlan(ctx, plan); err != nil {
		return nil, err
	}
	if cfg.OnDeployed != nil {
		if err := cfg.OnDeployed(d); err != nil {
			return nil, err
		}
	}

	// The farm's budget, plus enough for the ZNN/QSR pool only if that pool still
	// needs liquidity. Asking for the pool's share unconditionally is what made a
	// second run — or a `dex seed` that had already opened the pool — demand QSR
	// the bench funder does not have.
	want := new(big.Int).Set(rewardUnits)
	if _, liquid, err := x.znnQSRPoolState(ctx); err != nil {
		return nil, err
	} else if !liquid {
		want.Add(want, qsrUnits(x.cfg.PoolZNN*10)) // an upper bound; the ratio is applied later
	}
	if err := x.seedQSR(ctx, from, want); err != nil {
		return nil, err
	}
	if d.ZNNQSRPair, err = x.znnQSRPool(ctx, from); err != nil {
		return nil, err
	}
	if err := x.seedFarm(ctx, d, rewardUnits); err != nil {
		return nil, err
	}
	if plan.HasFeeStrategy {
		if err := x.armFeeStrategy(ctx, d); err != nil {
			return nil, err
		}
	}
	if !cfg.SkipNFT {
		collections, err := x.seedMarket(ctx, d)
		if err != nil {
			return nil, err
		}
		d.Collections = collections
	}
	return d, nil
}

// fundFromVault brings the deployer's ZNN in the way everyone's comes in: a
// send to the vault, which derivation mints on this side.
func fundFromVault(ctx context.Context, cfg SetupConfig, to common.Address, whole int64) error {
	want := new(big.Int).Mul(big.NewInt(whole), big.NewInt(oneZNN))
	balance, err := cfg.Eth.BalanceAt(ctx, to, nil)
	if err != nil {
		return err
	}
	if balance.Cmp(want) >= 0 {
		return nil
	}
	cfg.Log.Info("funding the deployer through the vault", "address", to, "znn", whole)
	acct := znn.NewAccount(devnet.Key(devnet.BenchFunder), cfg.L1, cfg.Rollup.L1ChainID)
	acct.AssumeFused = true
	units := new(big.Int).Mul(big.NewInt(whole), znn.OneZNN)
	call := derive.Call{To: &to, Value: new(big.Int).Mul(units, rollup.ZNNScale), Gas: 30_000}
	blk, err := acct.Send(ctx, cfg.Rollup.Vault, znn.ZNN, units, derive.EncodeCall(call))
	if err != nil {
		return fmt.Errorf("funding the deployer: %w", err)
	}
	_, err = bench.WaitReceipt(ctx, cfg.Eth, derive.DepositCallTx(cfg.Rollup, blk, call), 3*time.Minute)
	return err
}

func qsrUnits(whole int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(whole), big.NewInt(1e8))
}

// armFeeStrategy points the Uniswap factory's feeTo at the strategy and gives it
// the dust LP position it needs to work.
//
// Two facts make both steps necessary. A pair diverts one sixth of its 0.30% fee
// only when the factory names a feeTo, so without setFeeTo the strategy is inert.
// And a pair mints that share only inside mint and burn, so the strategy realises
// it by burning a position of its own — which it has to be given once, because
// the first burn is what mints the first fee.
//
// The seed is sized from the pool's own reserves rather than from a constant: the
// binding constraint is that burning it must return a non-zero amount of *both*
// tokens, and with QSR at 8 decimals against wrapped ZNN at 18 a seed that looks
// like dust on one side rounds to zero on the other.
func (x *session) armFeeStrategy(ctx context.Context, d *Deployment) error {
	if d.FeeStrategy == (common.Address{}) || x.cfg.Dex == nil {
		return nil
	}
	feeTo, err := x.read(ctx, x.cfg.Dex.Factory, "UniswapV2Factory", "feeTo")
	if err != nil {
		return err
	}
	if feeTo[0].(common.Address) != d.FeeStrategy {
		// feeToSetter is the deployer, which is this sender.
		if _, err := x.call(ctx, x.cfg.Dex.Factory, "UniswapV2Factory", "setFeeTo", nil, 80_000, d.FeeStrategy); err != nil {
			return fmt.Errorf("setting the Uniswap feeTo to the strategy: %w", err)
		}
		x.log.Info("swap fees now accrue to the farm", "feeTo", d.FeeStrategy, "share", "1/6 of 0.30%")
	}

	pair := d.ZNNQSRPair
	if pair == (common.Address{}) {
		x.log.Warn("no ZNN/QSR pool: the fee strategy is armed but has nothing to seed against")
		return nil
	}
	held, err := x.read(ctx, pair, "UniswapV2Pair", "balanceOf", d.FeeStrategy)
	if err != nil {
		return err
	}
	if held[0].(*big.Int).Sign() > 0 {
		x.log.Info("the fee strategy already holds a seed position", "pair", pair)
		return nil
	}

	reserves, err := x.read(ctx, pair, "UniswapV2Pair", "getReserves")
	if err != nil {
		return err
	}
	supply, err := x.read(ctx, pair, "UniswapV2Pair", "totalSupply")
	if err != nil {
		return err
	}
	token0, err := x.read(ctx, pair, "UniswapV2Pair", "token0")
	if err != nil {
		return err
	}
	r0 := new(big.Int).Set(reserves[0].(*big.Int))
	r1 := new(big.Int).Set(reserves[1].(*big.Int))
	total := supply[0].(*big.Int)

	// One ten-thousandth of the pool, floored at whatever makes the smaller side
	// survive a round trip: liquidity*reserve/totalSupply must exceed zero for
	// both tokens, so the seed is at least totalSupply/min(reserve) plus slack.
	amount0 := new(big.Int).Div(r0, big.NewInt(10_000))
	amount1 := new(big.Int).Div(r1, big.NewInt(10_000))
	for _, side := range []struct {
		amount  *big.Int
		reserve *big.Int
	}{{amount0, r0}, {amount1, r1}} {
		// liquidity minted is amount*totalSupply/reserve; ask for at least 1,000
		// so the burn's own division has room.
		min := new(big.Int).Div(new(big.Int).Mul(big.NewInt(1_000), side.reserve), total)
		min.Add(min, big.NewInt(1))
		if side.amount.Cmp(min) < 0 {
			side.amount.Set(min)
		}
	}
	if amount0.Sign() == 0 || amount1.Sign() == 0 {
		x.log.Warn("the ZNN/QSR pool is too shallow to seed the fee strategy", "pair", pair)
		return nil
	}

	token1, err := x.read(ctx, pair, "UniswapV2Pair", "token1")
	if err != nil {
		return err
	}

	// The strategy pulls both sides from whoever calls seed, and by this point the
	// deployer holds neither: the pool took its wrapped ZNN and the farm took its
	// QSR. So each side is provisioned the way it arrives in the first place —
	// QSR over the bridge, wrapped ZNN by wrapping some of what the deployer has.
	for _, t := range []struct {
		token  common.Address
		amount *big.Int
	}{{token0[0].(common.Address), amount0}, {token1[0].(common.Address), amount1}} {
		switch t.token {
		case rollup.QSRTokenAddr:
			if err := x.seedQSR(ctx, x.send.From, t.amount); err != nil {
				return fmt.Errorf("bringing QSR for the strategy's seed: %w", err)
			}
		case x.cfg.Dex.WETH:
			held, err := x.read(ctx, t.token, "BenchERC20", "balanceOf", x.send.From)
			if err != nil {
				return err
			}
			if short := new(big.Int).Sub(t.amount, held[0].(*big.Int)); short.Sign() > 0 {
				if _, err := x.call(ctx, t.token, "WZNN", "deposit", short, 80_000); err != nil {
					return fmt.Errorf("wrapping ZNN for the strategy's seed: %w", err)
				}
			}
		default:
			return fmt.Errorf("the ZNN/QSR pool holds %s, which is neither QSR nor wrapped ZNN", t.token)
		}
		if _, err := x.call(ctx, t.token, "BenchERC20", "approve", nil, 80_000, d.FeeStrategy, t.amount); err != nil {
			return err
		}
	}
	r, err := x.call(ctx, d.FeeStrategy, "ZVMFarmFeeStrategy", "seed", nil, 400_000, pair, amount0, amount1)
	if err != nil {
		return fmt.Errorf("seeding the fee strategy: %w", err)
	}
	x.log.Info("fee strategy seeded", "pair", pair, "amount0", amount0, "amount1", amount1, "block", r.BlockNumber)
	return nil
}

// deployPlan sends each application contract through the deterministic
// deployer, skipping any that is already there — which, because the address
// does not move, is a question that can be asked (D32).
func (x *session) deployPlan(ctx context.Context, plan *Plan) error {
	have, err := plan.Deployed(ctx, x.cfg.Eth)
	if err != nil {
		return err
	}
	for _, round := range plan.Rounds() {
		if err := x.deployRound(ctx, round, have); err != nil {
			return err
		}
	}
	return nil
}

// deployRound sends one round's contracts together, so they land in one block,
// and waits for all of them.
func (x *session) deployRound(ctx context.Context, round []Planned, have map[common.Address]bool) error {
	var sent []*types.Transaction
	var pending []Planned
	for _, c := range round {
		if have[c.Address] {
			x.log.Info("already deployed", "contract", c.Name, "address", c.Address)
			continue
		}
		to := rollup.Create2Deployer
		tx, err := x.send.Send(ctx, &to, new(big.Int), c.Calldata(), 0)
		if err != nil {
			return fmt.Errorf("deploying %s: %w", c.Name, err)
		}
		sent, pending = append(sent, tx), append(pending, c)
	}
	for i, tx := range sent {
		r, err := bench.WaitReceipt(ctx, x.cfg.Eth, tx, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("deploying %s: %w", pending[i].Name, err)
		}
		code, err := x.cfg.Eth.CodeAt(ctx, pending[i].Address, nil)
		if err != nil {
			return err
		}
		if len(code) == 0 {
			return fmt.Errorf("%s did not land at %s: the deterministic deployer ran but left no code",
				pending[i].Name, pending[i].Address)
		}
		x.log.Info("deployed", "contract", pending[i].Name, "address", pending[i].Address,
			"salt", pending[i].Salt, "gas", r.GasUsed, "block", r.BlockNumber)
	}
	return nil
}

func (x *session) deploy(ctx context.Context, name string, args ...any) (common.Address, error) {
	init, err := x.arts.MustGet(name).Deploy(args...)
	if err != nil {
		return common.Address{}, err
	}
	tx, err := x.send.Send(ctx, nil, new(big.Int), init, 0)
	if err != nil {
		return common.Address{}, fmt.Errorf("deploying %s: %w", name, err)
	}
	r, err := bench.WaitReceipt(ctx, x.cfg.Eth, tx, 5*time.Minute)
	if err != nil {
		return common.Address{}, fmt.Errorf("deploying %s: %w", name, err)
	}
	x.log.Info("deployed", "contract", name, "address", r.ContractAddress, "gas", r.GasUsed, "block", r.BlockNumber)
	return r.ContractAddress, nil
}

// call sends a contract call from the EVM funder and waits for its receipt.
func (x *session) call(ctx context.Context, to common.Address, artifact, method string, value *big.Int, gas uint64, args ...any) (*types.Receipt, error) {
	data, err := x.arts.MustGet(artifact).Call(method, args...)
	if err != nil {
		return nil, err
	}
	tx, err := x.send.Send(ctx, &to, value, data, gas)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", artifact, method, err)
	}
	r, err := bench.WaitReceipt(ctx, x.cfg.Eth, tx, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", artifact, method, err)
	}
	return r, nil
}

func (x *session) read(ctx context.Context, to common.Address, artifact, method string, args ...any) ([]any, error) {
	art := x.arts.MustGet(artifact)
	data, err := art.Call(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := x.cfg.Eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", artifact, method, err)
	}
	return art.ABI.Unpack(method, out)
}

// native publishes a call as a Zenon account block, the lane a Syrius user is
// on: no EVM signature, and the gas is whatever the block's plasma buys.
func (x *session) native(ctx context.Context, key *znn.KeyPair, to *common.Address, value *big.Int, gas uint64, data []byte) (*types.Receipt, error) {
	acct := znn.NewAccount(key, x.cfg.L1, x.cfg.Rollup.L1ChainID)
	acct.AssumeFused = true
	call := fitNative(x.cfg.Rollup, derive.Call{To: to, Value: value, Data: data}, gas)
	_, tx, err := derive.PublishCall(ctx, x.cfg.Rollup, acct, znn.ZNN, call)
	if err != nil {
		return nil, err
	}
	return bench.WaitReceipt(ctx, x.cfg.Eth, tx, 5*time.Minute)
}

// fitNative sizes a native call the way the swap page does: a native call's gas
// is what its NoM block's plasma bought, so a call that needs more gets zero
// bytes appended to its calldata — which a Solidity function ignores — until
// the block pays for it.
func fitNative(cfg *rollup.Config, call derive.Call, want uint64) derive.Call {
	const basePlasma, bytePlasma = 21_000, 68
	out := call
	out.Gas = want
	if want > cfg.MaxNativeGas {
		out.Gas = cfg.MaxNativeGas
		want = cfg.MaxNativeGas
	}
	for i := 0; i < 8; i++ {
		plasma := uint64(basePlasma + bytePlasma*len(derive.EncodeCall(out)))
		if plasma*cfg.GasPerPlasma >= want {
			return out
		}
		step := cfg.GasPerPlasma * bytePlasma
		missing := (want - plasma*cfg.GasPerPlasma + step - 1) / step
		out.Data = append(out.Data, make([]byte, missing+2)...)
	}
	return out
}

func (x *session) qsrBalance(ctx context.Context, who common.Address) (*big.Int, error) {
	out, err := x.read(ctx, rollup.QSRTokenAddr, "BenchERC20", "balanceOf", who)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// seedQSR brings QSR onto the ZVM and into the deployer's hands. A QSR deposit
// credits the Zenon account that sent it — there is no "to" on that path — so
// the QSR arrives at a dev account's own 20 bytes and is moved from there with
// a native call, which is the same trip a Syrius user's QSR makes.
func (x *session) seedQSR(ctx context.Context, to common.Address, want *big.Int) error {
	have, err := x.qsrBalance(ctx, to)
	if err != nil {
		return err
	}
	if have.Cmp(want) >= 0 {
		return nil
	}
	missing := new(big.Int).Sub(want, have)
	key := devnet.Key(devnet.BenchFunder)
	zenonSide := common.Address(key.Address)
	onZVM, err := x.qsrBalance(ctx, zenonSide)
	if err != nil {
		return err
	}
	if onZVM.Cmp(missing) < 0 {
		deposit := new(big.Int).Sub(missing, onZVM)
		x.log.Info("depositing QSR through the vault", "qsr", new(big.Int).Div(deposit, big.NewInt(1e8)), "from", key.Address)
		acct := znn.NewAccount(key, x.cfg.L1, x.cfg.Rollup.L1ChainID)
		acct.AssumeFused = true
		if _, err := acct.Send(ctx, x.cfg.Rollup.Vault, znn.QSR, deposit, nil); err != nil {
			return fmt.Errorf("depositing QSR: %w", err)
		}
		// A deposit is derived, not signed for: wait for the mint to appear.
		deadline := time.Now().Add(3 * time.Minute)
		for {
			onZVM, err = x.qsrBalance(ctx, zenonSide)
			if err != nil {
				return err
			}
			if onZVM.Cmp(missing) >= 0 {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the QSR deposit was not derived within three minutes")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	data, err := x.arts.MustGet("BenchERC20").Call("transfer", to, missing)
	if err != nil {
		return err
	}
	r, err := x.native(ctx, key, &rollup.QSRTokenAddr, nil, 80_000, data)
	if err != nil {
		return fmt.Errorf("moving QSR to the deployer: %w", err)
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("the native QSR transfer reverted in block %d", r.BlockNumber)
	}
	x.log.Info("QSR moved to the deployer over the native lane", "block", r.BlockNumber, "gasUsed", r.GasUsed)
	return nil
}

// qsrPerZNN asks this node what a ZNN and a QSR are worth, so the pool opens at
// the price the bridged pair trades at on Ethereum rather than at a guess.
func (x *session) qsrPerZNN(ctx context.Context) float64 {
	const fallback = 7.2
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(x.cfg.APIBase, "/")+"/api/prices", nil)
	if err != nil {
		return fallback
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	var body struct {
		Assets map[string]struct {
			USD float64 `json:"usd"`
		} `json:"assets"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil {
		return fallback
	}
	znnUSD, qsrUSD := body.Assets["ZNN"].USD, body.Assets["QSR"].USD
	if znnUSD <= 0 || qsrUSD <= 0 {
		return fallback
	}
	return znnUSD / qsrUSD
}

// znnQSRPool opens the pool the ZVM did not have: ZNN against the QSR the
// bridge mints, at the ratio the wrapped pair trades at on Ethereum.
func (x *session) znnQSRPool(ctx context.Context, from common.Address) (common.Address, error) {
	if x.cfg.Dex == nil || x.cfg.Dex.Router == (common.Address{}) {
		x.log.Warn("no Uniswap deployment: skipping the ZNN/QSR pool")
		return common.Address{}, nil
	}
	pair, liquid, err := x.znnQSRPoolState(ctx)
	if err != nil {
		return common.Address{}, err
	}
	if liquid {
		x.log.Info("ZNN/QSR pool already has liquidity", "pair", pair)
		return pair, nil
	}
	ratio := x.qsrPerZNN(ctx)
	znnAmount := new(big.Int).Mul(big.NewInt(x.cfg.PoolZNN), big.NewInt(oneZNN))
	qsrAmount := qsrUnits(int64(float64(x.cfg.PoolZNN) * ratio))
	x.log.Info("opening the ZNN/QSR pool", "znn", x.cfg.PoolZNN, "qsrPerZnn", fmt.Sprintf("%.4f", ratio))
	if _, err := x.call(ctx, rollup.QSRTokenAddr, "BenchERC20", "approve", nil, 80_000, x.cfg.Dex.Router, qsrAmount); err != nil {
		return common.Address{}, err
	}
	// Exact minimums: a new pool takes the ratio it is given.
	if _, err := x.call(ctx, x.cfg.Dex.Router, "UniswapV2Router02", "addLiquidityETH", znnAmount, 4_000_000,
		rollup.QSRTokenAddr, qsrAmount, qsrAmount, znnAmount, from, deadline()); err != nil {
		return common.Address{}, err
	}
	out, err := x.read(ctx, x.cfg.Dex.Factory, "UniswapV2Factory", "getPair", rollup.QSRTokenAddr, x.cfg.Dex.WETH)
	if err != nil {
		return common.Address{}, err
	}
	pair = out[0].(common.Address)
	x.log.Info("ZNN/QSR pool", "pair", pair, "znn", x.cfg.PoolZNN, "qsr", new(big.Int).Div(qsrAmount, big.NewInt(1e8)))
	return pair, nil
}

// znnQSRPoolState finds the ZNN/QSR pair, if the factory has one, and says
// whether it already holds liquidity. Both the seeding amount and the decision
// to open the pool at all hang on it, and `dex seed` may have opened it already.
func (x *session) znnQSRPoolState(ctx context.Context) (common.Address, bool, error) {
	if x.cfg.Dex == nil || x.cfg.Dex.Factory == (common.Address{}) {
		return common.Address{}, false, nil
	}
	out, err := x.read(ctx, x.cfg.Dex.Factory, "UniswapV2Factory", "getPair", rollup.QSRTokenAddr, x.cfg.Dex.WETH)
	if err != nil {
		return common.Address{}, false, err
	}
	pair := out[0].(common.Address)
	if pair == (common.Address{}) {
		return pair, false, nil
	}
	reserves, err := x.read(ctx, pair, "UniswapV2Pair", "getReserves")
	if err != nil {
		return pair, false, err
	}
	return pair, reserves[0].(*big.Int).Sign() > 0, nil
}

// SeedZNNQSRPool brings QSR onto the ZVM and opens the ZNN/QSR pool, without
// deploying or seeding anything else. `zvm dex seed` calls it so that the swap
// page has the pair a holder of ZNN and QSR actually wants, rather than waiting
// for `apps setup` to get to it; running both is safe, because a pool that
// already has liquidity is left alone.
func SeedZNNQSRPool(ctx context.Context, cfg SetupConfig) (common.Address, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PoolZNN == 0 {
		cfg.PoolZNN = 10_000
	}
	if cfg.Dex == nil || cfg.Dex.Router == (common.Address{}) {
		return common.Address{}, fmt.Errorf("no Uniswap deployment: run `zvm bench setup` first")
	}
	arts, err := LoadArtifacts()
	if err != nil {
		return common.Address{}, err
	}
	key := devnet.EVMKey(bench.FunderEVMKey)
	from := crypto.PubkeyToAddress(key.PublicKey)
	sender, err := bench.NewSender(ctx, key, cfg.Eth, cfg.Submit, cfg.Rollup)
	if err != nil {
		return common.Address{}, err
	}
	x := &session{cfg: cfg, arts: arts, send: sender, log: cfg.Log}
	if _, liquid, err := x.znnQSRPoolState(ctx); err != nil {
		return common.Address{}, err
	} else if !liquid {
		if err := x.seedQSR(ctx, from, qsrUnits(cfg.PoolZNN*10)); err != nil {
			return common.Address{}, err
		}
	}
	return x.znnQSRPool(ctx, from)
}

func deadline() *big.Int { return big.NewInt(time.Now().Add(time.Hour).Unix()) }

func (x *session) seedFarm(ctx context.Context, d *Deployment, reward *big.Int) error {
	if _, err := x.call(ctx, rollup.QSRTokenAddr, "BenchERC20", "approve", nil, 80_000, d.Farm, reward); err != nil {
		return err
	}
	r, err := x.call(ctx, d.Farm, "ZVMFarm", "fund", nil, 200_000, reward)
	if err != nil {
		return err
	}
	x.log.Info("farm funded", "qsr", new(big.Int).Div(reward, big.NewInt(1e8)), "days", x.cfg.FarmDays, "block", r.BlockNumber)

	type farmPool struct {
		name  string
		token common.Address
		alloc uint64
	}
	pools := []farmPool{}
	if d.ZNNQSRPair != (common.Address{}) {
		pools = append(pools, farmPool{"ZNN/QSR", d.ZNNQSRPair, 60})
	}
	if x.cfg.Dex != nil {
		for _, p := range []struct {
			name  string
			token common.Address
			alloc uint64
		}{{"ZNN/BNA", x.cfg.Dex.ERC20, 25}, {"ZNN/BNB", x.cfg.Dex.TokenB, 15}} {
			out, err := x.read(ctx, x.cfg.Dex.Factory, "UniswapV2Factory", "getPair", p.token, x.cfg.Dex.WETH)
			if err != nil {
				return err
			}
			if pair := out[0].(common.Address); pair != (common.Address{}) {
				pools = append(pools, farmPool{p.name, pair, p.alloc})
			}
		}
	}
	for _, p := range pools {
		if _, err := x.call(ctx, d.Farm, "ZVMFarm", "addPool", nil, 300_000, p.token, p.alloc); err != nil {
			return fmt.Errorf("adding the %s farm pool: %w", p.name, err)
		}
		x.log.Info("farm pool", "pair", p.name, "lpToken", p.token, "allocPoint", p.alloc)
	}
	return nil
}

// --- the media store --------------------------------------------------------

type mediaClient struct {
	base  string
	token string
	http  *http.Client
}

type signer func(message string) media.SessionRequest

func evmSigner(key *ecdsa.PrivateKey) (string, signer) {
	address := crypto.PubkeyToAddress(key.PublicKey)
	return address.Hex(), func(message string) media.SessionRequest {
		sig, err := crypto.Sign(accounts.TextHash([]byte(message)), key)
		if err != nil {
			return media.SessionRequest{}
		}
		sig[64] += 27
		return media.SessionRequest{Address: address.Hex(), Message: message, Signature: hex.EncodeToString(sig)}
	}
}

func openMedia(ctx context.Context, base, address string, sign signer) (*mediaClient, error) {
	m := &mediaClient{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 60 * time.Second}}
	var challenge struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/media/challenge?address="+address, nil, "", &challenge); err != nil {
		return nil, err
	}
	if challenge.Message == "" {
		return nil, fmt.Errorf("media challenge: %s", challenge.Error)
	}
	body, err := json.Marshal(sign(challenge.Message))
	if err != nil {
		return nil, err
	}
	var session struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := m.do(ctx, http.MethodPost, "/api/media/session", body, "application/json", &session); err != nil {
		return nil, err
	}
	if session.Token == "" {
		return nil, fmt.Errorf("media session: %s", session.Error)
	}
	m.token = session.Token
	return m, nil
}

func (m *mediaClient) do(ctx context.Context, method, path string, body []byte, contentType string, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, reader)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(raw)))
		}
	}
	return nil
}

func (m *mediaClient) put(ctx context.Context, data []byte, contentType string) (string, error) {
	var o struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := m.do(ctx, http.MethodPost, "/api/media", data, contentType, &o); err != nil {
		return "", err
	}
	if o.URL == "" {
		return "", fmt.Errorf("media upload: %s", o.Error)
	}
	return o.URL, nil
}

func (m *mediaClient) putJSON(ctx context.Context, v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return m.put(ctx, raw, "application/json")
}

// --- the market demo ----------------------------------------------------------

type collectionSpec struct {
	name        string
	symbol      string
	description string
	items       int
	royaltyBps  uint64
	// native: created and sold from a Zenon key, with no EVM signature anywhere.
	native bool
}

func (x *session) seedMarket(ctx context.Context, d *Deployment) ([]common.Address, error) {
	funderKey := devnet.EVMKey(bench.FunderEVMKey)
	funder := crypto.PubkeyToAddress(funderKey.PublicKey)
	address, sign := evmSigner(funderKey)
	store, err := openMedia(ctx, x.cfg.APIBase, address, sign)
	if err != nil {
		return nil, fmt.Errorf("the node stores no media (start it with -media-dir): %w", err)
	}

	var out []common.Address
	collA, idsA, err := x.createCollection(ctx, d, store, funder, collectionSpec{
		name: "Zenon Momentums", symbol: "MOM", items: 12, royaltyBps: 500,
		description: "Twelve momentums, each " + deadArt + ".",
	})
	if err != nil {
		return nil, err
	}
	out = append(out, collA)

	// The second collection belongs to a Zenon account: created, minted, listed
	// and sold with account blocks alone. Uploads take an EVM wallet's
	// signature (D31), and media is stored by hash whoever uploads it, so the
	// funder's session carries its art.
	zkey := devnet.Key(devnet.Dev3)
	collB, idsB, err := x.createCollection(ctx, d, store, common.Address(zkey.Address), collectionSpec{
		name: "Plasma Shards", symbol: "SHARD", items: 6, royaltyBps: 250, native: true,
		description: "Six shards, minted by a Zenon account through the inbox.",
	})
	if err != nil {
		return nil, err
	}
	out = append(out, collB)

	if err := x.tradeNFTs(ctx, d, collA, idsA, collB, idsB, zkey); err != nil {
		return nil, err
	}
	return out, nil
}

// createCollection uploads the art and the metadata, creates the collection and
// mints into it — on the EVM lane, or entirely on the native one.
func (x *session) createCollection(ctx context.Context, d *Deployment, store *mediaClient, owner common.Address, spec collectionSpec) (common.Address, []*big.Int, error) {
	cover, err := NewGlyph(spec.name, 0).PNG(512)
	if err != nil {
		return common.Address{}, nil, err
	}
	coverURL, err := store.put(ctx, cover, "image/png")
	if err != nil {
		return common.Address{}, nil, err
	}
	contractURI, err := store.putJSON(ctx, map[string]any{
		"name": spec.name, "description": spec.description, "image": coverURL,
	})
	if err != nil {
		return common.Address{}, nil, err
	}

	uris := make([]string, 0, spec.items)
	for i := 1; i <= spec.items; i++ {
		g := NewGlyph(spec.name, i)
		art, err := g.PNG(512)
		if err != nil {
			return common.Address{}, nil, err
		}
		imageURL, err := store.put(ctx, art, "image/png")
		if err != nil {
			return common.Address{}, nil, err
		}
		uri, err := store.putJSON(ctx, g.Metadata(
			fmt.Sprintf("%s #%d", spec.name, i),
			fmt.Sprintf("%s — %s.", spec.name, deadArt),
			imageURL))
		if err != nil {
			return common.Address{}, nil, err
		}
		uris = append(uris, uri)
	}

	factoryArgs := []any{spec.name, spec.symbol, contractURI, "", big.NewInt(int64(spec.royaltyBps))}
	var collection common.Address
	if spec.native {
		data, err := x.arts.MustGet("ZVMCollectionFactory").Call("createCollection", factoryArgs...)
		if err != nil {
			return common.Address{}, nil, err
		}
		key := devnet.Key(devnet.Dev3)
		r, err := x.native(ctx, key, &d.CollectionFactory, nil, 900_000, data)
		if err != nil {
			return common.Address{}, nil, fmt.Errorf("creating %s natively: %w", spec.name, err)
		}
		if r.Status != types.ReceiptStatusSuccessful {
			return common.Address{}, nil, fmt.Errorf("creating %s natively reverted in block %d", spec.name, r.BlockNumber)
		}
		collection, err = x.lastCollection(ctx, d)
		if err != nil {
			return common.Address{}, nil, err
		}
		x.log.Info("collection created on the native lane", "name", spec.name, "address", collection,
			"owner", key.Address, "gasUsed", r.GasUsed, "block", r.BlockNumber)
		// Three at a time: a native call's gas is bounded by its block's plasma.
		for i := 0; i < len(uris); i += 3 {
			batch := uris[i:min(i+3, len(uris))]
			data, err := x.arts.MustGet("ZVMCollection").Call("mintBatch", common.Address(key.Address), batch)
			if err != nil {
				return common.Address{}, nil, err
			}
			r, err := x.native(ctx, key, &collection, nil, 800_000, data)
			if err != nil {
				return common.Address{}, nil, err
			}
			if r.Status != types.ReceiptStatusSuccessful {
				return common.Address{}, nil, fmt.Errorf("minting %s reverted in block %d", spec.name, r.BlockNumber)
			}
		}
	} else {
		r, err := x.call(ctx, d.CollectionFactory, "ZVMCollectionFactory", "createCollection", nil, 600_000, factoryArgs...)
		if err != nil {
			return common.Address{}, nil, err
		}
		collection, err = x.lastCollection(ctx, d)
		if err != nil {
			return common.Address{}, nil, err
		}
		x.log.Info("collection created", "name", spec.name, "address", collection, "gas", r.GasUsed, "block", r.BlockNumber)
		if _, err := x.call(ctx, collection, "ZVMCollection", "mintBatch", nil, uint64(200_000*len(uris)), owner, uris); err != nil {
			return common.Address{}, nil, err
		}
	}

	ids := make([]*big.Int, 0, spec.items)
	for i := 1; i <= spec.items; i++ {
		ids = append(ids, big.NewInt(int64(i)))
	}
	x.log.Info("minted", "collection", spec.name, "items", spec.items, "owner", owner)
	return collection, ids, nil
}

func (x *session) lastCollection(ctx context.Context, d *Deployment) (common.Address, error) {
	out, err := x.read(ctx, d.CollectionFactory, "ZVMCollectionFactory", "collectionsLength")
	if err != nil {
		return common.Address{}, err
	}
	n := out[0].(*big.Int)
	if n.Sign() == 0 {
		return common.Address{}, fmt.Errorf("the factory created nothing")
	}
	last, err := x.read(ctx, d.CollectionFactory, "ZVMCollectionFactory", "collections", new(big.Int).Sub(n, big.NewInt(1)))
	if err != nil {
		return common.Address{}, err
	}
	return last[0].(common.Address), nil
}

// orderTimes opens an order a few minutes in the past, so that a momentum
// timestamp slightly behind this machine's clock does not read it as not yet
// open, and closes it in thirty days.
func orderTimes() (uint64, uint64) {
	now := time.Now()
	return uint64(now.Add(-5 * time.Minute).Unix()), uint64(now.Add(30 * 24 * time.Hour).Unix())
}

// traderNonce is the exchange's nonce for a trader: an order signed with any
// other is already cancelled.
func (x *session) traderNonce(ctx context.Context, exchange, trader common.Address) (*big.Int, error) {
	out, err := x.read(ctx, exchange, "ZVMExchange", "nonces", trader)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func salt() *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256([]byte(fmt.Sprint(time.Now().UnixNano()))))
}

// tradeNFTs leaves the market with history, every kind of order D28 allows:
// a bulk listing (five items, one signature), listings a Zenon account agreed
// to with account blocks, purchases of both, a bid a Zenon account sells into,
// and a collection bid left standing.
func (x *session) tradeNFTs(ctx context.Context, d *Deployment, collA common.Address, idsA []*big.Int,
	collB common.Address, idsB []*big.Int, zkey *znn.KeyPair) error {
	if d.WrappedZNN == (common.Address{}) {
		return fmt.Errorf("bids are paid in wrapped ZNN: run `zvm bench setup` first so there is one")
	}
	domain := orderbook.Domain(x.cfg.Rollup.L2ChainID, d.Exchange)
	opens, expires := orderTimes()
	funderKey := devnet.EVMKey(bench.FunderEVMKey)
	funder := crypto.PubkeyToAddress(funderKey.PublicKey)
	zeno := common.Address(zkey.Address)

	// The deployer lets the exchange move its tokens, then lists five with one
	// signature over their Merkle root — Blur's bulk listing.
	if _, err := x.call(ctx, collA, "ZVMCollection", "setApprovalForAll", nil, 80_000, d.Exchange, true); err != nil {
		return err
	}
	nonce, err := x.traderNonce(ctx, d.Exchange, funder)
	if err != nil {
		return err
	}
	var listings []*orderbook.Order
	for i, whole := range []int64{5, 8, 12, 25, 40} {
		listings = append(listings, &orderbook.Order{
			Trader: funder, Side: orderbook.SideSell, Collection: collA, TokenID: orderbook.B(idsA[i]),
			Amount: orderbook.U(1), Price: orderbook.B(znnWei(whole)),
			ListingTime: opens, ExpirationTime: expires, Nonce: orderbook.B(nonce), Salt: orderbook.B(salt()),
		})
	}
	bulk, err := orderbook.SignBulk(funderKey, domain, listings)
	if err != nil {
		return err
	}
	if _, err := orderbook.Post(ctx, x.cfg.APIBase, bulk); err != nil {
		return err
	}
	x.log.Info("listed with one signature", "collection", collA, "items", len(bulk), "root", bulk[0].Root)

	// The Zenon account has no EVM key, so it agrees to its listings with a
	// transaction — Seaport's validate — sent as account blocks. The node picks
	// the orders up from the exchange's log.
	approve, err := x.arts.MustGet("ZVMCollection").Call("setApprovalForAll", d.Exchange, true)
	if err != nil {
		return err
	}
	if _, err := x.native(ctx, zkey, &collB, nil, 80_000, approve); err != nil {
		return err
	}
	var native []*orderbook.Order
	for i, whole := range []int64{3, 15} {
		native = append(native, &orderbook.Order{
			Trader: zeno, Side: orderbook.SideSell, Collection: collB, TokenID: orderbook.B(idsB[i]),
			Amount: orderbook.U(1), Price: orderbook.B(znnWei(whole)),
			ListingTime: opens, ExpirationTime: expires, Nonce: orderbook.U(0), Salt: orderbook.B(salt()),
		})
	}
	data, err := x.arts.MustGet("ZVMExchange").Call("approveOrders", orderbook.ABIOrders(native))
	if err != nil {
		return err
	}
	r, err := x.native(ctx, zkey, &d.Exchange, nil, 250_000, data)
	if err != nil {
		return err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("the native approveOrders reverted in block %d", r.BlockNumber)
	}
	x.log.Info("listed on the native lane", "collection", collB, "items", len(native), "seller", zkey.Address, "gasUsed", r.GasUsed)

	// A second wallet buys one of each.
	buyerKey := devnet.EVMKey(bench.FunderEVMKey + 1)
	buyer := crypto.PubkeyToAddress(buyerKey.PublicKey)
	if err := x.fundEVM(ctx, buyer, 200); err != nil {
		return err
	}
	buyerSender, err := bench.NewSender(ctx, buyerKey, x.cfg.Eth, x.cfg.Submit, x.cfg.Rollup)
	if err != nil {
		return err
	}
	buyerSession := &session{cfg: x.cfg, arts: x.arts, send: buyerSender, log: x.log}
	onchain := &orderbook.Input{Order: *native[0], Kind: orderbook.KindOnChain, Signature: []byte{}, Proof: []common.Hash{}}
	for _, in := range []*orderbook.Input{bulk[0], onchain} {
		if _, err := buyerSession.call(ctx, d.Exchange, "ZVMExchange", "buy", in.Order.Price.Big(), 400_000, in.ABI()); err != nil {
			return fmt.Errorf("buying %s: %w", in.Order.Hash(), err)
		}
	}
	x.log.Info("bought", "buyer", buyer, "orders", 2)

	// A bid on one of the Zenon account's tokens, which it sells into natively.
	// Bids are paid in wrapped ZNN with an allowance, so the buyer wraps first.
	budget := znnWei(30)
	if _, err := buyerSession.call(ctx, d.WrappedZNN, "WZNN", "deposit", budget, 80_000); err != nil {
		return err
	}
	if _, err := buyerSession.call(ctx, d.WrappedZNN, "WZNN", "approve", nil, 80_000, d.Exchange, budget); err != nil {
		return err
	}
	buyerNonce, err := x.traderNonce(ctx, d.Exchange, buyer)
	if err != nil {
		return err
	}
	bid := &orderbook.Order{
		Trader: buyer, Side: orderbook.SideBuy, Collection: collB, TokenID: orderbook.B(idsB[2]),
		Amount: orderbook.U(1), PaymentToken: d.WrappedZNN, Price: orderbook.B(znnWei(9)),
		ListingTime: opens, ExpirationTime: expires, Nonce: orderbook.B(buyerNonce), Salt: orderbook.B(salt()),
	}
	signedBid, err := orderbook.Sign(buyerKey, domain, bid)
	if err != nil {
		return err
	}
	if _, err := orderbook.Post(ctx, x.cfg.APIBase, []*orderbook.Input{signedBid}); err != nil {
		return err
	}
	accept, err := x.arts.MustGet("ZVMExchange").Call("acceptBid", signedBid.ABI(), idsB[2])
	if err != nil {
		return err
	}
	r, err = x.native(ctx, zkey, &d.Exchange, nil, 400_000, accept)
	if err != nil {
		return err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("accepting the bid reverted in block %d", r.BlockNumber)
	}
	x.log.Info("bid accepted on the native lane", "seller", zkey.Address, "block", r.BlockNumber, "gasUsed", r.GasUsed)

	// And a collection-wide bid for two, left standing so the page has one.
	standing := &orderbook.Order{
		Trader: buyer, Side: orderbook.SideBuy, Collection: collA, TokenID: orderbook.U(0), AnyToken: true,
		Amount: orderbook.U(2), PaymentToken: d.WrappedZNN, Price: orderbook.B(znnWei(4)),
		ListingTime: opens, ExpirationTime: expires, Nonce: orderbook.B(buyerNonce), Salt: orderbook.B(salt()),
	}
	signedStanding, err := orderbook.Sign(buyerKey, domain, standing)
	if err != nil {
		return err
	}
	_, err = orderbook.Post(ctx, x.cfg.APIBase, []*orderbook.Input{signedStanding})
	return err
}

func znnWei(whole int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(whole), big.NewInt(oneZNN))
}

// ListMore puts more of what the deployer still holds on sale, so a demo market
// — or a test that buys from it — never runs dry. It deploys nothing, and it
// sends nothing either unless the exchange needs an approval: the listings are
// one signature, handed to the node.
func ListMore(ctx context.Context, cfg SetupConfig, d *Deployment, collection common.Address, count int, priceZNN float64) (int, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	arts, err := LoadArtifacts()
	if err != nil {
		return 0, err
	}
	key := devnet.EVMKey(bench.FunderEVMKey)
	from := crypto.PubkeyToAddress(key.PublicKey)
	sender, err := bench.NewSender(ctx, key, cfg.Eth, cfg.Submit, cfg.Rollup)
	if err != nil {
		return 0, err
	}
	x := &session{cfg: cfg, arts: arts, send: sender, log: cfg.Log}

	type collectionItem struct {
		TokenID string          `json:"tokenId"`
		Owner   string          `json:"owner"`
		URI     string          `json:"uri"`
		Listing json.RawMessage `json:"listing"`
	}
	var items []collectionItem
	url := fmt.Sprintf("%s/api/nft/collections/%s/items?limit=200", strings.TrimRight(cfg.APIBase, "/"), collection.Hex())
	readItems := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("reading the collection: %w", err)
		}
		defer resp.Body.Close()
		items = items[:0]
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&items); err != nil {
			return fmt.Errorf("reading the collection: %w", err)
		}
		return nil
	}
	if err := readItems(); err != nil {
		return 0, err
	}

	approved, err := x.read(ctx, collection, "ZVMCollection", "isApprovedForAll", from, d.Exchange)
	if err != nil {
		return 0, err
	}
	if !approved[0].(bool) {
		if _, err := x.call(ctx, collection, "ZVMCollection", "setApprovalForAll", nil, 80_000, d.Exchange, true); err != nil {
			return 0, err
		}
	}
	nonce, err := x.traderNonce(ctx, d.Exchange, from)
	if err != nil {
		return 0, err
	}

	// Mint what is missing rather than refuse. Everything minted here is held
	// by the deployer and sold from it, so a market seeded once and bought from
	// repeatedly — every e2e run buys one — would otherwise run dry, which is
	// exactly what this command exists to prevent.
	unlisted := func() int {
		n := 0
		for _, item := range items {
			if strings.EqualFold(item.Owner, from.Hex()) && len(item.Listing) == 0 {
				n++
			}
		}
		return n
	}
	minted := false
	if short := count - unlisted(); short > 0 {
		uri := ""
		for _, item := range items {
			if item.URI != "" {
				uri = item.URI
				break
			}
		}
		if uri == "" {
			return 0, fmt.Errorf("%s has no item to copy metadata from", collection)
		}
		uris := make([]string, short)
		for i := range uris {
			uris[i] = uri
		}
		if _, err := x.call(ctx, collection, "ZVMCollection", "mintBatch", nil, uint64(200_000*len(uris)), from, uris); err != nil {
			return 0, fmt.Errorf("minting %d more to list: %w", short, err)
		}
		x.log.Info("minted more to list", "collection", collection, "items", short, "owner", from)
		// The receipt is not the index: these listings are built from what the
		// node reports, which follows the derived block. "A moment later" is a
		// momentum when the chain is quiet and considerably more when it is
		// digesting a load run, so wait long enough for the busy case.
		deadline := time.Now().Add(3 * time.Minute)
		minted = true
		for {
			if err := readItems(); err != nil {
				return 0, err
			}
			if unlisted() >= count || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}

	price, _ := new(big.Float).Mul(big.NewFloat(priceZNN), big.NewFloat(oneZNN)).Int(nil)
	opens, expires := orderTimes()
	var orders []*orderbook.Order
	for _, item := range items {
		if len(orders) >= count {
			break
		}
		if !strings.EqualFold(item.Owner, from.Hex()) || len(item.Listing) > 0 {
			continue
		}
		id, ok := new(big.Int).SetString(item.TokenID, 10)
		if !ok {
			continue
		}
		orders = append(orders, &orderbook.Order{
			Trader: from, Side: orderbook.SideSell, Collection: collection, TokenID: orderbook.B(id),
			Amount: orderbook.U(1), Price: orderbook.B(price),
			ListingTime: opens, ExpirationTime: expires, Nonce: orderbook.B(nonce), Salt: orderbook.B(salt()),
		})
	}
	if len(orders) == 0 {
		if minted {
			return 0, fmt.Errorf("minted into %s but the node still lists nothing unlisted for the deployer — the index is behind the mint", collection)
		}
		return 0, fmt.Errorf("the deployer holds nothing unlisted in %s", collection)
	}
	signed, err := orderbook.SignBulk(key, orderbook.Domain(cfg.Rollup.L2ChainID, d.Exchange), orders)
	if err != nil {
		return 0, err
	}
	if _, err := orderbook.Post(ctx, cfg.APIBase, signed); err != nil {
		return 0, err
	}
	cfg.Log.Info("listed with one signature", "collection", collection, "items", len(signed), "znn", priceZNN)
	return len(signed), nil
}

func (x *session) fundEVM(ctx context.Context, to common.Address, whole int64) error {
	want := new(big.Int).Mul(big.NewInt(whole), big.NewInt(oneZNN))
	have, err := x.cfg.Eth.BalanceAt(ctx, to, nil)
	if err != nil {
		return err
	}
	if have.Cmp(want) >= 0 {
		return nil
	}
	tx, err := x.send.Send(ctx, &to, new(big.Int).Sub(want, have), nil, 30_000)
	if err != nil {
		return err
	}
	_, err = bench.WaitReceipt(ctx, x.cfg.Eth, tx, 3*time.Minute)
	return err
}

// EnableFeeStrategy gives a chain whose marketplace is already deployed the swap
// fee strategy, and nothing else.
//
// `apps setup` would do it as part of a full run, but a full run is not safe to
// repeat: ZVMFarm.addPool reverts on a pool it already has, so a second setup
// fails partway and a second fund would spend the bench funder's QSR again. This
// deploys the one contract that is missing, points the factory's feeTo at it, and
// seeds it — each step skipped if it has already happened.
func EnableFeeStrategy(ctx context.Context, cfg SetupConfig, d *Deployment) error {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Dex == nil || cfg.Dex.Router == (common.Address{}) {
		return fmt.Errorf("no Uniswap deployment: there are no swap fees to redirect")
	}
	if d.Farm == (common.Address{}) {
		return fmt.Errorf("the deployment names no farm")
	}
	arts, err := LoadArtifacts()
	if err != nil {
		return err
	}
	planned, err := PlanFeeStrategy(arts, d.Farm, cfg.Dex.Router, cfg.Dex.Factory, cfg.Dex.WETH)
	if err != nil {
		return err
	}
	key := devnet.EVMKey(bench.FunderEVMKey)
	sender, err := bench.NewSender(ctx, key, cfg.Eth, cfg.Submit, cfg.Rollup)
	if err != nil {
		return err
	}
	x := &session{cfg: cfg, arts: arts, send: sender, log: cfg.Log}

	code, err := cfg.Eth.CodeAt(ctx, planned.Address, nil)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		to := rollup.Create2Deployer
		tx, err := x.send.Send(ctx, &to, new(big.Int), planned.Calldata(), 0)
		if err != nil {
			return fmt.Errorf("deploying the fee strategy: %w", err)
		}
		r, err := bench.WaitReceipt(ctx, cfg.Eth, tx, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("deploying the fee strategy: %w", err)
		}
		if code, err = cfg.Eth.CodeAt(ctx, planned.Address, nil); err != nil {
			return err
		}
		if len(code) == 0 {
			return fmt.Errorf("the deterministic deployer ran but left no code at %s", planned.Address)
		}
		x.log.Info("deployed", "contract", planned.Name, "address", planned.Address,
			"salt", planned.Salt, "gas", r.GasUsed, "block", r.BlockNumber)
	} else {
		x.log.Info("already deployed", "contract", planned.Name, "address", planned.Address)
	}

	d.FeeStrategy = planned.Address
	if d.Salts == nil {
		d.Salts = map[string]string{}
	}
	d.Salts["farmFeeStrategy"] = SaltFeeStrategy.Hex()
	if d.ZNNQSRPair == (common.Address{}) {
		if pair, _, err := x.znnQSRPoolState(ctx); err == nil {
			d.ZNNQSRPair = pair
		}
	}
	return x.armFeeStrategy(ctx, d)
}
