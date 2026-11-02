//go:build fullclient_recovery

package orderbook_test

import (
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

func ownedSignedFullClientColdBackupRecovery(t *testing.T, want map[string][]string) {
	t.Helper()
	const phase = "derivation-before-commit"
	source := orderbook.NewFullClientSignedProcessFixture(t)
	p := startOwnedCLIWithTest(t, source, "", 0, "TestFullClientOwnedProcess")
	waitCLIHorizon(t, source, 7, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	source.CheckExecutionClientDirectory()
	source.RestartExecutionClient()
	source.AdmitSignedOrders()
	source.CloseExecutionClient()
	source.OwnedProcessTrigger(phase, true)
	source.Horizon(8)
	p = startOwnedCLIWithTest(t, source, phase, 1, "TestFullClientOwnedProcess")
	p.waitExit(86, false)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 86, phase)
	source.CheckExecutionClientDirectory()
	source.OwnedProcessTrigger(phase, false)
	// Both exact source children joined. The helper closes the mode=ro SQL
	// observer, preserves a bounded raw pair and restores independent files.
	backup := source.ColdPersistenceBackup()
	restored := orderbook.RestoreFullClientSignedColdBackup(t, backup)
	restored.Horizon(8)
	p = startOwnedCLIWithTest(t, restored, "", 0, "TestFullClientOwnedProcess")
	waitCLIHorizon(t, restored, 8, p)
	waitCLIHorizon(t, restored, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	restored.CheckExecutionClientDirectory()
	// Production startup, derivation, indexing and the original book timers
	// perform recovery. The parent only reopens after the recovery child joined
	// and applies the unchanged exact signature/business/all-table oracle.
	restored.RestartExecutionClient()
	orderbook.AssertRecoveryCLIState(t, restored.SignedSnapshot(), want)
	restored.CloseExecutionClient()
	orderbook.AssertFullClientColdBackupUnchanged(t, backup)
	t.Log("full_client_signed_paired_backup_restore=true boundary=derivation-before-commit actual_exit=86 engine_head_at_exit=7 source_children_joined=2 restored_children_joined=1 full_service_opens=5 original_EOA_signatures_preserved=true all_six_original_canonical_categories_equal=true backup_logical_limit=268435456 no_parent_repair_or_resubmission=true NoM_source_signatures=false online_backup=false kernel_kill=false hardware_power_loss=false full_N04_acceptance=false")
}
