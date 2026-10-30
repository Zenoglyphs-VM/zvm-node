//go:build fullclient_recovery

package orderbook_test

import (
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"testing"
)

func ownedSignedFullClientProcessRecovery(t *testing.T, want map[string][]string) {
	t.Helper()
	ownedSignedFullClientProcessRecoveryAt(t, want, "derivation-before-commit")
}

func ownedSignedFullClientProcessRecoveryAt(t *testing.T, want map[string][]string, phase string) {
	t.Helper()
	switch phase {
	case "derivation-before-commit", "index-before-commit", "book-before-commit":
	default:
		t.Fatal("undeclared signed forward process boundary")
	}
	f := orderbook.NewFullClientSignedProcessFixture(t)
	start := func(phase string, number int) *ownedCLIProcess {
		return startOwnedCLIWithTest(t, f, phase, number, "TestFullClientOwnedProcess")
	}
	p := start("", 0)
	waitCLIHorizon(t, f, 7, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	// Initial admission uses the real persisted service after priming joined.
	// This is before the fault, with no CLI/engine process left active.
	f.RestartExecutionClient()
	f.AdmitSignedOrders()
	f.CloseExecutionClient()
	f.OwnedProcessTrigger(phase, true)
	f.Horizon(8)
	p = start(phase, 1)
	p.waitExit(86, false)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 86, phase)
	f.CheckExecutionClientDirectory()
	// The parent only removes its declared exit hook; index uses a child wrapper.
	// The production CLI and real full service recover all canonical state.
	f.OwnedProcessTrigger(phase, false)
	f.Horizon(8)
	p = start("", 2)
	waitCLIHorizon(t, f, 8, p)
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	// The parent reopens only after the exact recovery child has joined,
	// solely for the original read-only signature/business/table oracle.
	f.RestartExecutionClient()
	orderbook.AssertRecoveryCLIState(t, f.SignedSnapshot(), want)
	f.CloseExecutionClient()
	t.Logf("full_client_signed_owned_process_restore=true boundary=%s actual_exit=86 engine_head_at_exit=7 service_cleanup_skipped=true joined_children=3 full_service_opens=5 initial_admission_before_fault_only=true parent_repair_or_resubmission_after_fault=false exact_canonical_tables_equal=true original_EOA_signatures_preserved=true NoM_source_signatures=false kernel_kill=false hardware_power_loss=false full_N04_acceptance=false", phase)
}
