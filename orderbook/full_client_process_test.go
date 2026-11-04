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
	rewind := phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit"
	boundaryHead := uint64(7)
	if rewind {
		// Catalyst retains the higher canonical head until replacement payloads.
		boundaryHead = 8
		f.SourceBranch(1)
	}
	start := func(phase string, number int) *ownedCLIProcess {
		return startOwnedCLIWithTest(t, f, phase, number, "TestFullClientOwnedProcess")
	}
	// Prime a real default-persistence service through its ordinary graceful
	// close. The next process must preserve or replay its interrupted head.
	p := start("", 0)
	if rewind {
		waitCLIHorizon(t, f, 9, p)
		t.Log("owned_full_client_rewind_prime=true source_branch=1 momentum=9 readonly_source_hash_and_order_state_verified=true parent_reconciliation=false")
	} else {
		waitCLIHorizon(t, f, 7, p)
	}
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	if rewind {
		f.DeclareRewindAncestor()
	}
	f.OwnedProcessTrigger(phase, true)
	if rewind {
		f.SourceBranch(0)
	} else {
		f.Horizon(8)
	}
	p = start(phase, 1)
	p.waitExit(86, false)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 86, phase)
	f.CheckExecutionClientDirectory()
	// Remove only the declared exit trigger. Production startup/derivation,
	// index and book workers perform recovery; the parent performs no repair.
	f.OwnedProcessTrigger(phase, false)
	// An interrupted rewind can retain momentum 9. Its replacement must be
	// available immediately; a frontier of 8 correctly makes production wait.
	if rewind {
		f.Horizon(9)
	} else {
		f.Horizon(8)
	}
	p = start("", 2)
	if !rewind {
		waitCLIHorizon(t, f, 8, p)
	}
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	// The service is opened in the parent only after all three exact children
	// joined, solely for the existing read-only chain/SQL state oracle.
	f.RestartExecutionClient()
	got := f.Snapshot()
	if rewind {
		f.AssertRecoveryWithOrderObservations(got, want)
	} else {
		orderbook.AssertRecoveryCLIState(t, got, want)
	}
	f.CloseExecutionClient()
	t.Logf("owned_full_client_process_restore=true boundary=%s readonly_canonical_state_equal=true engine_head_at_process_exit=%d service_cleanup_skipped=true seeded_synthetic_genesis=true standalone_daemon=false full_N04_acceptance=false", phase, boundaryHead)
}
