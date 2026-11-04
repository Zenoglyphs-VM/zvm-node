package miner

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/core/types"
)

// Publish through the actual implementation before starting the late waiter.
// Rescue the original waiter after recording its failure, then join it.
func TestZVMWaitFullCompletedBeforeWait(t *testing.T) {
	p := newPayload(context.Background(), nil, nil, nil, engine.PayloadID{})
	defer p.rpcCancel()
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	if !p.update(&newPayloadResult{block: block, fees: new(big.Int)}, 0) {
		t.Fatal("completed payload was not published")
	}
	done := make(chan struct{})
	go func() {
		p.WaitFull()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
		t.Error("late WaitFull missed an already completed payload")
	}
	// The unmodified red control needs one subsequent notification. Keep the
	// condition lock while notifying, and join before returning from the test.
	p.lock.Lock()
	p.cond.Broadcast()
	p.lock.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("original condition waiter did not join after its rescue")
	}
}
