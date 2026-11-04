package orderbook

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

const ExecutedMarketRelayerSource = "f5d55cce233a90fa962837671b0371004ea56a12"
const ExecutedMarketRelayerRepository = "edgepillar/zvm-relayer"

// The selected blocks are the actual program's signed publications. The
// adapter implements account selection and full frontier counts; it does not
// supply a recovered queue or invent an execution/confirmation decision.
func executedRelayerAccountResponse(blocks []*znn.AccountBlock, method string, params []json.RawMessage) (any, error) {
	if len(blocks) > 256 {
		return nil, errors.New("owned account fixture history exceeds its bound")
	}
	address := devnet.RelayerKey(0).Address
	var previous znn.Hash
	for i, block := range blocks {
		if block == nil || block.Address != address || block.Height != uint64(i+1) || block.PreviousHash != previous || block.Hash != block.ComputeHash() {
			return nil, errors.New("owned account fixture is not a complete unchanged chain")
		}
		previous = block.Hash
	}
	if method == "ledger.getAccountBlockByHash" {
		var hash znn.Hash
		if len(params) != 1 || json.Unmarshal(params[0], &hash) != nil {
			return nil, errors.New("one exact account hash is required")
		}
		for _, block := range blocks {
			if block.Hash == hash {
				return block, nil
			}
		}
		return nil, nil
	}
	var selected znn.Address
	if len(params) == 0 || json.Unmarshal(params[0], &selected) != nil || selected != address {
		return nil, errors.New("undeclared relayer account")
	}
	switch method {
	case "ledger.getFrontierAccountBlock":
		if len(params) != 1 {
			return nil, errors.New("one account is required")
		}
		if len(blocks) == 0 {
			return nil, nil
		}
		return blocks[len(blocks)-1], nil
	case "ledger.getAccountBlocksByHeight":
		var start, count uint64
		if len(params) != 3 || json.Unmarshal(params[1], &start) != nil || json.Unmarshal(params[2], &count) != nil || start == 0 || count == 0 || count > 64 {
			return nil, errors.New("bounded account height and count required")
		}
		result := &znn.BlockList{Count: uint64(len(blocks)), List: []*znn.AccountBlock{}}
		for _, block := range blocks {
			if block.Height >= start && block.Height-start < count {
				result.List = append(result.List, block)
			}
		}
		return result, nil
	case "ledger.getUnconfirmedBlocksByAddress":
		var page, size uint64
		if len(params) != 3 || json.Unmarshal(params[1], &page) != nil || json.Unmarshal(params[2], &size) != nil || size == 0 || size > 64 || page > (^uint64(0))/size {
			return nil, errors.New("bounded unconfirmed page and size required")
		}
		all := []*znn.AccountBlock{}
		for _, block := range blocks {
			if block.ConfirmationDetail == nil {
				all = append(all, block)
			}
		}
		result := &znn.BlockList{Count: uint64(len(all)), List: []*znn.AccountBlock{}}
		start := page * size
		for i, block := range all {
			if uint64(i) >= start && uint64(i)-start < size {
				result.List = append(result.List, block)
			}
		}
		return result, nil
	default:
		return nil, errors.New("undeclared relayer account operation")
	}
}

// Called only after the first application receipt was observed as executed,
// and after the new production process reports complete recovery readiness.
func (p *ExecutedMarketProviderFixture) RelayerRecoveryEvidence(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	legs := p.providerLegs()
	if len(legs) != 2 || legs[0].published == nil || !legs[0].bound || legs[0].mined == 0 || legs[1].published != nil ||
		p.submissions != 1 || p.publications != 1 || p.unexpected != 0 || p.completeHistoryReads < 1 || p.canonicalRecoveryNonces < 1 {
		t.Fatal("actual market relayer did not recover the complete executed first account block before another wallet submission")
	}
	return map[string]any{"source": ExecutedMarketRelayerSource, "repository": ExecutedMarketRelayerRepository,
		"completeAccountHistoryReads": p.completeHistoryReads, "canonicalExecutedStateNonceReads": p.canonicalRecoveryNonces,
		"frontierHeight": 1, "actualFirstAccountSignaturePreviouslyVerified": true, "actualFirstApplicationReceiptObserved": true,
		"signedSubmissionsBeforeAndAfterReopen": 1, "NoMPublicationsBeforeAndAfterReopen": 1,
		"duplicateSubmissionOrPublication": false, "nativeBuySubstitutions": 0, "realWalletOrPublicNetworkOperations": 0}
}
