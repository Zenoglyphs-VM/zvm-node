//go:build fullclient_recovery

package orderbook_test

import (
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// This command preserves the original signed forward and unsigned rewind
// controls. Only source selection and the declared owned exit hook change;
// the parent never repairs canonical state or resubmits an admitted order.
func TestFullClientSignedRewindRecovery(t *testing.T) {
	reference := orderbook.NewFullClientSignedRecoveryFixture(t)
	p := startOwnedCLI(t, reference, "", 0)
	waitCLIHorizon(t, reference, 7, p)
	p.waitExit(0, true)
	reference.AdmitSignedOrders()
	p = startOwnedCLI(t, reference, "", 1)
	waitCLIHorizon(t, reference, 8, p)
	waitCLIHorizon(t, reference, 9, p)
	p.waitExit(0, true)
	want := reference.SignedSnapshot()
	reference.CloseExecutionClient()
	if t.Failed() {
		t.FailNow()
	}

	for _, phase := range []string{"derivation-rewind-before-commit", "index-rewind-before-commit"} {
		t.Run(phase, func(t *testing.T) {
			f := orderbook.NewFullClientSignedProcessFixture(t)
			// Momentum 7 is the existing common ancestor of both declared
			// synthetic source branches; signed admission is before any fault.
			f.SourceBranch(1)
			start := func(phase string, number int) *ownedCLIProcess {
				return startOwnedCLIWithTest(t, f, phase, number, "TestFullClientOwnedProcess")
			}
			p := start("", 0)
			waitCLIHorizon(t, f, 7, p)
			p.waitExit(0, true)
			orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
			f.CheckExecutionClientDirectory()
			f.RestartExecutionClient()
			f.AdmitSignedOrders()
			f.CloseExecutionClient()

			p = start("", 1)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
			f.CheckExecutionClientDirectory()
			t.Log("full_client_signed_rewind_prime=true source_branch=1 momentum=9 original_EOA_signatures_admitted_before_fault=true readonly_source_hash_and_order_state_verified=true parent_reconciliation=false")
			f.DeclareRewindAncestor()

			f.OwnedProcessTrigger(phase, true)
			f.SourceBranch(0)
			p = start(phase, 2)
			p.waitExit(86, false)
			// The unchanged checker requires the real production rewind and
			// independent RPC/direct common-ancestor header observations.
			orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 86, phase)
			f.CheckExecutionClientDirectory()
			f.OwnedProcessTrigger(phase, false)
			// Interrupted rewind can retain momentum 9, so its replacement
			// must be immediately available to production startup recovery.
			f.Horizon(9)
			p = start("", 3)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
			f.CheckExecutionClientDirectory()

			// Reopen only after every owned child joined. SignedSnapshot and
			// the original observation proof are read-only: every business
			// field/table stays exact, and only validated proof coordinates
			// may differ from sequential cold replay.
			f.RestartExecutionClient()
			got := f.SignedSnapshot()
			f.AssertRecoveryWithOrderObservations(got, want)
			f.CloseExecutionClient()
			t.Logf("full_client_signed_owned_rewind_restore=true boundary=%s actual_exit=86 retained_source_momentum=9 common_source_momentum=7 common_execution_ancestor=6 engine_head_at_exit=8 ancestor_rpc_and_direct_hashes_verified=true service_cleanup_skipped=true joined_children=4 full_service_opens=6 initial_admission_before_fault_only=true parent_repair_or_resubmission_after_fault=false canonical_business_and_tables_equal_after_validated_observation_projection=true original_EOA_signatures_preserved=true NoM_source_signatures=false kernel_kill=false hardware_power_loss=false full_N04_acceptance=false", phase)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
}
