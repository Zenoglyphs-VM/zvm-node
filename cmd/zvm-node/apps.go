package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func init() {
	register("apps", "deploy and seed the NFT marketplace, the collection factory and the farm", runApps)
}

func runApps(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zvm apps <setup|addresses|enable-fees|harvest-fees|trade|demo|glyph> [flags]")
	}
	switch args[0] {
	case "setup":
		return appsSetup(args[1:])
	case "addresses":
		return appsAddresses(args[1:])
	case "enable-fees":
		return appsEnableFees(args[1:])
	case "harvest-fees":
		return appsHarvestFees(args[1:])
	case "trade":
		return appsTrade(args[1:])
	case "demo":
		return appsDemo(args[1:])
	case "glyph":
		return appsGlyph(args[1:])
	default:
		return fmt.Errorf("unknown apps command %q", args[0])
	}
}

// appsAddresses recomputes every application address from this repository's
// artifacts and the published salts, so nobody has to take a deployment file's
// word for what the marketplace is (D32).
func appsAddresses(args []string) error {
	fs := flag.NewFlagSet("apps addresses", flag.ExitOnError)
	owner := fs.String("owner", "", "the address that owns the marketplace and the farm (default: the dev deployer)")
	rewardQSR := fs.Int64("reward-qsr", 100_000, "the farm's budget, in whole QSR")
	farmDays := fs.Int64("farm-days", 30, "how many days the budget is spread over")
	appsFile := fs.String("apps", "bench/apps.json", "a deployment to check against, if it exists")
	fs.Parse(args)

	arts, err := apps.LoadArtifacts()
	if err != nil {
		return err
	}
	from := crypto.PubkeyToAddress(devnet.EVMKey(bench.FunderEVMKey).PublicKey)
	if *owner != "" {
		from = common.HexToAddress(*owner)
	}
	plan, err := apps.BuildPlan(arts, apps.DefaultRecipe(from, *rewardQSR, *farmDays))
	if err != nil {
		return err
	}
	fmt.Printf("deployer    %s (a genesis predeploy on every ZVM chain)\n", plan.Deployer)
	fmt.Printf("owner       %s\n", plan.Recipe.Owner)
	fmt.Printf("reward rate %s per second\n\n", plan.Recipe.RewardRate)
	for _, c := range plan.All() {
		fmt.Printf("%-22s %s\n  salt %s\n  init code %d bytes\n", c.Name, c.Address, c.Salt, len(c.InitCode))
	}
	if d, err := apps.Load(*appsFile); err == nil {
		fmt.Println()
		if plan.Matches(d) {
			fmt.Printf("%s names exactly these contracts\n", *appsFile)
		} else {
			fmt.Printf("%s names OTHER contracts: exchange %s, factory %s, implementation %s, farm %s.\n"+
				"Either it predates deterministic deployment, or it is somebody else's deployment.\n",
				*appsFile, d.Exchange, d.CollectionFactory, d.CollectionImpl, d.Farm)
		}
	}
	return nil
}

// appsDemo lists more of what the deployer still holds, so a demo — or a test
// that buys — never runs the market dry.
func appsDemo(args []string) error {
	fs := flag.NewFlagSet("apps demo", flag.ExitOnError)
	env := bench.Flags(fs)
	appsFile := fs.String("apps", "bench/apps.json", "the application deployment")
	collection := fs.String("collection", "", "collection to list from (default: the first one)")
	count := fs.Int("count", 3, "how many to list")
	price := fs.Float64("price", 9, "asking price in ZNN")
	fs.Parse(args)

	e, err := env()
	if err != nil {
		return err
	}
	d, err := apps.Load(*appsFile)
	if err != nil {
		return fmt.Errorf("%w — run `zvm apps setup` first", err)
	}
	target := *collection
	if target == "" {
		if len(d.Collections) == 0 {
			return fmt.Errorf("%s names no collection", *appsFile)
		}
		target = d.Collections[0].Hex()
	}
	ctx, cancel := signalContext()
	defer cancel()
	listed, err := apps.ListMore(ctx, apps.SetupConfig{
		Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, APIBase: e.API,
		Log: slog.Default().With("svc", "apps"),
	}, d, common.HexToAddress(target), *count, *price)
	if err != nil {
		return err
	}
	fmt.Printf("listed %d item(s) from %s at %g ZNN\n", listed, target, *price)
	return nil
}

func appsSetup(args []string) error {
	fs := flag.NewFlagSet("apps setup", flag.ExitOnError)
	env := bench.Flags(fs)
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment written by `zvm bench setup`")
	out := fs.String("out", "bench/apps.json", "where to write the application deployment")
	rewardQSR := fs.Int64("reward-qsr", 100_000, "QSR to put in the farm")
	farmDays := fs.Int64("farm-days", 30, "how long the farm's budget should last")
	poolZNN := fs.Int64("pool-znn", 10_000, "ZNN to put in the new ZNN/QSR pool")
	skipNFT := fs.Bool("skip-nft", false, "deploy the contracts but seed no collections")
	fundZNN := fs.Int64("fund-znn", 100_000, "ZNN the deployer is brought up to through the vault; the bench funder has to hold it")
	fs.Parse(args)

	e, err := env()
	if err != nil {
		return err
	}
	dex, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return fmt.Errorf("%w — run `zvm bench setup` first", err)
	}
	if dex.ChainID != e.Cfg.L2ChainID {
		return fmt.Errorf("%s is for chain %d, this is chain %d", *deployment, dex.ChainID, e.Cfg.L2ChainID)
	}
	ctx, cancel := signalContext()
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	save := func(d *apps.Deployment) error {
		d.MediaURL = mediaBase(e.API)
		return d.Save(*out)
	}
	d, err := apps.Setup(ctx, apps.SetupConfig{
		Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, APIBase: e.API, Dex: dex,
		RewardQSR: *rewardQSR, FarmDays: *farmDays, PoolZNN: *poolZNN, SkipNFT: *skipNFT, FundZNN: *fundZNN,
		// The node learns where the exchange is from this file, and the seed
		// hands it signed orders — so it is written as soon as the contracts
		// exist, and again at the end with the collections added.
		OnDeployed: save,
		Log:        slog.Default().With("svc", "apps"),
	})
	if err != nil {
		return err
	}
	if err := save(d); err != nil {
		return err
	}
	fmt.Printf("application deployment written to %s\n", *out)
	fmt.Printf("  exchange           %s (bids in %s)\n", d.Exchange, d.WrappedZNN)
	fmt.Printf("  collection factory %s (implementation %s)\n", d.CollectionFactory, d.CollectionImpl)
	fmt.Printf("  farm               %s, rewards in %s\n", d.Farm, d.RewardToken)
	if d.ZNNQSRPair != (common.Address{}) {
		fmt.Printf("  ZNN/QSR pool       %s\n", d.ZNNQSRPair)
	}
	if d.FeeStrategy != (common.Address{}) {
		fmt.Printf("  fee strategy       %s\n", d.FeeStrategy)
		fmt.Printf("                     the Uniswap feeTo: 0.05%% of every swap becomes farm budget,\n")
		fmt.Printf("                     realised by `zvm apps harvest-fees`\n")
	}
	for _, c := range d.Collections {
		fmt.Printf("  collection         %s\n", c)
	}
	if d.MediaURL != "" {
		fmt.Printf("  media              %s\n", d.MediaURL)
	}
	return nil
}

// mediaBase asks the node where the objects it stores are addressed from.
func mediaBase(api string) string {
	resp, err := http.Get(strings.TrimRight(api, "/") + "/api/info")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		Media struct {
			PublicURL string `json:"publicUrl"`
		} `json:"media"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		return ""
	}
	return info.Media.PublicURL
}

// appsTrade walks the pools' prices around for a while, so the charts have
// something to draw and the trade lists something to show. Every trade is a
// real swap through the relayer.
func appsTrade(args []string) error {
	fs := flag.NewFlagSet("apps trade", flag.ExitOnError)
	env := bench.Flags(fs)
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment")
	duration := fs.Duration("duration", 10*time.Minute, "how long to trade for")
	pause := fs.Duration("pause", 20*time.Second, "how long to wait between bursts")
	burst := fs.Int("burst", 2, "swaps per burst")
	minZNN := fs.Float64("min", 1, "smallest trade, in ZNN")
	maxZNN := fs.Float64("max", 40, "largest trade, in ZNN")
	fs.Parse(args)

	e, err := env()
	if err != nil {
		return err
	}
	dex, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return err
	}
	arts, err := apps.LoadArtifacts()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	s, err := bench.NewSender(ctx, devnet.EVMKey(bench.FunderEVMKey), e.Eth, e.Submit, e.Cfg)
	if err != nil {
		return err
	}
	router := arts.MustGet("UniswapV2Router02")
	erc20 := arts.MustGet("BenchERC20")
	factory := arts.MustGet("UniswapV2Factory")

	// Every token with a pool against wrapped ZNN is fair game.
	type market struct {
		name  string
		token common.Address
		pair  common.Address
		start float64
	}
	var markets []market
	for _, t := range []struct {
		name  string
		token common.Address
	}{{"BNA", dex.ERC20}, {"BNB", dex.TokenB}, {"QSR", rollup.QSRTokenAddr}} {
		data, err := factory.Call("getPair", t.token, dex.WETH)
		if err != nil {
			return err
		}
		out, err := e.Eth.CallContract(ctx, ethereum.CallMsg{To: &dex.Factory, Data: data}, nil)
		if err != nil {
			return err
		}
		pair := common.BytesToAddress(out)
		if pair == (common.Address{}) {
			continue
		}
		markets = append(markets, market{name: t.name, token: t.token, pair: pair})
	}
	if len(markets) == 0 {
		return fmt.Errorf("no pools to trade — run `zvm dex seed` and `zvm apps setup` first")
	}

	// One approval per token, so the sells need no more.
	for _, m := range markets {
		data, err := erc20.Call("approve", dex.Router, new(big.Int).Lsh(big.NewInt(1), 200))
		if err != nil {
			return err
		}
		tx, err := s.Send(ctx, &m.token, new(big.Int), data, 80_000)
		if err != nil {
			return err
		}
		if _, err := bench.WaitReceipt(ctx, e.Eth, tx, 3*time.Minute); err != nil {
			return err
		}
	}

	from := crypto.PubkeyToAddress(devnet.EVMKey(bench.FunderEVMKey).PublicKey)
	deadline := time.Now().Add(*duration)
	trades, reverts := 0, 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		var last error
		for i := 0; i < *burst; i++ {
			m := &markets[rand.IntN(len(markets))]
			// A trade is as likely to go either way, but a price that has run
			// away from where it started is pulled back: a random walk that
			// wanders off the chart is not a market.
			buy := rand.IntN(2) == 0
			if price, err := poolPrice(ctx, e.Eth, arts, m.pair, m.token); err == nil {
				if m.start == 0 {
					m.start = price
				} else if price > m.start*1.25 {
					buy = false
				} else if price < m.start*0.8 {
					buy = true
				}
			}
			size := *minZNN + rand.Float64()*(*maxZNN-*minZNN)
			amount, err := parseUnits(fmt.Sprintf("%.6f", size), 18)
			if err != nil {
				return err
			}
			var data []byte
			var value *big.Int
			if buy {
				path := []common.Address{dex.WETH, m.token}
				data, err = router.Call("swapExactETHForTokens", new(big.Int), path, from, big.NewInt(time.Now().Add(time.Hour).Unix()))
				value = amount
			} else {
				// Sell roughly the same value back, in the token's own units.
				tokens, perr := tokensWorth(ctx, e.Eth, arts, dex.Router, dex.WETH, m.token, amount)
				if perr != nil {
					last = perr
					continue
				}
				held, perr := balanceOf(ctx, e.Eth, arts, m.token, from)
				if perr != nil {
					last = perr
					continue
				}
				if tokens.Cmp(held) > 0 {
					tokens = new(big.Int).Div(held, big.NewInt(2))
				}
				if tokens.Sign() == 0 {
					continue
				}
				path := []common.Address{m.token, dex.WETH}
				data, err = router.Call("swapExactTokensForETH", tokens, new(big.Int), path, from, big.NewInt(time.Now().Add(time.Hour).Unix()))
				value = new(big.Int)
			}
			if err != nil {
				return err
			}
			if _, err := s.Send(ctx, &dex.Router, value, data, 260_000); err != nil {
				last = err
				reverts++
				continue
			}
			trades++
		}
		if last != nil {
			slog.Warn("a swap did not go out", "err", last)
		}
		select {
		case <-ctx.Done():
		case <-time.After(*pause):
		}
	}
	fmt.Printf("%d swaps sent across %d pools (%d refused)\n", trades, len(markets), reverts)
	return nil
}

// read makes one view call against a deployed contract.
func read(ctx context.Context, eth *ethclient.Client, art *bench.Artifact, to common.Address, method string, args ...any) ([]any, error) {
	data, err := art.Call(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", art.Name, method, err)
	}
	return art.ABI.Unpack(method, out)
}

// poolPrice is the pool's price for `token`, in the other side's units. The
// decimals are left out on purpose: the walk only compares this price with
// where it started, and a constant factor does not change that.
func poolPrice(ctx context.Context, eth *ethclient.Client, arts *bench.Artifacts, pair, token common.Address) (float64, error) {
	art := arts.MustGet("UniswapV2Pair")
	first, err := read(ctx, eth, art, pair, "token0")
	if err != nil {
		return 0, err
	}
	reserves, err := read(ctx, eth, art, pair, "getReserves")
	if err != nil {
		return 0, err
	}
	r0, _ := new(big.Float).SetInt(reserves[0].(*big.Int)).Float64()
	r1, _ := new(big.Float).SetInt(reserves[1].(*big.Int)).Float64()
	if r0 == 0 || r1 == 0 {
		return 0, fmt.Errorf("pool %s is empty", pair)
	}
	if first[0].(common.Address) == token {
		return r1 / r0, nil
	}
	return r0 / r1, nil
}

// tokensWorth is how many `token` the router would give for this much ZNN,
// which is what selling that much back costs.
func tokensWorth(ctx context.Context, eth *ethclient.Client, arts *bench.Artifacts, router, weth, token common.Address, amountZNN *big.Int) (*big.Int, error) {
	out, err := read(ctx, eth, arts.MustGet("UniswapV2Router02"), router, "getAmountsOut", amountZNN, []common.Address{weth, token})
	if err != nil {
		return nil, err
	}
	amounts := out[0].([]*big.Int)
	return amounts[len(amounts)-1], nil
}

func balanceOf(ctx context.Context, eth *ethclient.Client, arts *bench.Artifacts, token, owner common.Address) (*big.Int, error) {
	out, err := read(ctx, eth, arts.MustGet("BenchERC20"), token, "balanceOf", owner)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

func appsGlyph(args []string) error {
	fs := flag.NewFlagSet("apps glyph", flag.ExitOnError)
	collection := fs.String("collection", "Zenon Momentums", "collection name, which seeds the art")
	id := fs.Int("id", 1, "token id")
	size := fs.Int("size", 512, "pixels")
	out := fs.String("out", "glyph.png", "where to write the PNG")
	fs.Parse(args)

	g := apps.NewGlyph(*collection, *id)
	data, err := g.PNG(*size)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s #%d: %s palette, %d rays, %d rings → %s (%d bytes)\n", *collection, *id, g.Palette, g.Rays, g.Rings, *out, len(data))
	return nil
}

// appsHarvestFees turns the swap fees the strategy has collected into farm budget.
//
// Permissionless on chain, so this needs no particular key — it uses the dev EVM
// funder because that is the one every other command here uses. What it does have
// to do off chain is find the pairs: the strategy holds LP of whatever pool last
// minted it a fee, and the factory is the only list of those.
func appsHarvestFees(args []string) error {
	fs := flag.NewFlagSet("apps harvest-fees", flag.ExitOnError)
	env := bench.Flags(fs)
	appsFile := fs.String("apps", "bench/apps.json", "the application deployment")
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment")
	minQSR := fs.String("min-qsr", "0", "fail unless at least this much QSR reaches the farm; the only real protection against the sale being sandwiched")
	fs.Parse(args)

	e, err := env()
	if err != nil {
		return err
	}
	dex, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return err
	}
	d, err := apps.Load(*appsFile)
	if err != nil {
		return err
	}
	if d.FeeStrategy == (common.Address{}) {
		return fmt.Errorf("%s names no fee strategy: re-run `zvm apps setup` against a chain with a swap", *appsFile)
	}
	minOut, err := parseUnits(*minQSR, 8)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()

	arts, err := apps.LoadArtifacts()
	if err != nil {
		return err
	}
	factory, strategy := arts.MustGet("UniswapV2Factory"), arts.MustGet("ZVMFarmFeeStrategy")
	farm := arts.MustGet("ZVMFarm")
	read := func(to common.Address, art *bench.Artifact, method string, args ...any) ([]any, error) {
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

	// Refuse early rather than land a revert: a strategy that is not feeTo has
	// collected nothing and never will.
	feeTo, err := read(dex.Factory, factory, "feeTo")
	if err != nil {
		return err
	}
	if feeTo[0].(common.Address) != d.FeeStrategy {
		return fmt.Errorf("the factory's feeTo is %s, not the strategy at %s — no swap fee is being diverted",
			feeTo[0].(common.Address), d.FeeStrategy)
	}

	n, err := read(dex.Factory, factory, "allPairsLength")
	if err != nil {
		return err
	}
	count := n[0].(*big.Int).Int64()
	var pairs []common.Address
	for i := int64(0); i < count; i++ {
		out, err := read(dex.Factory, factory, "allPairs", big.NewInt(i))
		if err != nil {
			return err
		}
		pair := out[0].(common.Address)
		held, err := read(d.FeeStrategy, strategy, "pending", pair)
		if err != nil {
			return err
		}
		lp := held[0].(*big.Int)
		fmt.Printf("pair %s  strategy LP %s\n", pair, lp)
		if lp.Sign() > 0 {
			pairs = append(pairs, pair)
		}
	}
	if len(pairs) == 0 {
		// Not an error. The fee accrues in the reserves from every swap, but a
		// pair only mints it during mint or burn, and the strategy needs a
		// position to burn before it can make that happen.
		fmt.Printf("nothing to harvest: the strategy holds no LP.\n")
		fmt.Printf("  A pair mints the protocol fee only inside mint and burn, so the first\n")
		fmt.Printf("  round needs a seed — `zvm apps setup` gives it one against the ZNN/QSR pool.\n")
		return nil
	}

	endBefore, err := read(d.Farm, farm, "endTime")
	if err != nil {
		return err
	}
	s, err := bench.NewSender(ctx, devnet.EVMKey(bench.FunderEVMKey), e.Eth, e.Submit, e.Cfg)
	if err != nil {
		return err
	}
	data, err := strategy.Call("harvest", pairs, minOut)
	if err != nil {
		return err
	}
	tx, err := s.Send(ctx, &d.FeeStrategy, new(big.Int), data, 3_000_000)
	if err != nil {
		return err
	}
	r, err := bench.WaitReceipt(ctx, e.Eth, tx, 3*time.Minute)
	if err != nil {
		return err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("harvest reverted in block %d", r.BlockNumber)
	}
	endAfter, err := read(d.Farm, farm, "endTime")
	if err != nil {
		return err
	}
	funded, err := read(d.Farm, farm, "totalFunded")
	if err != nil {
		return err
	}
	fmt.Printf("harvested %d pair(s) in block %d, gas %d\n", len(pairs), r.BlockNumber, r.GasUsed)
	fmt.Printf("  farm total funded  %s QSR\n", formatUnits(funded[0].(*big.Int), 8))
	fmt.Printf("  emission ends      %s -> %s (+%ds)\n",
		time.Unix(endBefore[0].(*big.Int).Int64(), 0).UTC().Format(time.RFC3339),
		time.Unix(endAfter[0].(*big.Int).Int64(), 0).UTC().Format(time.RFC3339),
		endAfter[0].(*big.Int).Int64()-endBefore[0].(*big.Int).Int64())
	return nil
}

// appsEnableFees adds the swap fee strategy to a chain whose marketplace is
// already deployed, without re-running the rest of the setup — which is not
// repeatable, because the farm refuses a pool it already has.
func appsEnableFees(args []string) error {
	fs := flag.NewFlagSet("apps enable-fees", flag.ExitOnError)
	env := bench.Flags(fs)
	appsFile := fs.String("apps", "bench/apps.json", "the application deployment, updated in place")
	deployment := fs.String("deployment", "bench/deployment.json", "the Uniswap deployment")
	fs.Parse(args)

	e, err := env()
	if err != nil {
		return err
	}
	dex, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return err
	}
	if dex.ChainID != e.Cfg.L2ChainID {
		return fmt.Errorf("%s is for chain %d, this is chain %d", *deployment, dex.ChainID, e.Cfg.L2ChainID)
	}
	d, err := apps.Load(*appsFile)
	if err != nil {
		return fmt.Errorf("%w — run `zvm apps setup` first", err)
	}
	if d.ChainID != e.Cfg.L2ChainID {
		return fmt.Errorf("%s is for chain %d, this is chain %d", *appsFile, d.ChainID, e.Cfg.L2ChainID)
	}
	ctx, cancel := signalContext()
	defer cancel()

	if err := apps.EnableFeeStrategy(ctx, apps.SetupConfig{
		Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, APIBase: e.API, Dex: dex,
		Log: slog.Default().With("svc", "apps"),
	}, d); err != nil {
		return err
	}
	if err := d.Save(*appsFile); err != nil {
		return err
	}
	fmt.Printf("fee strategy %s\n", d.FeeStrategy)
	fmt.Printf("  the Uniswap factory now mints it one sixth of every pair's 0.30%% fee\n")
	fmt.Printf("  realise it with `zvm apps harvest-fees`; it funds %s\n", d.Farm)
	fmt.Printf("%s updated\n", *appsFile)
	return nil
}
