package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func init() {
	register("bench", "deploy standard contracts and drive load through both lanes", runBench)
}

func runBench(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zvm-node bench <setup|run|suite|report|storage> [flags]\nscenarios: %s", strings.Join(bench.ScenarioNames(), ", "))
	}
	switch args[0] {
	case "setup":
		return benchSetup(args[1:])
	case "run":
		return benchRun(args[1:])
	case "suite":
		return benchSuite(args[1:])
	case "report":
		return benchReport(args[1:])
	case "storage":
		return benchStorage(args[1:])
	default:
		return fmt.Errorf("unknown bench command %q", args[0])
	}
}

func benchSetup(args []string) error {
	fs := flag.NewFlagSet("bench setup", flag.ExitOnError)
	env := bench.Flags(fs)
	out := fs.String("out", "bench/deployment.json", "where to write the deployment")
	fundZNN := fs.Int64("fund-znn", 200_000, "ZNN the deployer is brought up to through the vault; the bench funder has to hold it")
	swapOnly := fs.Bool("swap-only", false, "deploy WZNN, the factory and the router and nothing else: no benchmark tokens, and no BNA/BNB pool. A pair is permanent once the factory emits it, so a chain that only wants ZNN pairs has to start this way")
	fs.Parse(args)
	e, err := env()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	d, err := bench.Setup(ctx, e.Cfg, e.Eth, e.Submit, e.L1, bench.SetupOptions{
		FundZNN: *fundZNN, SwapOnly: *swapOnly, Log: slog.Default().With("svc", "bench"),
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := d.Save(*out); err != nil {
		return err
	}
	fmt.Printf("deployment written to %s\n", *out)
	fmt.Printf("  WZNN %s\n  Uniswap V2 factory %s\n  router %s\n", d.WETH, d.Factory, d.Router)
	if *swapOnly {
		fmt.Printf("  swap only: no benchmark tokens, and no pair until `zvm dex seed` opens one\n")
	} else {
		fmt.Printf("  ERC-20 %s\n  ERC-721 %s\n  ERC-1155 %s\n  Uniswap V2 pair %s\n  storage churn %s\n",
			d.ERC20, d.ERC721, d.ERC1155, d.Pair, d.Churn)
	}
	return nil
}

type runFlags struct {
	deployment *string
	scenario   *string
	lane       *string
	wallets    *int
	rate       *float64
	duration   *time.Duration
	outDir     *string
	containers *string
}

func addRunFlags(fs *flag.FlagSet) runFlags {
	return runFlags{
		deployment: fs.String("deployment", "bench/deployment.json", "deployment written by `bench setup`"),
		scenario:   fs.String("scenario", "erc20", "scenario: "+strings.Join(bench.ScenarioNames(), ", ")),
		lane:       fs.String("lane", "batch", "batch (EVM keys through the relayer) or native (Zenon keys)"),
		wallets:    fs.Int("wallets", 25, "concurrent wallets"),
		rate:       fs.Float64("rate", 10, "target transactions per second across all wallets (0: as fast as they go)"),
		duration:   fs.Duration("duration", 2*time.Minute, "how long to drive load"),
		outDir:     fs.String("out", "bench/results", "directory for result JSON"),
		// Both engines are named because either may be the one running (D2);
		// bench.Run drops the ones docker does not have.
		containers: fs.String("containers", "zvm-op-geth,zvm-op-reth,zvm-l1-pillar,zvm-l1-rpc", "containers to sample (empty to skip)"),
	}
}

func doRun(ctx context.Context, e *bench.Env, f runFlags) (*bench.Result, error) {
	d, err := bench.LoadDeployment(*f.deployment)
	if err != nil {
		return nil, fmt.Errorf("%w — run `zvm bench setup` first", err)
	}
	var containers []string
	if s := strings.TrimSpace(*f.containers); s != "" {
		containers = strings.Split(s, ",")
	}
	log := slog.Default().With("svc", "bench")
	log.Info("run", "scenario", *f.scenario, "lane", *f.lane, "wallets", *f.wallets, "rate", *f.rate, "duration", f.duration.String())
	res, err := bench.Run(ctx, bench.RunConfig{
		Rollup: e.Cfg, Eth: e.Eth, Submit: e.Submit, L1: e.L1, Deployment: d, APIBase: e.API,
		Scenario: *f.scenario, Lane: *f.lane, Wallets: *f.wallets, Rate: *f.rate, Duration: *f.duration,
		Containers: containers, Log: log,
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(*f.outDir, 0o755); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s-w%d-r%.0f-%s.json", *f.scenario, *f.lane, *f.wallets, *f.rate, res.StartedAt.Format("150405"))
	path := filepath.Join(*f.outDir, name)
	if err := res.Save(path); err != nil {
		return nil, err
	}
	s := res.Summary
	fmt.Printf("%-9s %-6s w%-4d %6.2f tx/s  p50 %5.1fs  p90 %5.1fs  gas/tx %8d  NoM %5.0f B/tx  max %d tx/block  → %s\n",
		res.Scenario, res.Lane, res.Wallets, s.Throughput, float64(s.Latency.P50)/1000, float64(s.Latency.P90)/1000,
		s.GasMean, s.BytesPerTx, s.TxPerBlockMax, path)
	return res, nil
}

func benchRun(args []string) error {
	fs := flag.NewFlagSet("bench run", flag.ExitOnError)
	env := bench.Flags(fs)
	f := addRunFlags(fs)
	fs.Parse(args)
	e, err := env()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	_, err = doRun(ctx, e, f)
	return err
}

// benchSuite is the matrix the report is built from.
func benchSuite(args []string) error {
	fs := flag.NewFlagSet("bench suite", flag.ExitOnError)
	env := bench.Flags(fs)
	f := addRunFlags(fs)
	quick := fs.Bool("quick", false, "shorter runs, for a smoke test of the harness")
	statusURL := fs.String("relayer-status", "", "relayer /status, polled between steps so a backlog does not spill into the next one (default: -submit + /status)")
	fs.Parse(args)
	e, err := env()
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if *statusURL == "" {
		*statusURL = strings.TrimRight(e.SubmitURL, "/") + "/status"
	}

	type step struct {
		scenario string
		lane     string
		wallets  int
		rate     float64
	}
	// Rates are bounded on purpose, saturation included: an unbounded lane
	// queues tens of thousands of transactions inside the relayer and the
	// backlog spills into the next step, which would measure the previous one.
	steps := []step{
		{"transfer", "batch", 20, 5},
		{"transfer", "batch", 50, 25},
		{"transfer", "batch", 100, 100},
		{"transfer", "batch", 200, 400},
		{"erc20", "batch", 50, 25},
		{"erc20", "batch", 150, 250},
		{"erc721", "batch", 50, 25},
		{"erc1155", "batch", 50, 25},
		{"swap", "batch", 40, 20},
		{"churn", "batch", 25, 10},
		{"deploy", "batch", 10, 3},
		{"transfer", "native", 20, 5},
		{"transfer", "native", 60, 15},
		{"erc20", "native", 40, 10},
	}
	dur := *f.duration
	if *quick {
		dur = 30 * time.Second
		steps = []step{{"transfer", "batch", 10, 5}, {"erc20", "batch", 10, 5}, {"transfer", "native", 5, 2}}
	}
	for i, s := range steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		scenario, lane, wallets, rate, d := s.scenario, s.lane, s.wallets, s.rate, dur
		step := runFlags{
			deployment: f.deployment, scenario: &scenario, lane: &lane, wallets: &wallets, rate: &rate,
			duration: &d, outDir: f.outDir, containers: f.containers,
		}
		if _, err := doRun(ctx, e, step); err != nil {
			fmt.Fprintf(os.Stderr, "step %d (%s/%s) failed: %v\n", i+1, s.scenario, s.lane, err)
		}
		// Let the chain settle between steps, so a run's window is its own. A
		// fixed pause is not enough: a saturating step leaves tens of thousands
		// of transactions queued inside the relayer, and on a chain that cannot
		// drain them — chain 71 at 400 offered, measured 2026-09-16 — every
		// later step measures that backlog instead of its own load. So wait for
		// the queue to go quiet, and say so when it will not.
		if err := settle(ctx, e, *statusURL, 20*time.Second, 3*time.Minute); err != nil {
			return err
		}
	}
	return nil
}

// settle pauses between benchmark steps until the relayer has worked off what
// the last one left, so a step's window is its own. It always waits at least
// `min`, gives up after `max`, and treats an unreachable relayer as "nothing to
// wait for" rather than an error — the cap is a courtesy, not a measurement.
func settle(ctx context.Context, e *bench.Env, statusURL string, min, max time.Duration) error {
	if err := sleepCtx(ctx, min); err != nil {
		return err
	}
	deadline := time.Now().Add(max)
	for {
		queued, haveQueue := relayerQueued(ctx, statusURL)
		price := fusionPrice(ctx, e)
		// Under dynamic plasma a saturating step leaves the fusion price high
		// for minutes afterwards, and a block costs `ceil(base × price/1000)`
		// fused plasma — so a step starting at 20x is not measuring the same
		// chain as one starting at 1x. Wait for both the queue and the price.
		if (!haveQueue || queued == 0) && price <= settlePriceScaled {
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "after %s the relayer still holds %d and the fusion price is %.2fx; the next step measures that too\n",
				max, queued, float64(price)/1000)
			return nil
		}
		if err := sleepCtx(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

// settlePriceScaled is "close enough to the base price to start a step", in
// go-zenon's thousandths: 1,200 is 1.2x.
const settlePriceScaled = 1_200

// fusionPrice is what a block will be charged on the next momentum. On v1 there
// is no such price and the base is reported, so this never holds a step up.
func fusionPrice(ctx context.Context, e *bench.Env) uint64 {
	if e == nil || e.L1 == nil {
		return 0
	}
	m, err := e.L1.FrontierMomentum(ctx)
	if err != nil || m == nil {
		return 0
	}
	return m.FusionPrice()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// relayerQueued reads the relayer's queue depth. The bool says whether the
// answer is worth anything.
func relayerQueued(ctx context.Context, statusURL string) (int, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return 0, false
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var st struct {
		Queued int `json:"queued"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&st); err != nil {
		return 0, false
	}
	return st.Queued, true
}

func benchReport(args []string) error {
	fs := flag.NewFlagSet("bench report", flag.ExitOnError)
	in := fs.String("in", "bench/results", "directory of result JSON")
	out := fs.String("out", "bench/RESULTS.md", "markdown to write")
	fs.Parse(args)
	results, err := bench.LoadResults(*in)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return fmt.Errorf("no results in %s", *in)
	}
	md := bench.Markdown(results)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s from %d runs\n", *out, len(results))
	return nil
}

// benchStorage sizes this node's database, and its media directory when it has
// one. It runs inside the node's own container, which is why it lives with the
// node rather than with the fleet harness that calls it: nothing else in the
// image can see those volumes.
func benchStorage(args []string) error {
	fs := flag.NewFlagSet("bench storage", flag.ExitOnError)
	db := fs.String("db", "/data/zvm.db", "the node's database")
	media := fs.String("media", "", "the media directory to size as well")
	fs.Parse(args)
	rep, err := bench.InspectStorage(context.Background(), *db, *media)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(rep)
}
