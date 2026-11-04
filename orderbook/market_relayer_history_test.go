package orderbook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func relayerAccountParams(values ...any) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for i, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		result[i] = raw
	}
	return result
}

func relayerHistoryVector(count int) []*znn.AccountBlock {
	result := make([]*znn.AccountBlock, count)
	var previous znn.Hash
	for i := range result {
		block := &znn.AccountBlock{Version: 1, ChainIdentifier: 70, BlockType: znn.BlockTypeUserSend,
			Address: devnet.RelayerKey(0).Address, Height: uint64(i + 1), PreviousHash: previous, Data: []byte{byte(i)}}
		block.Hash = block.ComputeHash()
		previous = block.Hash
		result[i] = block
	}
	return result
}

func TestExecutedRelayerHistoryPagesRetainFullFrontierHeight(t *testing.T) {
	blocks := relayerHistoryVector(130)
	for _, test := range []struct {
		name               string
		start, count, want uint64
	}{{"first", 1, 64, 64}, {"middle", 65, 64, 64}, {"last", 129, 64, 2}, {"past_frontier", 131, 1, 0}} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := executedRelayerAccountResponse(blocks, "ledger.getAccountBlocksByHeight", relayerAccountParams(devnet.RelayerKey(0).Address, test.start, test.count))
			if err != nil {
				t.Fatal(err)
			}
			page := raw.(*znn.BlockList)
			if page.Count != 130 || page.More || uint64(len(page.List)) != test.want {
				t.Fatal("page length replaced the full frontier height")
			}
			for i, block := range page.List {
				if block != blocks[int(test.start)-1+i] {
					t.Fatal("account page changed source order or bytes")
				}
			}
		})
	}
}

func TestExecutedRelayerAccountSelectionRefusesForeignOrIncompleteHistory(t *testing.T) {
	blocks := relayerHistoryVector(2)
	address := devnet.RelayerKey(0).Address
	for _, test := range []struct {
		name, method string
		params       []json.RawMessage
	}{
		{"foreign_account", "ledger.getFrontierAccountBlock", relayerAccountParams(devnet.RelayerKey(1).Address)},
		{"missing_hash", "ledger.getAccountBlockByHash", nil},
		{"zero_height", "ledger.getAccountBlocksByHeight", relayerAccountParams(address, 0, 1)},
		{"zero_count", "ledger.getAccountBlocksByHeight", relayerAccountParams(address, 1, 0)},
		{"large_count", "ledger.getAccountBlocksByHeight", relayerAccountParams(address, 1, 65)},
		{"page_overflow", "ledger.getUnconfirmedBlocksByAddress", relayerAccountParams(address, ^uint64(0), 2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := executedRelayerAccountResponse(blocks, test.method, test.params); err == nil {
				t.Fatal("invalid account selection accepted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		blocks []*znn.AccountBlock
	}{
		{"gap", blocks[1:]}, {"nil_block", []*znn.AccountBlock{nil}}, {"reordered", []*znn.AccountBlock{blocks[1], blocks[0]}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := executedRelayerAccountResponse(test.blocks, "ledger.getFrontierAccountBlock", relayerAccountParams(address)); err == nil {
				t.Fatal("incomplete account chain accepted")
			}
		})
	}
	for _, block := range blocks {
		got, err := executedRelayerAccountResponse(blocks, "ledger.getAccountBlockByHash", relayerAccountParams(block.Hash))
		if err != nil || got != block {
			t.Fatal("exact account hash selection changed")
		}
	}
	got, err := executedRelayerAccountResponse(blocks, "ledger.getAccountBlockByHash", relayerAccountParams(znn.Hash{99}))
	if err != nil || got != nil {
		t.Fatal("missing account hash substituted the frontier")
	}
	got, err = executedRelayerAccountResponse(nil, "ledger.getFrontierAccountBlock", relayerAccountParams(address))
	if err != nil || got != nil {
		t.Fatal("empty account chain was fabricated")
	}
}

func TestExecutedRelayerUnconfirmedPagesPreserveTheirOwnCount(t *testing.T) {
	blocks := relayerHistoryVector(5)
	blocks[0].ConfirmationDetail = &znn.ConfirmationDetail{MomentumHeight: 1}
	blocks[3].ConfirmationDetail = &znn.ConfirmationDetail{MomentumHeight: 4}
	got, err := executedRelayerAccountResponse(blocks, "ledger.getUnconfirmedBlocksByAddress", relayerAccountParams(devnet.RelayerKey(0).Address, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	page := got.(*znn.BlockList)
	if page.Count != 3 || len(page.List) != 1 || page.List[0] != blocks[4] {
		t.Fatal("unconfirmed pagination changed confirmation selection or count")
	}
}

func TestExecutedCanonicalNonceSelectorRefusesAnOrphanWithoutLatestFallback(t *testing.T) {
	run := newExecutionRun(t, newExecutionScenario(t))
	run.through(9, false)
	address := common.Address{11}
	run.backend.mu.Lock()
	old := run.backend.chain.CurrentBlock()
	state, err := run.backend.chain.StateAt(old.Root)
	if err != nil {
		run.backend.mu.Unlock()
		t.Fatal(err)
	}
	want := state.GetNonce(address)
	_, err = run.backend.chain.SetCanonical(run.backend.chain.GetBlockByNumber(6))
	run.backend.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var nonce hexutil.Uint64
	if err := run.eth.Client().CallContext(ctx, &nonce, "eth_getTransactionCount", address, map[string]any{"blockHash": old.Hash(), "requireCanonical": false}); err != nil || uint64(nonce) != want {
		t.Fatal("explicit noncanonical historical state could not be read", err)
	}
	if err := run.eth.Client().CallContext(ctx, &nonce, "eth_getTransactionCount", address, map[string]any{"blockHash": old.Hash(), "requireCanonical": true}); err == nil {
		t.Fatal("orphaned canonical nonce selector was accepted or replaced with latest")
	}
	if _, err := run.eth.NonceAt(ctx, address, nil); err != nil {
		t.Fatal("refused orphan changed independent latest-state reads", err)
	}
}
