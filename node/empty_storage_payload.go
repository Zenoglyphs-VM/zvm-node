package node

import (
	"errors"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// payloadWithBoundEmptyWithdrawalsRoot handles the pinned encoder's omission of
// the empty MessagePasser root. The Isthmus Engine API still requires that field.
// Try only the empty root, on a copy, and require the full advertised block hash
// with the exact same beacon root and Engine API arguments before returning it.
// This does not authenticate the engine, NoM source or execution result.
func payloadWithBoundEmptyWithdrawalsRoot(payload *engine.ExecutableData, beaconRoot common.Hash, chain *params.ChainConfig) (*engine.ExecutableData, error) {
	if payload == nil {
		return nil, errors.New("engine returned no execution payload")
	}
	if payload.WithdrawalsRoot != nil {
		return payload, nil
	}
	if chain == nil {
		return nil, errors.New("missing execution-payload fork context")
	}
	if !chain.HasOptimismWithdrawalsRoot(payload.Timestamp) {
		return payload, nil
	}
	if payload.BlobGasUsed == nil || payload.ExcessBlobGas == nil {
		return nil, errors.New("engine omitted required blob gas header fields")
	}
	copy := *payload
	root := types.EmptyWithdrawalsHash
	copy.WithdrawalsRoot = &root
	if _, err := engine.ExecutableDataToBlock(copy, []common.Hash{}, &beaconRoot, [][]byte{}, chain); err != nil {
		return nil, errors.New("engine omitted withdrawalsRoot without a matching empty-root block hash")
	}
	return &copy, nil
}
