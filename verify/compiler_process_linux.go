//go:build linux

package verify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func newCompilerCommand(_ context.Context, binary string, arguments ...string) *exec.Cmd {
	// Only this invocation joins the new group. The caller's service and
	// concurrent compilers remain in their own groups.
	cmd := exec.Command(binary, arguments...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func compilerWaitUnreaped(pid, options int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|options, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func compilerStopOwnedGroup(pid int) error {
	// Never permit kill(0) or kill(-1). WNOWAIT preserves the leader's
	// waitable identity, including after normal exit, until our later Cmd.Wait.
	// No other code may wait on this private command before this function.
	if pid <= 1 {
		return errors.New("compiler process group identity unavailable")
	}
	if err := compilerWaitUnreaped(pid, unix.WNOHANG); err != nil {
		return fmt.Errorf("compiler process group ownership unavailable: %w", err)
	}
	err := unix.Kill(-pid, unix.SIGKILL)
	if err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("compiler process group stop failed: %w", err)
	}
	return nil
}

func runCompilerCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 || cmd.Cancel != nil {
		return errors.New("private compiler process group configuration required")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Observe, but do not reap: calling Cmd.Wait concurrently with group
	// cleanup would release the numeric leader/group ID before it is signaled.
	observed := make(chan error, 1)
	go func() { observed <- compilerWaitUnreaped(cmd.Process.Pid, 0) }()
	var observeErr, stopErr error
	select {
	case observeErr = <-observed:
		if observeErr == nil {
			stopErr = compilerStopOwnedGroup(cmd.Process.Pid)
		}
	case <-ctx.Done():
		stopErr = compilerStopOwnedGroup(cmd.Process.Pid)
		if stopErr != nil && !errors.Is(stopErr, unix.ECHILD) {
			// A denied group operation is an error, never successful group
			// cleanup. Still stop the owned direct child before joining it.
			_ = cmd.Process.Kill()
		}
		observeErr = <-observed
	}
	if observeErr != nil && !errors.Is(observeErr, unix.ECHILD) {
		_ = cmd.Process.Kill()
	}
	// Reap only after the one group-stop attempt. Wait also joins all pipe
	// copies under the caller's unchanged WaitDelay and capture byte limits.
	waitErr := cmd.Wait()
	if observeErr != nil || stopErr != nil {
		return errors.Join(observeErr, stopErr, waitErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return waitErr
}
