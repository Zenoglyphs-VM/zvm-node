//go:build fullclient_recovery && linux

package orderbook

import (
	"fmt"
	"strings"
	"testing"
)

// The same service, genesis, Engine/eth observations and production arguments
// are used. Only the boundary callback's requested termination label differs.
func OwnedFullClientKernelKillCLIArgs(t *testing.T, directory string, args []string, phase string) ([]string, func()) {
	t.Helper()
	return ownedFullClientCLIArgs(t, directory, args, phase, true)
}

func AssertOwnedFullClientKernelKillProcess(t *testing.T, output, phase string) {
	t.Helper()
	count := func(want string) int {
		n := 0
		for _, line := range strings.Split(output, "\n") {
			if line == want {
				n++
			}
		}
		return n
	}
	boundary := fmt.Sprintf("owned_full_client_boundary phase=%s engine_head=%d service_responding=true requested_action=parent-SIGKILL", phase, ownedFullClientBoundaryHead(phase))
	index := 0
	if phase == "index-before-commit" {
		index = 1
	}
	if count("owned_full_client_started "+fullClientDefaultsProfile) != 1 ||
		strings.Contains(output, "owned_full_client_closed ") || count(boundary) != 1 ||
		strings.Count(output, "owned_full_client_boundary phase=") != 1 ||
		count("owned_index_plugin_before_commit block=7 transaction_visible=true real_nft_plugin_completed=true") != index ||
		strings.Contains(output, "actual_exit_requested=86") || strings.Contains(output, "owned_full_client_boundary_refused ") {
		t.Fatalf("owned kernel-killed full-client lifecycle or live boundary differs: %s", output)
	}
	if phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit" {
		ancestor := fmt.Sprintf("owned_full_client_rewind_ancestor phase=%s source_momentum=7 execution_block=6 rpc_and_direct_hashes_verified=true", phase)
		if !strings.Contains(output, "NoM reorganised; rewinding svc=deriver from=9 to=7") || count(ancestor) != 1 || strings.Count(output, "owned_full_client_rewind_ancestor phase=") != 1 {
			t.Fatal("kernel-killed full client lacks its production rewind and exact common-ancestor proof")
		}
	}
	t.Logf("owned_default_full_client_kernel_child boundary=%s default_profile_verified=true engine_head=%d service_cleanup_skipped=true colocated_cli_and_engine=true rewind_ancestor_verified=%t", phase, ownedFullClientBoundaryHead(phase), phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit")
}
