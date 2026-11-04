package node

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func emptyStoragePayloadFixture(t *testing.T, storageRoot, requestsHash common.Hash) (*engine.ExecutableData, common.Hash, *params.ChainConfig) {
	t.Helper()
	zero := uint64(0)
	beacon := common.HexToHash("0x04")
	chain := &params.ChainConfig{ChainID: big.NewInt(321), Optimism: &params.OptimismConfig{}, IsthmusTime: &zero}
	header := &types.Header{ParentHash: common.HexToHash("0x01"), UncleHash: types.EmptyUncleHash,
		Coinbase: common.HexToAddress("0x1001"), Root: common.HexToHash("0x02"), TxHash: types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash, Difficulty: new(big.Int), Number: big.NewInt(1), GasLimit: 30_000_000,
		Time: 10, BaseFee: big.NewInt(1), MixDigest: common.HexToHash("0x03"), WithdrawalsHash: &storageRoot,
		ExcessBlobGas: &zero, BlobGasUsed: &zero, ParentBeaconRoot: &beacon, RequestsHash: &requestsHash}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Withdrawals: []*types.Withdrawal{}})
	payload := engine.BlockToExecutableData(block, new(big.Int), nil, [][]byte{}).ExecutionPayload
	if payload == nil || payload.BlockHash != block.Hash() {
		t.Fatal("original encoder fixture did not retain the header commitment")
	}
	return payload, beacon, chain
}

func TestBoundEmptyStoragePayload(t *testing.T) {
	fixture := func(t *testing.T) (*engine.ExecutableData, common.Hash, *params.ChainConfig) {
		return emptyStoragePayloadFixture(t, types.EmptyWithdrawalsHash, types.CalcRequestsHash([][]byte{}))
	}
	t.Run("missing-root-bound-to-complete-header", func(t *testing.T) {
		payload, beacon, chain := fixture(t)
		if payload.WithdrawalsRoot != nil {
			t.Fatal("the pinned original empty-root encoder behavior changed")
		}
		before, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain)
		if err != nil || bound == nil || bound == payload || bound.WithdrawalsRoot == nil || *bound.WithdrawalsRoot != types.EmptyWithdrawalsHash {
			t.Fatalf("committed empty root was not reconstructed on a copy: %v", err)
		}
		if _, err := engine.ExecutableDataToBlock(*bound, []common.Hash{}, &beacon, [][]byte{}, chain); err != nil {
			t.Fatal("returned payload no longer matches its full header", err)
		}
		after, err := json.Marshal(payload)
		if err != nil || !bytes.Equal(before, after) || payload.WithdrawalsRoot != nil {
			t.Fatal("the original engine payload was modified")
		}
	})
	t.Run("present-root-forwarded-unchanged", func(t *testing.T) {
		for _, root := range []common.Hash{types.EmptyWithdrawalsHash, common.HexToHash("0x1234")} {
			payload, beacon, chain := emptyStoragePayloadFixture(t, root, types.CalcRequestsHash([][]byte{}))
			payload.WithdrawalsRoot = &root
			bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain)
			if err != nil || bound != payload || bound.WithdrawalsRoot != payload.WithdrawalsRoot {
				t.Fatal("an already present root or payload was replaced")
			}
		}
	})
	t.Run("pre-isthmus-payload-forwarded-unchanged", func(t *testing.T) {
		payload, beacon, chain := fixture(t)
		chain.IsthmusTime = nil
		bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain)
		if err != nil || bound != payload || payload.WithdrawalsRoot != nil {
			t.Fatal("pre-Isthmus data was normalized")
		}
	})
	t.Run("nil-payload-refused", func(t *testing.T) {
		_, beacon, chain := fixture(t)
		if bound, err := payloadWithBoundEmptyWithdrawalsRoot(nil, beacon, chain); err == nil || bound != nil {
			t.Fatal("nil payload accepted")
		}
	})
	t.Run("missing-fork-context-refused", func(t *testing.T) {
		payload, beacon, _ := fixture(t)
		if bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, nil); err == nil || bound != nil {
			t.Fatal("missing fork context accepted")
		}
	})
	changes := []struct {
		name   string
		mutate func(*engine.ExecutableData)
	}{
		{"nil-withdrawals", func(p *engine.ExecutableData) { p.Withdrawals = nil }},
		{"nonempty-withdrawals", func(p *engine.ExecutableData) { p.Withdrawals = []*types.Withdrawal{{Index: 1, Amount: 1}} }},
		{"different-advertised-hash", func(p *engine.ExecutableData) { p.BlockHash = common.HexToHash("0x9999") }},
		{"different-parent-hash", func(p *engine.ExecutableData) { p.ParentHash = common.HexToHash("0x9999") }},
		{"different-state-root", func(p *engine.ExecutableData) { p.StateRoot = common.HexToHash("0x9999") }},
		{"different-receipts-root", func(p *engine.ExecutableData) { p.ReceiptsRoot = common.HexToHash("0x9999") }},
		{"malformed-transaction", func(p *engine.ExecutableData) { p.Transactions = [][]byte{{0x01}} }},
		{"short-log-bloom", func(p *engine.ExecutableData) { p.LogsBloom = make([]byte, 255) }},
		{"oversized-extra-data", func(p *engine.ExecutableData) { p.ExtraData = make([]byte, 33) }},
		{"negative-base-fee", func(p *engine.ExecutableData) { p.BaseFeePerGas = big.NewInt(-1) }},
		{"oversized-base-fee", func(p *engine.ExecutableData) { p.BaseFeePerGas = new(big.Int).Lsh(big.NewInt(1), 256) }},
		{"missing-blob-gas-header-field", func(p *engine.ExecutableData) { p.BlobGasUsed = nil }},
		{"missing-excess-blob-gas-header-field", func(p *engine.ExecutableData) { p.ExcessBlobGas = nil }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			payload, beacon, chain := fixture(t)
			change.mutate(payload)
			if bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain); err == nil || bound != nil {
				t.Fatal("changed or malformed committed material accepted")
			}
		})
	}
	t.Run("different-beacon-root-refused", func(t *testing.T) {
		payload, _, chain := fixture(t)
		if bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, common.HexToHash("0x7777"), chain); err == nil || bound != nil {
			t.Fatal("different beacon root accepted")
		}
	})
	t.Run("nonempty-hidden-storage-root-refused", func(t *testing.T) {
		payload, beacon, chain := emptyStoragePayloadFixture(t, common.HexToHash("0x1234"), types.CalcRequestsHash([][]byte{}))
		payload.WithdrawalsRoot = nil
		if bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain); err == nil || bound != nil {
			t.Fatal("a nonempty hidden root was replaced with the empty root")
		}
	})
	t.Run("different-requests-commitment-refused", func(t *testing.T) {
		payload, beacon, chain := emptyStoragePayloadFixture(t, types.EmptyWithdrawalsHash, types.CalcRequestsHash([][]byte{{0, 1}}))
		if bound, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain); err == nil || bound != nil {
			t.Fatal("different execution requests commitment accepted")
		}
	})
	t.Run("returned-root-pointers-independent", func(t *testing.T) {
		payload, beacon, chain := fixture(t)
		first, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain)
		if err != nil {
			t.Fatal(err)
		}
		second, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain)
		if err != nil {
			t.Fatal(err)
		}
		if first.WithdrawalsRoot == second.WithdrawalsRoot {
			t.Fatal("returned roots share storage")
		}
		*first.WithdrawalsRoot = common.HexToHash("0x1111")
		if *second.WithdrawalsRoot != types.EmptyWithdrawalsHash || payload.WithdrawalsRoot != nil {
			t.Fatal("a returned copy changed another payload or the original")
		}
	})
	t.Run("concurrent-shared-input-remains-unchanged", func(t *testing.T) {
		payload, beacon, chain := fixture(t)
		results := make(chan error, 32)
		for i := 0; i < 32; i++ {
			go func() { _, err := payloadWithBoundEmptyWithdrawalsRoot(payload, beacon, chain); results <- err }()
		}
		for i := 0; i < 32; i++ {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if payload.WithdrawalsRoot != nil {
			t.Fatal("concurrent conversion changed the shared engine payload")
		}
	})
}
