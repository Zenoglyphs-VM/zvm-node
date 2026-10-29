//go:build fullclient_recovery

package orderbook_test

import (
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// This explicit, serialized lane retains full client cache/persistence defaults.
// The storage/reopen cases use a declared nonempty synthetic MessagePasser slot;
// shipped empty genesis remains a required separate compatibility refusal.
func TestFullClientDefaultPersistenceRecovery(t *testing.T) {
	t.Run("shipped-empty-message-passer-refusal", orderbook.AssertFullClientEmptyMessagePasserRefused)
	if t.Failed() {
		t.FailNow()
	}
	reference := orderbook.NewFullClientRecoveryCLIFixture(t, true)
	p := startOwnedCLI(t, reference, "", 0)
	for _, height := range []uint64{7, 8, 9} {
		waitCLIHorizon(t, reference, height, p)
	}
	p.waitExit(0, true)
	want := reference.Snapshot()
	reference.CloseExecutionClient()
	if t.Failed() {
		t.FailNow()
	}
	for _, phase := range []string{"graceful-client-and-cli-reopen", "sql-exit-with-client-reopen"} {
		t.Run(phase, func(t *testing.T) {
			f := orderbook.NewFullClientRecoveryCLIFixture(t, true)
			p := startOwnedCLI(t, f, "", 0)
			waitCLIHorizon(t, f, 7, p)
			p.waitExit(0, true)
			if phase == "sql-exit-with-client-reopen" {
				f.Trigger(true)
				f.Horizon(8)
				p = startOwnedCLI(t, f, "derivation-before-commit", 1)
				p.waitExit(86, false)
				f.Trigger(false)
			}
			f.RestartExecutionClient()
			p = startOwnedCLI(t, f, "", 2)
			if phase == "graceful-client-and-cli-reopen" {
				waitCLIHorizon(t, f, 7, p)
			}
			waitCLIHorizon(t, f, 8, p)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			orderbook.AssertRecoveryCLIState(t, f.Snapshot(), want)
			f.CloseExecutionClient()
			t.Log("full_client_reopen=true readonly_canonical_state_equal=true parent_reconciliation=false seeded_synthetic_genesis=true full_N04_acceptance=false")
		})
		if t.Failed() {
			t.FailNow()
		}
	}
	t.Run("owned-client-process-exit", func(t *testing.T) {
		ownedFullClientProcessRecovery(t, want, "derivation-before-commit")
	})
	for _, phase := range []string{"index-before-commit", "book-before-commit", "derivation-rewind-before-commit", "index-rewind-before-commit"} {
		t.Run("owned-"+phase, func(t *testing.T) {
			ownedFullClientProcessRecovery(t, want, phase)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
	t.Run("owned-legacy-closed-recovery", func(t *testing.T) {
		ownedFullClientLegacyClosureRecovery(t, want)
	})
}
