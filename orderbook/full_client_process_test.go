//go:build fullclient_recovery

package orderbook_test

import (
	"os"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// Only the exact same-binary child runs the actual full service with the CLI.
// The parent selects this helper explicitly and owns its bounded output/exit.
func TestFullClientOwnedProcess(t *testing.T) {
	raw := os.Getenv("ZVM_OWNED_CLI_CHILD")
	if raw == "" {
		t.Skip("owned full-client helper requires its parent declaration")
	}
	runOwnedCLIChildWithSetup(t, raw, func(t *testing.T, cfg ownedCLIConfig) ([]string, func()) {
		return orderbook.OwnedFullClientCLIArgs(t, cfg.Directory, cfg.Args, cfg.Phase)
	})
}

func ownedFullClientProcessRecovery(t *testing.T, want map[string][]string, phase string) {
	t.Helper()
	f := orderbook.NewFullClientRecoveryProcessFixture(t)
	start := func(phase string, number int) *ownedCLIProcess {
		return startOwnedCLIWithTest(t, f, phase, number, "TestFullClientOwnedProcess")
	}
	// Prime a real default-persistence service through its ordinary graceful
	// close. The next process must preserve or replay its interrupted head.
	p := start("", 0)
	waitCLIHorizon(t, f, 7, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	f.OwnedProcessTrigger(phase, true)
	f.Horizon(8)
	p = start(phase, 1)
	p.waitExit(86, false)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 86, phase)
	f.CheckExecutionClientDirectory()
	// Remove only the declared exit trigger. Production startup/derivation,
	// index and book workers perform recovery; the parent performs no repair.
	f.OwnedProcessTrigger(phase, false)
	p = start("", 2)
	waitCLIHorizon(t, f, 8, p)
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	// The service is opened in the parent only after all three exact children
	// joined, solely for the existing read-only chain/SQL state oracle.
	f.RestartExecutionClient()
	orderbook.AssertRecoveryCLIState(t, f.Snapshot(), want)
	f.CloseExecutionClient()
	t.Logf("owned_full_client_process_restore=true boundary=%s readonly_canonical_state_equal=true engine_head_at_process_exit=7 service_cleanup_skipped=true seeded_synthetic_genesis=true standalone_daemon=false full_N04_acceptance=false", phase)
}
