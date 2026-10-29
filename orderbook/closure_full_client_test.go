//go:build fullclient_recovery

package orderbook_test

import (
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

func ownedFullClientLegacyClosureRecovery(t *testing.T, want map[string][]string) {
	t.Helper()
	f := orderbook.NewFullClientRecoveryProcessFixture(t)
	start := func(number int) *ownedCLIProcess {
		return startOwnedCLIWithTest(t, f, "", number, "TestFullClientOwnedProcess")
	}
	p := start(0)
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, "legacy-closed-recovery")
	f.CheckExecutionClientDirectory()
	// This is a declared legacy input, not an assertion that a crash produced
	// these exact bytes. Every actual recovery is performed by production.
	f.DeclareLegacyClosedCaches()
	p = start(1)
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, "legacy-closed-recovery")
	f.CheckExecutionClientDirectory()
	f.RestartExecutionClient()
	f.AssertRecoveryWithOrderObservations(f.Snapshot(), want)
	f.CloseExecutionClient()
	t.Log("owned_full_client_legacy_closed_restore=true readonly_canonical_business_state_equal=true original_order_proofs_verified=true original_inputs_preserved=true parent_repair_after_restart=false same_binary_cli_and_client=true graceful_children=2 historical_first_closure_time_established=false full_N04_acceptance=false")
}
