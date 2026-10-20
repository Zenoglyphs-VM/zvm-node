package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"

	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func init() {
	register("dex", "seed Uniswap V2 pools that pair ZNN with the benchmark tokens", runDex)
}

// runDex seeds the pools that make the swap page usable from nothing but a
// faucet drip. `zvm bench setup` deploys Uniswap V2 with one pool, BNA/BNB,
// which nobody holding only ZNN can enter; ZNN pools are the way in.
func runDex(args []string) error {
	if len(args) == 0 || args[0] != "seed" {
		return fmt.Errorf("usage: zvm dex seed [flags]")
	}
	fs := flag.NewFlagSet("dex seed", flag.ExitOnError)
	env := bench.Flags(fs)
	deployment := fs.String("deployment", "bench/deployment.json", "deployment written by `zvm bench setup`")
	perPool := fs.String("znn", "10000", "ZNN to put in each pool")
	qsrPool := fs.Int64("qsr-pool-znn", 1_000, "ZNN to put in the ZNN/QSR pool, whose QSR comes over the bridge (0 to skip it)")
	fs.Parse(args[1:])

	e, err := env()
	if err != nil {
		return err
	}
	d, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return fmt.Errorf("%w — run `zvm bench setup` first", err)
	}
	arts, err := bench.LoadArtifacts()
	if err != nil {
		return err
	}
	znnAmount, err := parseUnits(*perPool, 18)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	s, err := bench.NewSender(ctx, devnet.EVMKey(bench.FunderEVMKey), e.Eth, e.Submit, e.Cfg)
	if err != nil {
		return err
	}
	factory, router := arts.MustGet("UniswapV2Factory"), arts.MustGet("UniswapV2Router02")
	token, pair := arts.MustGet("BenchERC20"), arts.MustGet("UniswapV2Pair")

	call := func(to common.Address, art *bench.Artifact, method string, args ...any) ([]any, error) {
		data, err := art.Call(method, args...)
		if err != nil {
			return nil, err
		}
		out, err := e.Eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", art.Name, method, err)
		}
		return art.ABI.Unpack(method, out)
	}

	// Different prices on purpose: routing BNA→BNB through ZNN then differs
	// visibly from the direct pool, so the page's route choice can be checked.
	for _, p := range []struct {
		name   string
		token  common.Address
		perZNN int64
	}{{"BNA", d.ERC20, 100}, {"BNB", d.TokenB, 250}} {
		// A swap-only deployment has no benchmark tokens, and -znn 0 says to leave
		// them alone. Either way the ZNN/QSR pool below is the only one opened,
		// which is the point: a pair is permanent once the factory emits it.
		if p.token == (common.Address{}) || znnAmount.Sign() == 0 {
			continue
		}
		got, err := call(d.Factory, factory, "getPair", p.token, d.WETH)
		if err != nil {
			return err
		}
		if existing := got[0].(common.Address); existing != (common.Address{}) {
			res, err := call(existing, pair, "getReserves")
			if err != nil {
				return err
			}
			if res[0].(*big.Int).Sign() > 0 {
				fmt.Printf("ZNN/%s  pool %s already has liquidity\n", p.name, existing.Hex())
				continue
			}
		}
		tokens := new(big.Int).Mul(znnAmount, big.NewInt(p.perZNN))
		allowance, err := call(p.token, token, "allowance", s.From, d.Router)
		if err != nil {
			return err
		}
		if allowance[0].(*big.Int).Cmp(tokens) < 0 {
			data, err := token.Call("approve", d.Router, tokens)
			if err != nil {
				return err
			}
			tx, err := s.Send(ctx, &p.token, new(big.Int), data, 80_000)
			if err != nil {
				return err
			}
			if _, err := bench.WaitReceipt(ctx, e.Eth, tx, 3*time.Minute); err != nil {
				return err
			}
		}
		// Exact minimums: a new pool takes the ratio it is given.
		data, err := router.Call("addLiquidityETH", p.token, tokens, tokens, znnAmount, s.From,
			big.NewInt(time.Now().Add(time.Hour).Unix()))
		if err != nil {
			return err
		}
		tx, err := s.Send(ctx, &d.Router, znnAmount, data, 4_000_000)
		if err != nil {
			return err
		}
		r, err := bench.WaitReceipt(ctx, e.Eth, tx, 3*time.Minute)
		if err != nil {
			return err
		}
		if got, err = call(d.Factory, factory, "getPair", p.token, d.WETH); err != nil {
			return err
		}
		fmt.Printf("ZNN/%s  pool %s: %s ZNN + %s %s, block %d, gas %d\n", p.name, got[0].(common.Address).Hex(),
			formatUnits(znnAmount, 18), formatUnits(tokens, 18), p.name, r.BlockNumber, r.GasUsed)
	}

	// The pair a holder of ZNN and QSR actually wants, and the only one on this
	// page whose price means anything outside the devnet. Its QSR has to be
	// brought over the bridge and moved with a native call first, which is what
	// apps.SeedZNNQSRPool does; `apps setup` opens the same pool if this is
	// skipped, and leaves it alone if it already has liquidity.
	if *qsrPool > 0 {
		pair, err := apps.SeedZNNQSRPool(ctx, apps.SetupConfig{
			Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, APIBase: e.API,
			Dex: d, PoolZNN: *qsrPool, Log: slog.Default().With("svc", "dex"),
		})
		if err != nil {
			return fmt.Errorf("the ZNN/QSR pool: %w", err)
		}
		fmt.Printf("ZNN/QSR pool %s: %d ZNN and the QSR the pair's price asks for\n", pair.Hex(), *qsrPool)
	}
	return nil
}
