package main

import (
	"bytes"
	"context"
	"math/big"

	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const selectedPrefix = "source-constructors-v1-"

func constructorDomain(chain uint64, address common.Address) []byte {
	raw := append(crypto.Keccak256([]byte("EIP712Domain(string name,uint256 chainId,address verifyingContract)")), crypto.Keccak256([]byte("Permit2"))...)
	w := word(chain)
	raw = append(raw, w[:]...)
	raw = append(raw, common.LeftPadBytes(address.Bytes(), 32)...)
	return crypto.Keccak256(raw)
}

// Every request uses an explicit canonical block hash. Checking the accepted
// head again binds the read-only observations to the head before the checkpoint.
func constructorCheckpoint(ctx context.Context, c *rpc.Client, cfg *rollup.Config, model *state.StateDB, profile, phase string, head common.Hash, number uint64) {
	check(cfg.PredeployState == rollup.PredeployStateConstructorsV1, "constructor checkpoint recipe differs")
	root := model.IntermediateRoot(true)
	check(model.Error() == nil, "constructor checkpoint model failed")
	canonical := func() {
		var header *types.Header
		must(c.CallContext(ctx, &header, "eth_getBlockByNumber", "latest", false))
		check(header != nil && header.Hash() == head && header.Root == root && header.Number.Uint64() == number, "constructor checkpoint canonical head differs")
	}
	canonical()
	anchor := rpc.BlockNumberOrHashWithHash(head, true)
	addresses := l2.Predeploys()
	names := []string{"Permit2", "EntryPoint_v07", "SenderCreator_v07", "Create2Deployer_Arachnid"}
	expectedNonce := []uint64{1, 2, 1, 3}
	for index, name := range names {
		address := addresses[name]
		check(model.GetNonce(address) == expectedNonce[index] && model.GetBalance(address).IsZero(), "constructor checkpoint source nonce or balance differs")
		var proof struct {
			Address     common.Address `json:"address"`
			Balance     *hexutil.Big   `json:"balance"`
			Nonce       hexutil.Uint64 `json:"nonce"`
			CodeHash    common.Hash    `json:"codeHash"`
			StorageHash common.Hash    `json:"storageHash"`
		}
		must(c.CallContext(ctx, &proof, "eth_getProof", address, []common.Hash{}, anchor))
		check(proof.Address == address && proof.Balance != nil && proof.Balance.ToInt().Sign() == 0 && uint64(proof.Nonce) == expectedNonce[index], "anchored constructor account nonce or balance differs")
		check(proof.CodeHash == crypto.Keccak256Hash(model.GetCode(address)) && proof.StorageHash == model.GetStorageRoot(address), "anchored constructor code or storage differs")
	}
	var status common.Hash
	must(c.CallContext(ctx, &status, "eth_getStorageAt", addresses["EntryPoint_v07"], word(2), anchor))
	check(status == word(1) && model.GetState(addresses["EntryPoint_v07"], word(2)) == word(1), "anchored EntryPoint status differs")
	read := func(address common.Address, signature string, args, expected []byte) {
		input := append(crypto.Keccak256([]byte(signature))[:4], args...)
		var output hexutil.Bytes
		must(c.CallContext(ctx, &output, "eth_call", map[string]any{"to": address, "input": hexutil.Bytes(input)}, anchor))
		check(bytes.Equal(output, expected), "anchored constructor getter differs")
	}
	reader := common.HexToAddress("0x1234")
	read(addresses["Permit2"], "DOMAIN_SEPARATOR()", nil, constructorDomain(cfg.L2ChainID, addresses["Permit2"]))
	read(addresses["EntryPoint_v07"], "balanceOf(address)", common.LeftPadBytes(reader.Bytes(), 32), make([]byte, 32))
	key := word(1)
	read(addresses["EntryPoint_v07"], "getNonce(address,uint192)", append(common.LeftPadBytes(reader.Bytes(), 32), key[:]...), new(big.Int).Lsh(big.NewInt(1), 64).FillBytes(make([]byte, 32)))
	canonical()
	unknownRefusals := 0
	if phase == "genesis" {
		missingHash := crypto.Keccak256Hash([]byte("offline-missing-constructor-canonical-anchor"))
		check(missingHash != head, "missing canonical anchor collides with accepted head")
		missingAnchor := rpc.BlockNumberOrHashWithHash(missingHash, true)
		var missingCall hexutil.Bytes
		err := c.CallContext(ctx, &missingCall, "eth_call", map[string]any{"to": addresses["Permit2"], "input": hexutil.Bytes(crypto.Keccak256([]byte("DOMAIN_SEPARATOR()"))[:4])}, missingAnchor)
		check(err != nil, "unknown canonical hash admitted by eth_call")
		var missingProof map[string]any
		err = c.CallContext(ctx, &missingProof, "eth_getProof", addresses["EntryPoint_v07"], []common.Hash{}, missingAnchor)
		check(err != nil, "unknown canonical hash admitted by eth_getProof")
		unknownRefusals = 2
		canonical()
	}
	output("CHECKPOINT", map[string]any{"profile": profile, "phase": phase, "number": number, "blockHash": head, "stateRoot": root,
		"predeployState": cfg.PredeployState, "anchoredConstructorAccountChecks": 4, "statusSlotReads": 1, "readOnlyGetterChecks": 3,
		"explicitCanonicalBlockHashAnchors": true, "constructorNonceBalanceCodeAndStorageMatched": true, "getterReturnValuesSourcePredicted": true,
		"acceptedHeadRecheckedUnchanged": true, "unknownCanonicalHashRPCRefusals": unknownRefusals, "realOwnerOrReleaseAccepted": false})
}
