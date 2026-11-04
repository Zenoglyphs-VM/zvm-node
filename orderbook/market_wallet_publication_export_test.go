package orderbook

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/ethereum/go-ethereum/common"
)

// The owned source accepts the actual signed block into its existing account
// history, but never writes its RPC response before the original client exits.
// The production Relayer, journal decoder and wallet are not intercepted.
type executedWalletPublicationGate struct {
	entered, finished, release         chan struct{}
	enterOnce, finishOnce, releaseOnce sync.Once
	clientCancelled                    bool
}

func (g *executedWalletPublicationGate) hold(ctx context.Context) {
	g.enterOnce.Do(func() { close(g.entered) })
	select {
	case <-ctx.Done():
		g.clientCancelled = true
	case <-g.release:
	case <-time.After(10 * time.Second):
	}
	g.finishOnce.Do(func() { close(g.finished) })
}

func (p *ExecutedMarketProviderFixture) HoldWalletPublicationACK(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.walletPublicationGate != nil || len(p.providerLegs()) != 2 || p.submissions != 0 || p.publications != 0 {
		t.Fatal("durable wallet publication gate requires a fresh two-leg fixture")
	}
	g := &executedWalletPublicationGate{entered: make(chan struct{}), finished: make(chan struct{}), release: make(chan struct{})}
	p.walletPublicationGate = g
	t.Cleanup(func() { g.releaseOnce.Do(func() { close(g.release) }) })
}

func (p *ExecutedMarketProviderFixture) AwaitWalletPublicationWithoutACK(t *testing.T, ctx context.Context) {
	t.Helper()
	p.mu.Lock()
	g := p.walletPublicationGate
	p.mu.Unlock()
	if g == nil {
		t.Fatal("durable wallet publication gate was not armed")
	}
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("owned wallet publication context ended before source acceptance")
	case <-time.After(6 * time.Second):
		t.Fatal("actual wallet publication did not enter the owned response boundary")
	}
	select {
	case <-g.finished:
		t.Fatal("original publication response boundary ended before the owned kill")
	default:
	}
}

func (p *ExecutedMarketProviderFixture) JoinHeldWalletPublication(t *testing.T, ctx context.Context) {
	t.Helper()
	p.mu.Lock()
	g := p.walletPublicationGate
	p.mu.Unlock()
	if g == nil {
		t.Fatal("durable wallet publication gate was not armed")
	}
	select {
	case <-g.finished:
	case <-ctx.Done():
		t.Fatal("owned wallet publication context ended before handler join")
	case <-time.After(3 * time.Second):
		t.Fatal("original publication HTTP handler was not joined after client kill")
	}
	if !g.clientCancelled {
		t.Fatal("original publication handler was released without the actual client closing")
	}
}

// No response is fabricated. Both blocks must be the actual program's signed
// publications; the existing source fixture remains the declared authority.
func (p *ExecutedMarketProviderFixture) WalletPublicationForRecovery(t *testing.T, index int) (common.Hash, []byte, map[string]any) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	legs := p.providerLegs()
	if len(legs) != 2 || index < 0 || index > 1 || p.submissions != index+1 || p.publications != index+1 || p.unexpected != 0 {
		t.Fatal("actual wallet publication cardinality differs before recovery")
	}
	leg := &legs[index]
	if len(leg.raw) == 0 || len(leg.raw) > 32<<10 || leg.published == nil || leg.mined != 0 || leg.bound != (index == 1) ||
		(index == 0 && (len(legs[1].raw) != 0 || legs[1].published != nil)) ||
		(index == 1 && (legs[0].published == nil || !legs[0].bound || legs[0].mined == 0)) {
		t.Fatal("actual wallet publication crossed its declared pre-execution boundary")
	}
	block := leg.published
	if block.Verify() != nil || len(block.Data) < 2 || block.Data[0] != derive.MsgBatch {
		t.Fatal("actual wallet publication signature or batch domain differs")
	}
	txs, err := derive.DecodeBatch(block.Data[1:], p.f.Rollup().MaxBatchBytes, true)
	if err != nil || len(txs) != 1 || !bytes.Equal(txs[0], leg.raw) {
		t.Fatal("actual wallet publication changed the original signed transaction")
	}
	return leg.hash(), append([]byte(nil), leg.raw...), map[string]any{
		"walletLeg": index, "originalWalletSubmissions": p.submissions, "actualNoMPublications": p.publications,
		"actualAccountSignatureVerified": true, "originalSignedBytesPreserved": true,
		"currentApplicationReceiptExecuted": false, "previousApplicationReceiptExecuted": index == 1,
		"sourceBoundBeforeRecovery": leg.bound, "completeAccountHistoryReads": p.completeHistoryReads,
		"canonicalExecutedStateNonceReads":    p.canonicalRecoveryNonces,
		"realWalletOrPublicNetworkOperations": 0,
	}
}
