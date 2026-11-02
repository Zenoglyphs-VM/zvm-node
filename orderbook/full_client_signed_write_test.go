//go:build fullclient_recovery

package orderbook_test

import (
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"testing"
)

// Keep these signed write cases separate from the existing four-minute command.
// Every owned CLI uses the same compiled binary and actual default full service.
func TestFullClientSignedWriteBoundaryRecovery(t *testing.T) {
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
	for _, phase := range []string{"index-before-commit", "book-before-commit"} {
		t.Run(phase, func(t *testing.T) { ownedSignedFullClientProcessRecoveryAt(t, want, phase) })
		if t.Failed() {
			t.FailNow()
		}
	}
}
