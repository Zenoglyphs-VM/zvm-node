//go:build !linux

package verify

import (
	"context"
	"os/exec"
)

// Other platforms retain their original direct-child/context and pipe bounds.
// Linux process-group qualification is not transferred to these platforms.
func newCompilerCommand(ctx context.Context, binary string, arguments ...string) *exec.Cmd {
	return exec.CommandContext(ctx, binary, arguments...)
}

func runCompilerCommand(_ context.Context, cmd *exec.Cmd) error {
	return cmd.Run()
}
