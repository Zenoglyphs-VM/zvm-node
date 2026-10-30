//go:build fullclient_recovery

package orderbook_test

import (
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"testing"
)

// This extends the signed-order path through the actual default-persistence
// client. Its engine reopen is graceful; the declared SQL fault exits the CLI.
// It does not extend the independent NoM/source or engine-process crash proof.
func TestFullClientSignedOrderRecovery(t *testing.T) {
	prime := func(f *orderbook.FullClientSignedRecoveryFixture) {
		p := startOwnedCLI(t, f, "", 0)
		waitCLIHorizon(t, f, 7, p)
		p.waitExit(0, true)
		f.AdmitSignedOrders()
	}
	reference := orderbook.NewFullClientSignedRecoveryFixture(t)
	prime(reference)
	p := startOwnedCLI(t, reference, "", 1)
	waitCLIHorizon(t, reference, 8, p)
	waitCLIHorizon(t, reference, 9, p)
	p.waitExit(0, true)
	want := reference.SignedSnapshot()
	reference.CloseExecutionClient()
	if t.Failed() {
		t.FailNow()
	}
	for _, phase := range []string{"graceful-client-reopen", "sql-exit-with-client-reopen"} {
		t.Run(phase, func(t *testing.T) {
			f := orderbook.NewFullClientSignedRecoveryFixture(t)
			// Child lifecycle and failure ownership must use this exact subtest.
			p := startOwnedCLI(t, f, "", 0)
			waitCLIHorizon(t, f, 7, p)
			p.waitExit(0, true)
			f.AdmitSignedOrders()
			next := 1
			if phase == "sql-exit-with-client-reopen" {
				f.Trigger(true)
				f.Horizon(8)
				p = startOwnedCLI(t, f, "derivation-before-commit", 1)
				p.waitExit(86, false)
				f.Trigger(false)
				next = 2
			}
			f.RestartExecutionClient()
			p = startOwnedCLI(t, f, "", next)
			waitCLIHorizon(t, f, 8, p)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			orderbook.AssertRecoveryCLIState(t, f.SignedSnapshot(), want)
			f.CloseExecutionClient()
			t.Logf("full_client_signed_restore=true boundary=%s exact_canonical_tables_equal=true original_EOA_signatures_preserved=true parent_repair_or_resubmission_after_fault=false engine_close=graceful full_N04_acceptance=false", phase)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
}
