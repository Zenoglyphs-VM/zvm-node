//go:build fullclient_recovery && linux

package orderbook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

func TestFullClientOwnedEmptyProcess(t *testing.T) {
	raw := os.Getenv("ZVM_OWNED_CLI_CHILD")
	if raw == "" {
		t.Skip("owned empty-client helper requires its parent declaration")
	}
	runOwnedCLIChildWithSetup(t, raw, func(t *testing.T, cfg ownedCLIConfig) ([]string, func()) {
		return orderbook.OwnedFullClientEmptyCLIArgs(t, cfg.Directory, cfg.Args, cfg.Phase)
	})
}

func TestFullClientOwnedEmptyKernelKillProcess(t *testing.T) {
	raw := os.Getenv("ZVM_OWNED_CLI_CHILD")
	if raw == "" {
		t.Skip("owned empty-client kernel helper requires its parent declaration")
	}
	runOwnedCLIChildWithSetup(t, raw, func(t *testing.T, cfg ownedCLIConfig) ([]string, func()) {
		if cfg.Phase == "" {
			t.Fatal("empty-client kernel helper requires a declared write boundary")
		}
		args, observe := orderbook.OwnedFullClientEmptyKernelKillCLIArgs(t, cfg.Directory, cfg.Args, cfg.Phase)
		return args, func() {
			observe()
			fmt.Printf("owned_full_client_kernel_boundary phase=%s declaration_sha256=%x pid=%d action=await-parent-SIGKILL\n", cfg.Phase, sha256.Sum256([]byte(raw)), os.Getpid())
			select {}
		}
	})
}

func emptyKernelImageSHA(t *testing.T, image map[string][]string) string {
	t.Helper()
	raw, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func emptyFullClientKernelKillRecovery(t *testing.T, want map[string][]string, genesis, phase string) {
	t.Helper()
	f := orderbook.NewFullClientEmptyRecoveryProcessFixture(t)
	rewind := phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit"
	if rewind {
		f.SourceBranch(1)
	}
	p := startOwnedCLIWithTest(t, f, "", 0, "TestFullClientOwnedEmptyProcess")
	if rewind {
		waitCLIHorizon(t, f, 9, p)
	} else {
		waitCLIHorizon(t, f, 7, p)
	}
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.AssertOwnedEmptyFullClientContext(t, p.output.snapshot())
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
	p = startOwnedCLIWithTest(t, f, phase, 1, "TestFullClientOwnedEmptyKernelKillProcess")
	// This reuses the original PID/declaration/phase marker, actual signal,
	// bounded output, exact SIGKILL exit and join validator without alteration.
	waitOwnedFullClientKernelKill(t, f, p, 1)
	f.AssertOwnedEmptyFullClientContext(t, p.output.snapshot())
	f.CheckExecutionClientDirectory()
	f.OwnedProcessTrigger(phase, false)
	if rewind {
		f.Horizon(9)
	} else {
		f.Horizon(8)
	}
	p = startOwnedCLIWithTest(t, f, "", 2, "TestFullClientOwnedEmptyProcess")
	if !rewind {
		waitCLIHorizon(t, f, 8, p)
	}
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.AssertOwnedEmptyFullClientContext(t, p.output.snapshot())
	f.CheckExecutionClientDirectory()
	// All three actual service/CLI children have joined before this read-only
	// observer opens the same persistence directory. No parent repair is made.
	f.RestartExecutionClient()
	if f.AssertEmptyKernelClientContext(t) != genesis {
		t.Fatal("empty kernel genesis differs from its cold reference")
	}
	got := f.Snapshot()
	oracle := "original-complete-forward-state"
	if rewind {
		f.AssertRecoveryWithOrderObservations(got, want)
		oracle = "original-complete-rewind-state-with-order-observations"
	} else {
		orderbook.AssertRecoveryCLIState(t, got, want)
	}
	f.CloseExecutionClient()
	value, err := json.Marshal(map[string]any{"phase": phase, "genesis_hash": genesis, "reference_state_sha256": emptyKernelImageSHA(t, want),
		"restored_state_sha256": emptyKernelImageSHA(t, got), "complete_state_fields": len(got), "state_oracle": oracle,
		"all_original_canonical_business_and_SQL_checks_required": true, "rewind_order_observations_retained": rewind,
		"joined_CLI_children": 3, "default_service_lifetimes_joined": 4, "actual_whole_client_SIGKILL_joins": 1,
		"empty_storage_child_contexts_verified": 3, "actual_signal_and_parent_marker_validator_unchanged": true,
		"message_passer_storage_seeded": false, "parent_canonical_row_repair": false, "fixture_hook_removed_after_child_join": true,
		"real_NoM_release_genesis_platform_or_hardware_acceptance": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("N04_EMPTY_KERNEL_RESTORE " + string(value))
}

func TestFullClientEmptyStorageKernelKillRecovery(t *testing.T) {
	reference := orderbook.NewFullClientRecoveryCLIFixture(t, false)
	genesis := reference.AssertEmptyKernelClientContext(t)
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
	value, err := json.Marshal(map[string]any{"genesis_hash": genesis, "complete_state_fields": len(want), "state_sha256": emptyKernelImageSHA(t, want),
		"joined_CLI_children": 1, "default_service_lifetimes_joined": 1, "message_passer_storage_seeded": false, "original_complete_state_oracle_required": true})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("N04_EMPTY_KERNEL_REFERENCE " + string(value))
	for _, phase := range []string{"derivation-before-commit", "index-before-commit", "book-before-commit", "derivation-rewind-before-commit", "index-rewind-before-commit"} {
		t.Run(phase, func(t *testing.T) { emptyFullClientKernelKillRecovery(t, want, genesis, phase) })
		if t.Failed() {
			t.FailNow()
		}
	}
}
