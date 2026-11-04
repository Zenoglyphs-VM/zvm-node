//go:build fullclient_recovery

package orderbook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The original seeded workloads and raw pinned-client refusal remain separate.
// These production CLI cases retain the original empty MessagePasser storage.
func TestFullClientEmptyStorageRecovery(t *testing.T) {
	var reference map[string][]string
	var genesis string
	record := func(t *testing.T, phase string, image map[string][]string, cli, exits, services int) {
		t.Helper()
		raw, err := json.Marshal(image)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		value, err := json.Marshal(map[string]any{"phase": phase, "genesis_hash": genesis, "canonical_state_sha256": hex.EncodeToString(digest[:]),
			"canonical_image_fields": len(image), "same_original_complete_state_oracle": true, "all_owned_CLI_children_joined": cli,
			"actual_derivation_SQL_exit_86": exits, "actual_service_lifetimes_joined": services, "message_passer_storage_seeded": false,
			"parent_reconciliation": false, "raw_catalyst_reply_rewritten_by_fixture": false, "new_native_kernel_kill_or_power_loss_qualification": false,
			"real_NoM_release_genesis_platform_or_production_acceptance": false})
		if err != nil {
			t.Fatal(err)
		}
		t.Log("N04_EMPTY_STORAGE_RECOVERY " + string(value))
	}
	t.Run("cold-production-replay", func(t *testing.T) {
		f := orderbook.NewFullClientRecoveryCLIFixture(t, false)
		genesis = f.AssertEmptyMessagePasserGenesis(t)
		p := startOwnedCLI(t, f, "", 0)
		for _, height := range []uint64{7, 8, 9} {
			waitCLIHorizon(t, f, height, p)
		}
		p.waitExit(0, true)
		reference = f.Snapshot()
		f.CloseExecutionClient()
		record(t, "cold-production-replay", reference, 1, 0, 1)
	})
	if t.Failed() {
		t.FailNow()
	}
	for _, phase := range []string{"graceful-client-and-cli-reopen", "derivation-SQL-write-exit-and-client-reopen"} {
		t.Run(phase, func(t *testing.T) {
			f := orderbook.NewFullClientRecoveryCLIFixture(t, false)
			if f.AssertEmptyMessagePasserGenesis(t) != genesis {
				t.Fatal("empty-storage genesis changed between cold and restore cases")
			}
			p := startOwnedCLI(t, f, "", 0)
			waitCLIHorizon(t, f, 7, p)
			p.waitExit(0, true)
			cli, exits := 2, 0
			if phase == "derivation-SQL-write-exit-and-client-reopen" {
				f.Trigger(true)
				f.Horizon(8)
				p = startOwnedCLI(t, f, "derivation-before-commit", 1)
				p.waitExit(86, false)
				f.Trigger(false)
				cli, exits = 3, 1
			}
			f.RestartExecutionClient()
			p = startOwnedCLI(t, f, "", 2)
			if exits == 0 {
				waitCLIHorizon(t, f, 7, p)
			}
			waitCLIHorizon(t, f, 8, p)
			waitCLIHorizon(t, f, 9, p)
			p.waitExit(0, true)
			image := f.Snapshot()
			orderbook.AssertRecoveryCLIState(t, image, reference)
			f.CloseExecutionClient()
			record(t, phase, image, cli, exits, 2)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
}
