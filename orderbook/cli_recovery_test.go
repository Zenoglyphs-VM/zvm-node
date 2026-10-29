package orderbook_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/nodecmd"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"modernc.org/sqlite"
)

type ownedCLIConfig struct {
	Directory string   `json:"directory"`
	Marker    string   `json:"marker"`
	Phase     string   `json:"phase"`
	Args      []string `json:"args"`
}

type cliOutput struct {
	mu        sync.Mutex
	raw       []byte
	truncated bool
}

func (b *cliOutput) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(raw)
	if room := (64 << 10) - len(b.raw); len(raw) > room {
		raw, b.truncated = raw[:room], true
	}
	b.raw = append(b.raw, raw...)
	return n, nil
}

func (b *cliOutput) snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.raw)
}

func validateOwnedCLIArgs(t *testing.T, cfg ownedCLIConfig, allowUnbound bool) {
	t.Helper()
	if len(cfg.Args) != 18 {
		t.Fatal("unexpected CLI argument count")
	}
	for i, name := range []string{"config", "jwt", "db", "l1", "engine", "eth", "apps"} {
		if cfg.Args[2*i] != "-"+name {
			t.Fatal("unexpected CLI argument")
		}
		value := cfg.Args[2*i+1]
		if i >= 3 && i <= 5 {
			if allowUnbound && i >= 4 && value == "" {
				continue
			}
			u, err := url.Parse(value)
			if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" {
				t.Fatal("CLI endpoint is outside owned loopback scope")
			}
		} else if filepath.Dir(value) != cfg.Directory {
			t.Fatal("CLI file is outside its fixture")
		}
	}
	if !reflect.DeepEqual(cfg.Args[14:], []string{"-listen=", "-verify=false", "-prices=off", "-mainnet-rpc=off"}) {
		t.Fatal("external CLI facilities are not disabled")
	}
}

func runOwnedCLIChild(t *testing.T, raw string) {
	t.Helper()
	runOwnedCLIChildWithSetup(t, raw, nil)
}

// Setup binds only the owned service endpoints after the original declaration
// checks. The final arguments pass the same validation before production startup.
func runOwnedCLIChildWithSetup(t *testing.T, raw string, setup func(*testing.T, ownedCLIConfig) ([]string, func())) {
	t.Helper()
	var beforeExit func()
	var cfg ownedCLIConfig
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &cfg) != nil {
		t.Fatal("invalid owned CLI declaration")
	}
	relative, err := filepath.Rel(os.TempDir(), cfg.Directory)
	if err != nil || !filepath.IsAbs(cfg.Directory) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Dir(cfg.Marker) != cfg.Directory {
		t.Fatal("owned CLI directory is outside its fixture")
	}
	info, err := os.Lstat(cfg.Marker)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("owned CLI marker missing or replaced")
	}
	data, err := os.ReadFile(cfg.Marker)
	var recorded ownedCLIConfig
	if err != nil || json.Unmarshal(data, &recorded) != nil || !reflect.DeepEqual(recorded, cfg) {
		t.Fatal("owned CLI marker differs")
	}
	if cfg.Phase != "" && cfg.Phase != "derivation-before-commit" && cfg.Phase != "index-before-commit" && cfg.Phase != "book-before-commit" {
		t.Fatal("unexpected owned CLI phase")
	}
	if (cfg.Phase == "index-before-commit" || cfg.Phase == "book-before-commit") && setup == nil {
		t.Fatal("actual-client write boundaries require their owned service setup")
	}
	validateOwnedCLIArgs(t, cfg, setup != nil)
	if setup != nil {
		cfg.Args, beforeExit = setup(t, cfg)
		validateOwnedCLIArgs(t, cfg, false)
	}
	if cfg.Phase != "" && cfg.Phase != "index-before-commit" {
		if err := sqlite.RegisterScalarFunction("zvm_owned_cli_exit", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			exitOwnedCLIAtBoundary(cfg.Phase, beforeExit)
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// This is the production entrypoint with both extensions used by runNode.
	// No package-private Deriver, Indexer or Book methods are called here.
	market := &nodecmd.Market{}
	var marketExtension nodecmd.Extender = market
	if cfg.Phase == "index-before-commit" {
		marketExtension = &ownedCLIIndexMarket{Market: market, beforeExit: beforeExit}
	}
	err = nodecmd.Run(cfg.Args, marketExtension, &nodecmd.Markets{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("production CLI did not stop through its signal context: %v", err)
	}
	fmt.Println(`{"owned_cli_return":"context-canceled","actual_exit_expected":0}`)
}

type ownedCLIProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	output *cliOutput
	done   chan error
	cancel context.CancelFunc
	ctx    context.Context
	joined bool
	phase  string
}

// Both fixtures use the same exact-child lifecycle and production CLI controls.
type ownedCLIFixture interface {
	Directory() string
	Args() []string
	Horizon(uint64)
	Ready(context.Context, uint64) (bool, string)
}

func startOwnedCLI(t *testing.T, f ownedCLIFixture, phase string, number int) *ownedCLIProcess {
	t.Helper()
	return startOwnedCLIWithTest(t, f, phase, number, "TestExecutedNativeCLIRecovery")
}

func startOwnedCLIWithTest(t *testing.T, f ownedCLIFixture, phase string, number int, testName string) *ownedCLIProcess {
	t.Helper()
	if testName != "TestExecutedNativeCLIRecovery" && testName != "TestFullClientOwnedProcess" {
		t.Fatal("unexpected owned CLI child entrypoint")
	}
	cfg := ownedCLIConfig{Directory: f.Directory(), Marker: filepath.Join(f.Directory(), fmt.Sprintf("owned-cli-%d.json", number)), Phase: phase, Args: f.Args()}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(cfg.Marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+testName+"$", "-test.timeout=115s")
	cmd.Env = append(os.Environ(), "ZVM_OWNED_CLI_CHILD="+string(raw), "GORACE=exitcode=87 halt_on_error=1 atexit_sleep_ms=0")
	output := &cliOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &ownedCLIProcess{t: t, cmd: cmd, output: output, done: make(chan error, 1), cancel: cancel, ctx: ctx, phase: cfg.Phase}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.joined {
			cmd.Process.Signal(syscall.SIGTERM)
			forced := false
			var err error
			select {
			case err = <-p.done:
			case <-time.After(5 * time.Second):
				forced = true
				cmd.Process.Kill()
				err = <-p.done
			}
			p.joined = true
			if t.Failed() {
				t.Logf("owned CLI failure cleanup joined=true forced_kill=%t wait_error=%v output_truncated=%t output=%s", forced, err, output.truncated, output.raw)
			}
		}
		cancel()
	})
	return p
}

func (p *ownedCLIProcess) waitExit(want int, signal bool) {
	p.t.Helper()
	if signal {
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			p.t.Fatal(err)
		}
	}
	var err error
	select {
	case err = <-p.done:
		p.joined = true
	case <-time.After(10 * time.Second):
		p.t.Fatal("owned CLI did not exit at the declared boundary")
	}
	exit := 0
	if err != nil {
		var failure *exec.ExitError
		if !errors.As(err, &failure) {
			p.t.Fatal(err)
		}
		exit = failure.ExitCode()
	}
	if exit != want || p.ctx.Err() != nil || p.output.truncated || strings.Contains(string(p.output.raw), "DATA RACE") || strings.Contains(string(p.output.raw), "race detected during execution") {
		p.t.Fatalf("owned CLI outcome differs: exit=%d error=%v output=%s", exit, err, p.output.raw)
	}
	marker := `"owned_cli_return":"context-canceled"`
	if want == 86 {
		marker = fmt.Sprintf(`"owned_cli_boundary":%q`, p.phase)
	}
	if !strings.Contains(string(p.output.raw), marker) {
		p.t.Fatalf("owned CLI lacks its exact boundary marker: %s", p.output.raw)
	}
	p.cancel()
	p.t.Logf("owned production CLI signal_requested=%t actual_exit=%d output_complete=true child_race_exit=87 child_race_diagnostics=false", signal, exit)
}

func waitCLIHorizon(t *testing.T, f ownedCLIFixture, height uint64, p *ownedCLIProcess) {
	t.Helper()
	f.Horizon(height)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for {
		select {
		case err := <-p.done:
			p.joined = true
			t.Fatalf("owned CLI exited before horizon %d: %v, output=%s", height, err, p.output.raw)
		default:
		}
		ready, detail := f.Ready(ctx, height)
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("production CLI horizon %d did not converge: %s, output=%s", height, detail, p.output.snapshot())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestExecutedNativeCLIRecovery(t *testing.T) {
	if raw := os.Getenv("ZVM_OWNED_CLI_CHILD"); raw != "" {
		runOwnedCLIChild(t, raw)
		return
	}
	reference := orderbook.NewRecoveryCLIFixture(t)
	p := startOwnedCLI(t, reference, "", 0)
	for _, h := range []uint64{7, 8, 9} {
		waitCLIHorizon(t, reference, h, p)
	}
	p.waitExit(0, true)
	want := reference.Snapshot()
	for _, phase := range []string{"graceful-reopen", "derivation-before-commit"} {
		t.Run(phase, func(t *testing.T) {
			f := orderbook.NewRecoveryCLIFixture(t)
			p := startOwnedCLI(t, f, "", 0)
			waitCLIHorizon(t, f, 7, p)
			p.waitExit(0, true)
			if phase == "derivation-before-commit" {
				f.Trigger(true)
				f.Horizon(8)
				p = startOwnedCLI(t, f, phase, 1)
				p.waitExit(86, false)
				f.Trigger(false)
				p = startOwnedCLI(t, f, "", 2)
			} else {
				p = startOwnedCLI(t, f, "", 1)
				waitCLIHorizon(t, f, 7, p)
			}
			waitCLIHorizon(t, f, 8, p)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			orderbook.AssertRecoveryCLIState(t, f.Snapshot(), want)
			t.Log("production CLI default_poll=3s default_recheck=30s readonly_canonical_state_equal=true parent_reconciliation=false")
		})
	}
}

func exitOwnedCLIAtBoundary(phase string, beforeExit func()) {
	if beforeExit != nil {
		beforeExit()
	}
	raw, err := json.Marshal(map[string]any{"owned_cli_boundary": phase, "actual_exit_requested": 86})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(raw))
	os.Exit(86)
}

// Delegate the actual shipped market and NFT plugin, then exit inside the
// index transaction after its real writes and before IndexBlock commits.
type ownedCLIIndexMarket struct {
	*nodecmd.Market
	beforeExit func()
}

func (m *ownedCLIIndexMarket) Build(ctx context.Context, env nodecmd.Env) (nodecmd.Result, error) {
	result, err := m.Market.Build(ctx, env)
	if err != nil {
		return result, err
	}
	if len(result.Plugins) != 1 || result.Plugins[0].Name() != "nft" {
		return nodecmd.Result{}, errors.New("owned index boundary requires the shipped NFT plugin")
	}
	result.Plugins[0] = ownedCLIIndexPlugin{Plugin: result.Plugins[0], beforeExit: m.beforeExit}
	return result, nil
}

type ownedCLIIndexPlugin struct {
	indexer.Plugin
	beforeExit func()
}

func (p ownedCLIIndexPlugin) AfterBlock(tx *sql.Tx, block uint64) error {
	if err := p.Plugin.AfterBlock(tx, block); err != nil {
		return err
	}
	if block == 7 {
		var blocks, logs int
		if err := tx.QueryRow("SELECT count(*) FROM blocks WHERE number=7").Scan(&blocks); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT count(*) FROM logs WHERE block=7").Scan(&logs); err != nil {
			return err
		}
		if blocks != 1 || logs == 0 {
			return errors.New("owned index boundary lacks its transaction-visible writes")
		}
		fmt.Println("owned_index_plugin_before_commit block=7 transaction_visible=true real_nft_plugin_completed=true")
		exitOwnedCLIAtBoundary("index-before-commit", p.beforeExit)
	}
	return nil
}

func (p ownedCLIIndexPlugin) AfterRollback() {
	if aware, ok := p.Plugin.(indexer.RollbackAware); ok {
		aware.AfterRollback()
	}
}
