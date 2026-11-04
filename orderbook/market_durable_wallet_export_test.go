package orderbook

import (
	"bytes"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/ethereum/go-ethereum/common"
)

// The production page has already received the actual CLI's original hash.
// No source block or executed receipt is supplied before the declared kill.
func (p *ExecutedMarketProviderFixture) QueuedWalletACK(t *testing.T) (common.Hash, []byte) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	legs := p.providerLegs()
	if len(legs) != 2 || p.submissions != 1 || p.publications != 0 || p.unexpected != 0 ||
		len(legs[0].raw) == 0 || len(legs[0].raw) >= 1024 || legs[0].published != nil || legs[0].bound || legs[0].mined != 0 ||
		len(legs[1].raw) != 0 || legs[1].published != nil {
		t.Fatal("wallet ACK was not held before the first actual publication")
	}
	return legs[0].hash(), append([]byte(nil), legs[0].raw...)
}

func (p *ExecutedMarketProviderFixture) DurableWalletPublicationEvidence(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	legs := p.providerLegs()
	if len(legs) != 2 || p.submissions != 1 || p.publications != 1 || p.unexpected != 0 ||
		legs[0].published == nil || !legs[0].bound || legs[0].mined != 0 || len(legs[1].raw) != 0 || legs[1].published != nil {
		t.Fatal("queued wallet recovery changed publication cardinality or executed before derivation")
	}
	block := legs[0].published
	if block.Verify() != nil || len(block.Data) < 2 || block.Data[0] != derive.MsgBatch {
		t.Fatal("recovered wallet publication did not retain the actual account signature")
	}
	txs, err := derive.DecodeBatch(block.Data[1:], p.f.Rollup().MaxBatchBytes, true)
	if err != nil || len(txs) != 1 || !bytes.Equal(txs[0], legs[0].raw) {
		t.Fatal("recovered publication changed the originally acknowledged signed wallet bytes")
	}
	return map[string]any{"source": ExecutedMarketRelayerSource, "admissionMode": "local-sync",
		"originalWalletSubmissions": 1, "actualNoMPublications": 1, "actualAccountSignatureVerified": true,
		"originalSignedBytesPreserved": true, "boundBeforeDerivation": true, "executedBeforeDerivation": false,
		"secondWalletSubmissionBeforeFirstReceipt": false, "realWalletOrPublicNetworkOperations": 0}
}
