//go:build linux

package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func compilerGroupQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func compilerGroupScript(marker, mode string) string {
	redirection := " </dev/null >/dev/null 2>&1"
	if mode == "parent-exit-held-pipes" {
		redirection = " </dev/null"
	}
	script := "/bin/sleep 3" + redirection + " &\nchild=$!\n" +
		"printf '%s %s\\n' \"$$\" \"$child\" > " + compilerGroupQuote(marker) + "\n" +
		"/bin/cat /proc/\"$child\"/stat >> " + compilerGroupQuote(marker) + "\n"
	switch mode {
	case "parent-exit-held-pipes", "parent-exit-closed-pipes":
		return script + "printf '{\"contracts\":{}}'\nexit 0"
	case "stdout-limit":
		return script + "while :; do printf 0123456789abcdef; done"
	case "stderr-limit":
		return script + "while :; do printf 0123456789abcdef >&2; done"
	default:
		return script + "wait \"$child\""
	}
}

func compilerGroupStat(raw []byte) (state string, group int, err error) {
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", 0, errors.New("missing bounded process state")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 3 {
		return "", 0, errors.New("incomplete bounded process state")
	}
	group, err = strconv.Atoi(fields[2])
	return fields[0], group, err
}

func compilerGroupMarker(t *testing.T, path string) (parent, child int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) <= 4096 {
			lines := strings.SplitN(string(raw), "\n", 2)
			if len(lines) == 2 {
				var extra string
				count, _ := fmt.Sscanf(lines[0], "%d %d %s", &parent, &child, &extra)
				state, group, err := compilerGroupStat([]byte(lines[1]))
				if count == 2 && parent > 1 && child > 1 && parent != child && err == nil && group == parent && state != "" {
					return parent, child
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("owned compiler did not record its actual same-group child")
	return 0, 0
}

func compilerGroupProcGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func compilerGroupChildStopped(t *testing.T, parent, child int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", child))
		if compilerGroupProcGone(err) {
			return
		}
		if err != nil {
			time.Sleep(time.Millisecond)
			continue
		}
		if len(raw) > 4096 {
			t.Fatal("owned compiler child state could not be established")
		}
		state, group, err := compilerGroupStat(raw)
		if err != nil || group != parent {
			t.Logf("OWNED-COMPILER-GROUP-OBSERVATION parseOK=%t groupMatches=%t groupIsZero=%t terminalState=%t emptyRead=%t observedBytes=%d observationBoundMs=300 originalFailurePreserved=true",
				err == nil, group == parent, group == 0, state == "Z" || state == "X", len(raw) == 0, len(raw))
			t.Fatal("owned compiler child left its declared process group")
		}
		if state == "Z" || state == "X" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("same-group compiler child stop was not established within the original observation bound")
}

func TestCompilerProcessGroupCleanup(t *testing.T) {
	for _, mode := range []string{"parent-exit-held-pipes", "parent-exit-closed-pipes", "caller-cancel", "deadline", "stdout-limit", "stderr-limit"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "owned-child.pid")
			runner := fixtureLocalRunner(t, compilerGroupScript(marker, mode))
			runner.Timeout = 2 * time.Second
			if mode == "deadline" {
				runner.Timeout = 600 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				output []byte
				err    error
			}
			finished := make(chan result, 1)
			started := time.Now()
			go func() { out, err := runner.Compile(ctx, "0.0.2", []byte(`{}`)); finished <- result{out, err} }()
			parent, child := compilerGroupMarker(t, marker)
			if mode == "caller-cancel" {
				cancel()
			}
			select {
			case actual := <-finished:
				switch mode {
				case "parent-exit-held-pipes", "parent-exit-closed-pipes":
					if actual.err != nil || string(actual.output) != `{"contracts":{}}` {
						t.Fatal("normal compiler output changed or inherited pipes retained a child")
					}
				case "caller-cancel":
					if !errors.Is(actual.err, context.Canceled) || len(actual.output) != 0 {
						t.Fatal("caller cancellation lost its original failure")
					}
				case "deadline":
					if !errors.Is(actual.err, context.DeadlineExceeded) || len(actual.output) != 0 {
						t.Fatal("compiler deadline lost its original failure")
					}
				default:
					if !errors.Is(actual.err, ErrCompilerOutputLimit) || len(actual.output) != 0 {
						t.Fatal("compiler overflow returned partial success")
					}
				}
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("compiler request failed to join before the fixture child could exit naturally")
			}
			if time.Since(started) >= 2*time.Second || len(runner.slots) != 0 {
				t.Fatal("compiler request retained its time or capacity reservation")
			}
			compilerGroupChildStopped(t, parent, child)
			t.Log("OWNED-COMPILER-GROUP-CLEANUP mode=" + mode + " sameGroupChildStopped=true leaderJoined=true originalFailureOrSuccessPreserved=true escapedOrAdoptedDescendantsQualified=false")
		})
	}
}

func TestCompilerConcurrentGroupsDoNotCrossCancel(t *testing.T) {
	firstPath, secondPath := filepath.Join(t.TempDir(), "first.pid"), filepath.Join(t.TempDir(), "second.pid")
	first, second := fixtureLocalRunner(t, compilerGroupScript(firstPath, "peer")), fixtureLocalRunner(t, compilerGroupScript(secondPath, "peer"))
	first.Timeout, second.Timeout = 2*time.Second, 2*time.Second
	first.MaxConcurrent, second.MaxConcurrent = 1, 1
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	joinedA, joinedB := make(chan error, 1), make(chan error, 1)
	go func() { _, err := first.Compile(ctxA, "0.0.2", []byte(`{}`)); joinedA <- err }()
	go func() { _, err := second.Compile(ctxB, "0.0.2", []byte(`{}`)); joinedB <- err }()
	parentA, childA := compilerGroupMarker(t, firstPath)
	parentB, childB := compilerGroupMarker(t, secondPath)
	if parentA == parentB {
		t.Fatal("concurrent compilers share a process group")
	}
	cancelA()
	select {
	case err := <-joinedA:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("first compiler did not join its cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("first compiler did not join")
	}
	compilerGroupChildStopped(t, parentA, childA)
	select {
	case <-joinedB:
		t.Fatal("cancelled compiler stopped its independent peer")
	default:
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", childB))
	if err != nil {
		t.Fatal("independent peer child disappeared")
	}
	state, group, err := compilerGroupStat(raw)
	if err != nil || group != parentB || state == "Z" || state == "X" {
		t.Fatal("independent peer child was stopped")
	}
	if _, err := second.Compile(context.Background(), "0.0.2", []byte(`{}`)); !errors.Is(err, ErrCompilerBusy) {
		t.Fatal("peer lost its own independent capacity reservation")
	}
	cancelB()
	select {
	case err := <-joinedB:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("peer compiler did not join its own cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("peer compiler did not join")
	}
	compilerGroupChildStopped(t, parentB, childB)
	if len(first.slots) != 0 || len(second.slots) != 0 {
		t.Fatal("compiler peer left a capacity reservation")
	}
	t.Log("OWNED-COMPILER-GROUP-PEERS independentGroups=true cancelledChildStopped=true peerStayedLive=true bothLeadersJoined=true")
}

func TestCompilerProcessGroupStartFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	missing := filepath.Join(t.TempDir(), "missing-owned-compiler")
	cmd := newCompilerCommand(ctx, missing)
	if err := runCompilerCommand(ctx, cmd); !errors.Is(err, context.Canceled) || cmd.Process != nil {
		t.Fatal("cancelled compile launched a child")
	}
	cmd = newCompilerCommand(context.Background(), missing)
	if err := runCompilerCommand(context.Background(), cmd); !errors.Is(err, os.ErrNotExist) || cmd.Process != nil {
		t.Fatal("failed compiler launch acquired a process group")
	}
	t.Log("OWNED-COMPILER-GROUP-START cancelledAndFailedLaunchesHaveNoProcess=true")
	for _, err := range []error{os.ErrNotExist, syscall.ESRCH, &os.PathError{Op: "read", Path: "owned-proc", Err: syscall.ESRCH}} {
		if !compilerGroupProcGone(err) {
			t.Fatal("kernel task disappearance was not recognized")
		}
	}
	for _, err := range []error{nil, os.ErrPermission, syscall.EIO, syscall.EINTR, context.Canceled} {
		if compilerGroupProcGone(err) {
			t.Fatal("unestablished read or cancellation was treated as a stopped child")
		}
	}
	t.Log("OWNED-COMPILER-GROUP-PROC-CONTROLS cases=8 disappearanceOnly=true permissionIOInterruptedAndCancellationRefused=true")

}
