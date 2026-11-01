//go:build fullclient_recovery && linux

package orderbook_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The original declaration, endpoints, full service and CLI validation are
// unchanged. A boundary callback parks this exact child until its parent kills
// and joins it; the original exit-86 scenarios keep their existing callbacks.
func TestFullClientOwnedKernelKillProcess(t *testing.T) {
	raw := os.Getenv("ZVM_OWNED_CLI_CHILD")
	if raw == "" {
		t.Skip("owned full-client kernel helper requires its parent declaration")
	}
	runOwnedCLIChildWithSetup(t, raw, func(t *testing.T, cfg ownedCLIConfig) ([]string, func()) {
		if cfg.Phase == "" {
			t.Fatal("kernel helper requires a declared write boundary")
		}
		args, observe := orderbook.OwnedFullClientKernelKillCLIArgs(t, cfg.Directory, cfg.Args, cfg.Phase)
		return args, func() {
			observe()
			fmt.Printf("owned_full_client_kernel_boundary phase=%s declaration_sha256=%x pid=%d action=await-parent-SIGKILL\n", cfg.Phase, sha256.Sum256([]byte(raw)), os.Getpid())
			// No deferred service/SQL cleanup can run before the requested kill.
			// Background service activity is not represented as a quiescent DB.
			select {}
		}
	})
}

func ownedFullClientKernelMarker(output, want string) (bool, error) {
	count := 0
	// Only complete lines admit a kill. A partial stream waits for more bytes.
	lines := strings.Split(output, "\n")
	for _, line := range lines[:len(lines)-1] {
		if strings.HasPrefix(line, "owned_full_client_kernel_boundary ") {
			if line != want {
				return false, errors.New("owned kernel boundary declaration differs")
			}
			count++
		}
	}
	if count > 1 {
		return false, errors.New("duplicate owned kernel boundary marker")
	}
	return count == 1, nil
}

func TestOwnedFullClientKernelMarkerControls(t *testing.T) {
	want := "owned_full_client_kernel_boundary phase=book-before-commit declaration_sha256=abc pid=123 action=await-parent-SIGKILL"
	for _, tc := range []struct {
		name, output   string
		ready, refused bool
	}{
		{"absent", "other output\n", false, false},
		{"partial", want, false, false},
		{"exact", want + "\n", true, false},
		{"preceding service output", "other output\n" + want + "\n", true, false},
		{"duplicate", want + "\n" + want + "\n", false, true},
		{"phase", strings.Replace(want, "book-before-commit", "index-before-commit", 1) + "\n", false, true},
		{"declaration", strings.Replace(want, "sha256=abc", "sha256=def", 1) + "\n", false, true},
		{"pid", strings.Replace(want, "pid=123", "pid=124", 1) + "\n", false, true},
		{"action", strings.Replace(want, "parent-SIGKILL", "parent-SIGTERM", 1) + "\n", false, true},
	} {
		ready, err := ownedFullClientKernelMarker(tc.output, want)
		if ready != tc.ready || (err != nil) != tc.refused {
			t.Fatalf("kernel boundary control failed: %s", tc.name)
		}
	}
	t.Log("owned_full_client_kernel_marker_controls cases=9 exact=true absent_and_partial_wait=true phase_declaration_pid_action_and_duplicate_refused=true")
}

func waitOwnedFullClientKernelKill(t *testing.T, f ownedCLIFixture, p *ownedCLIProcess, number int) {
	t.Helper()
	marker := filepath.Join(f.Directory(), fmt.Sprintf("owned-cli-%d.json", number))
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 8192 {
		t.Fatal("kernel parent lacks its bounded original declaration")
	}
	raw, err := os.ReadFile(marker)
	current, statErr := os.Lstat(marker)
	if err != nil || statErr != nil || !os.SameFile(info, current) || int64(len(raw)) != info.Size() {
		t.Fatal("kernel parent declaration changed")
	}
	want := fmt.Sprintf("owned_full_client_kernel_boundary phase=%s declaration_sha256=%x pid=%d action=await-parent-SIGKILL", p.phase, sha256.Sum256(raw), p.cmd.Process.Pid)
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	for !ready {
		select {
		case err := <-p.done:
			p.joined = true
			t.Fatalf("owned full client exited before its kernel boundary: %v output=%s", err, p.output.snapshot())
		case <-deadline.C:
			t.Fatalf("owned full client did not reach its declared kernel boundary: %s", p.output.snapshot())
		case <-p.ctx.Done():
			t.Fatal("owned full-client kernel boundary exceeded its process deadline")
		case <-ticker.C:
			var markerErr error
			ready, markerErr = ownedFullClientKernelMarker(p.output.snapshot(), want)
			if markerErr != nil {
				t.Fatal(markerErr)
			}
		}
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal("could not send SIGKILL to the exact owned full client")
	}
	joined := time.NewTimer(10 * time.Second)
	defer joined.Stop()
	select {
	case err = <-p.done:
		p.joined = true
	case <-joined.C:
		t.Fatal("owned full client did not join after SIGKILL")
	}
	var failure *exec.ExitError
	if !errors.As(err, &failure) {
		t.Fatalf("owned full client did not report a signal exit: %v", err)
	}
	status, ok := failure.Sys().(syscall.WaitStatus)
	output := p.output.snapshot()
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || p.ctx.Err() != nil || p.output.truncated ||
		strings.Contains(output, "DATA RACE") || strings.Contains(output, "race detected during execution") ||
		strings.Count(output, want+"\n") != 1 || strings.Count(output, "owned_full_client_kernel_boundary ") != 1 ||
		strings.Contains(output, `"owned_cli_boundary":`) || strings.Contains(output, `"owned_cli_return":`) {
		t.Fatalf("owned full client kernel outcome differs: status=%v output=%s", failure.Sys(), output)
	}
	orderbook.AssertOwnedFullClientKernelKillProcess(t, output, p.phase)
	p.cancel()
	t.Logf("owned_full_client_kernel_join boundary=%s signal=SIGKILL requested_by_parent=true exact_child=true joined=true declaration_bound=true output_complete=true early_exit_refused=true cleanup_skipped=true", p.phase)
}

func fullClientKernelKillRecovery(t *testing.T, want map[string][]string, phase string) {
	t.Helper()
	f := orderbook.NewFullClientRecoveryProcessFixture(t)
	rewind := phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit"
	if rewind {
		f.SourceBranch(1)
	}
	p := startOwnedCLIWithTest(t, f, "", 0, "TestFullClientOwnedProcess")
	if rewind {
		waitCLIHorizon(t, f, 9, p)
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
	p = startOwnedCLIWithTest(t, f, phase, 1, "TestFullClientOwnedKernelKillProcess")
	waitOwnedFullClientKernelKill(t, f, p, 1)
	f.CheckExecutionClientDirectory()
	// Remove only the declared fixture hook after the exact engine/CLI child
	// joined. No canonical rows are repaired by the parent; ordinary SQLite
	// recovery can occur when the fixture hook is removed, before startup.
	f.OwnedProcessTrigger(phase, false)
	if rewind {
		f.Horizon(9)
	} else {
		f.Horizon(8)
	}
	p = startOwnedCLIWithTest(t, f, "", 2, "TestFullClientOwnedProcess")
	if !rewind {
		waitCLIHorizon(t, f, 8, p)
	}
	waitCLIHorizon(t, f, 9, p)
	p.waitExit(0, true)
	orderbook.AssertOwnedFullClientProcess(t, p.output.snapshot(), 0, phase)
	f.CheckExecutionClientDirectory()
	// Open an engine only after all three exact children have joined, solely
	// for the existing read-only canonical six-category chain/SQL oracle.
	f.RestartExecutionClient()
	got := f.Snapshot()
	if rewind {
		f.AssertRecoveryWithOrderObservations(got, want)
	} else {
		orderbook.AssertRecoveryCLIState(t, got, want)
	}
	f.CloseExecutionClient()
	t.Logf("owned_full_client_kernel_restore boundary=%s canonical_state_equal=true default_path_persistence=true parent_canonical_row_writes=false fixture_hook_removed=true source_horizons=7,8,9 seeded_synthetic_genesis=true standalone_daemon=false signed_lane=false hardware_power_loss=false full_N04_acceptance=false", phase)
}

func TestFullClientKernelKillRecovery(t *testing.T) {
	// One uninterrupted reference is shared by the sequential declared cases.
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
	for _, phase := range []string{"derivation-before-commit", "index-before-commit", "book-before-commit", "derivation-rewind-before-commit", "index-rewind-before-commit"} {
		t.Run(phase, func(t *testing.T) { fullClientKernelKillRecovery(t, want, phase) })
		if t.Failed() {
			t.FailNow()
		}
	}
}
