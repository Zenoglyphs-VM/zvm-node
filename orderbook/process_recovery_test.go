package orderbook

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"modernc.org/sqlite"
)

const ownedRecoveryExit = 86

var ownedRecoveryPhases = []string{
	"engine-commit", "derivation-before-commit", "derivation-after-commit",
	"index-before-commit", "index-after-commit", "book-before-commit", "book-after-commit",
	"derivation-rewind-before-commit", "derivation-rewind-after-commit", "index-rewind-before-commit", "index-rewind-after-commit",
}

// Only a new same-binary child receives this configuration. A marker inside
// the parent's fresh test directory and the exact exit code/phase are required.
// Exiting skips database Close/Stop and test cleanup in that owned child.
// This is process termination, not a hardware power-loss or kernel-kill proof.
type ownedRecoveryConfig struct {
	Phase   string `json:"phase"`
	Path    string `json:"path"`
	Genesis string `json:"genesis"`
}

func exitOwnedRecoveryProcess(phase string) {
	raw, err := json.Marshal(map[string]any{"owned_recovery_boundary": phase, "actual_exit_requested": ownedRecoveryExit})
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stdout, string(raw))
	os.Exit(ownedRecoveryExit)
}

type ownedProcessPlugin struct {
	indexer.Plugin
	phase string
}

func (p ownedProcessPlugin) AfterBlock(tx *sql.Tx, block uint64) error {
	if err := p.Plugin.AfterBlock(tx, block); err != nil {
		return err
	}
	if block == 7 && p.phase == "index-before-commit" {
		exitOwnedRecoveryProcess(p.phase)
	}
	return nil
}

func (p ownedProcessPlugin) AfterRollback() {
	if aware, ok := p.Plugin.(indexer.RollbackAware); ok {
		aware.AfterRollback()
	}
}

type ownedRecoveryOutput struct {
	mu        sync.Mutex
	raw       []byte
	truncated bool
}

func (b *ownedRecoveryOutput) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(raw)
	if room := (64 << 10) - len(b.raw); len(raw) > room {
		raw, b.truncated = raw[:room], true
	}
	b.raw = append(b.raw, raw...)
	return n, nil
}

func ownedRecoveryChild(t *testing.T, cfg ownedRecoveryConfig) {
	t.Helper()
	valid := false
	for _, phase := range ownedRecoveryPhases {
		valid = valid || phase == cfg.Phase
	}
	relative, err := filepath.Rel(os.TempDir(), cfg.Path)
	if !valid || err != nil || !filepath.IsAbs(cfg.Path) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatal("invalid owned process fixture configuration")
	}
	marker := filepath.Join(cfg.Path, "owned-recovery.json")
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("owned fixture marker missing or replaced")
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var recorded ownedRecoveryConfig
	if json.Unmarshal(raw, &recorded) != nil || recorded != cfg {
		t.Fatal("owned fixture marker differs")
	}
	scenario := newExecutionScenario(t)
	if scenario.cfg.L2GenesisHash.Hex() != cfg.Genesis {
		t.Fatal("owned child genesis differs")
	}
	if err := sqlite.RegisterScalarFunction("zvm_owned_process_exit", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		exitOwnedRecoveryProcess(cfg.Phase)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	r := &executionRun{t: t, scenario: scenario, path: cfg.Path, limit: 7}
	// The plugin must be installed before Indexer.New, but rewind exits are
	// armed only after the competing branch has been committed.
	if cfg.Phase == "index-before-commit" {
		r.processPhase = cfg.Phase
	}
	r.nom = httptest.NewServer(http.HandlerFunc(r.nomReply))
	t.Cleanup(r.nom.Close)
	r.open()
	t.Cleanup(r.close)
	if strings.Contains(cfg.Phase, "rewind") {
		r.nomMu.Lock()
		r.branch = 1
		r.nomMu.Unlock()
		r.through(9, false)
	}
	r.processPhase = cfg.Phase
	r.backend.mu.Lock()
	r.backend.processPhase = cfg.Phase
	r.backend.mu.Unlock()
	triggers := map[string]string{
		"derivation-before-commit":        `CREATE TRIGGER n04_owned_process_exit AFTER UPDATE ON chunks WHEN NEW.consumed_at=8 BEGIN SELECT zvm_owned_process_exit(); END`,
		"book-before-commit":              `CREATE TRIGGER n04_owned_process_exit BEFORE INSERT ON nft_order_meta WHEN NEW.key LIKE 'scanned:%' AND NEW.value=7 BEGIN SELECT zvm_owned_process_exit(); END`,
		"derivation-rewind-before-commit": `CREATE TRIGGER n04_owned_process_exit AFTER UPDATE ON chunks WHEN OLD.consumed_at=8 AND NEW.consumed_at IS NULL BEGIN SELECT zvm_owned_process_exit(); END`,
		"index-rewind-before-commit":      `CREATE TRIGGER n04_owned_process_exit AFTER UPDATE ON nft_order_meta WHEN NEW.key LIKE 'scanned:%' AND OLD.value=8 AND NEW.value=6 BEGIN SELECT zvm_owned_process_exit(); END`,
	}
	if statement := triggers[cfg.Phase]; statement != "" {
		if _, err := r.db.SQL().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	r.d.OnRewind = func(number uint64) {
		if cfg.Phase == "derivation-rewind-after-commit" {
			exitOwnedRecoveryProcess(cfg.Phase)
		}
		r.callbackError = r.ix.Rewind(number)
		if r.callbackError == nil && cfg.Phase == "index-rewind-after-commit" {
			exitOwnedRecoveryProcess(cfg.Phase)
		}
	}
	r.nomMu.Lock()
	r.branch = 0
	r.nomMu.Unlock()
	r.through(9, false)
	t.Fatal("owned child did not reach its declared process boundary")
}

func TestExecutedNativeProcessRecoveryConverges(t *testing.T) {
	if raw := os.Getenv("ZVM_OWNED_RECOVERY_CHILD"); raw != "" {
		var cfg ownedRecoveryConfig
		if len(raw) > 8192 || json.Unmarshal([]byte(raw), &cfg) != nil {
			t.Fatal("invalid owned child declaration")
		}
		ownedRecoveryChild(t, cfg)
		return
	}
	scenario := newExecutionScenario(t)
	reference := newExecutionRun(t, scenario)
	reference.through(9, false)
	reference.assertExpected()
	want := reference.state()
	for _, phase := range ownedRecoveryPhases {
		t.Run(phase, func(t *testing.T) {
			r := newExecutionRun(t, scenario)
			r.through(7, false)
			r.close()
			cfg := ownedRecoveryConfig{Phase: phase, Path: r.path, Genesis: scenario.cfg.L2GenesisHash.Hex()}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(filepath.Join(r.path, "owned-recovery.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutedNativeProcessRecoveryConverges$", "-test.timeout=25s")
			// A child race must stop with its own code, before the intentional
			// recovery exit. Removing the race runtime's exit delay also keeps
			// the declared process boundary from running background work longer.
			cmd.Env = append(os.Environ(), "ZVM_OWNED_RECOVERY_CHILD="+string(raw), "GORACE=exitcode=87 halt_on_error=1 atexit_sleep_ms=0")
			output := &ownedRecoveryOutput{}
			cmd.Stdout, cmd.Stderr = output, output
			err = cmd.Run() // Wait joins this exact owned child before any reopen.
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != ownedRecoveryExit || ctx.Err() != nil || output.truncated {
				t.Fatalf("owned child outcome differs: %v, %s", err, output.raw)
			}
			if strings.Contains(string(output.raw), "DATA RACE") || strings.Contains(string(output.raw), "race detected during execution") {
				t.Fatalf("owned child reported a race: %s", output.raw)
			}
			marker := fmt.Sprintf(`"owned_recovery_boundary":%q`, phase)
			if !strings.Contains(string(output.raw), marker) {
				t.Fatalf("owned exit lacks the declared boundary marker: %s", output.raw)
			}
			t.Logf("owned child boundary=%s actual_exit=%d output_complete=true child_race_exit=87 child_race_diagnostics=false", phase, exit.ExitCode())
			r.nomMu.Lock()
			r.limit, r.branch = 9, 0
			r.nomMu.Unlock()
			r.clearProcessTrigger = true
			r.open()
			r.catchUp()
			if r.d.Head().Height < 9 {
				r.through(9, false)
			}
			r.assertExpected()
			assertExecutionState(t, r.state(), want)
			r.close()
			r.open()
			r.catchUp()
			r.assertExpected()
			assertExecutionState(t, r.state(), want)
		})
	}
}
