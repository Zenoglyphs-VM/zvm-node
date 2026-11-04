package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

type executedContractSignatureSetup struct {
	owner, wallet                       common.Address
	init, runtime                       []byte
	abi                                 abi.ABI
	sourceSHA, compilerSHA, artifactSHA string
	mode                                uint8
	revokeBeforeFill                    bool
}

func loadContractSignatureWallet(t *testing.T, owner common.Address) *executedContractSignatureSetup {
	t.Helper()
	path := os.Getenv("ZVM_BROWSER_CONTRACT_SIGNATURE_WALLET")
	if path == "" {
		t.Fatal("explicit compiled contract-signature wallet is required")
	}
	raw := creditRegularFile(t, path, 64<<10)
	var artifact struct {
		Schema          int             `json:"schema"`
		SourceName      string          `json:"sourceName"`
		SourceSHA       string          `json:"sourceSha256"`
		CompilerVersion string          `json:"compilerVersion"`
		CompilerSHA     string          `json:"compilerSha256"`
		SupportSHA      string          `json:"supportSha256"`
		ABI             json.RawMessage `json:"abi"`
		Bytecode        string          `json:"bytecode"`
		Runtime         string          `json:"deployedBytecode"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		t.Fatal(err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		t.Fatal("contract-signature artifact has trailing input")
	}
	source := creditRegularFile(t, "testdata/ContractSignatureFixtureWallet.sol", 16<<10)
	support := creditRegularFile(t, "../scripts/market-credit-build-support.cjs", 64<<10)
	if artifact.Schema != 1 || artifact.SourceName != "ContractSignatureFixtureWallet.sol" || artifact.SourceSHA != creditSourceSHA(source) ||
		artifact.SupportSHA != creditSourceSHA(support) || artifact.CompilerVersion != "0.8.30+commit.73712a01.Emscripten.clang" ||
		artifact.CompilerSHA != "81475c98b6d2094a821fd9d7b6278556d8095ccc23e0b8a1029b1c08a89cd4b2" {
		t.Fatal("contract-signature artifact differs from its exact source/compiler")
	}
	contractABI, err := abi.JSON(bytes.NewReader(artifact.ABI))
	if err != nil {
		t.Fatal(err)
	}
	init, err := hexutil.Decode(artifact.Bytecode)
	if err != nil || len(init) == 0 || len(init) > 8<<10 {
		t.Fatal("invalid bounded contract-signature init code")
	}
	runtime, err := hexutil.Decode(artifact.Runtime)
	if err != nil || len(runtime) == 0 || len(runtime) > 8<<10 {
		t.Fatal("invalid bounded contract-signature runtime")
	}
	constructor, err := contractABI.Pack("", owner)
	if err != nil {
		t.Fatal(err)
	}
	return &executedContractSignatureSetup{owner: owner, init: append(init, constructor...), runtime: runtime, abi: contractABI,
		sourceSHA: artifact.SourceSHA, compilerSHA: artifact.CompilerSHA, artifactSHA: creditSourceSHA(raw)}
}

func (s *executedContractSignatureSetup) call(t *testing.T, method string, args ...any) []byte {
	t.Helper()
	raw, err := s.abi.Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func contractInputDigest(domain common.Hash, input *Input) common.Hash {
	if input.Kind == KindBulk {
		return RootDigest(domain, ProcessProof(input.Order.Hash(), input.Proof))
	}
	return OrderDigest(domain, input.Order.Hash())
}

func NewExecutedContractSignatureBrowserFixture(t *testing.T, kind string) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	if kind != "single" && kind != "bulk" {
		t.Fatal("undeclared contract-signature listing kind")
	}
	owner := crypto.PubkeyToAddress(executedMarketKey(t).PublicKey)
	f, p := newExecutedMarketActionContractSignatureBrowserFixture(t, kind, "batch", "", nil, loadContractSignatureWallet(t, owner))
	p.batchBalances = &executedBatchBalances{}
	return f, p
}

func (f *ExecutedMarketFixture) SigningOwner() common.Address {
	if setup := f.r.scenario.contractSignature; setup != nil {
		return setup.owner
	}
	return f.Seller()
}

func (f *ExecutedMarketFixture) ContractSignatureDescription() map[string]any {
	setup := f.r.scenario.contractSignature
	if setup == nil {
		return nil
	}
	return map[string]any{"profile": "owner-signed-erc1271", "owner": setup.owner.Hex(), "trader": setup.wallet.Hex(),
		"sourceSHA256": setup.sourceSHA, "compilerSHA256": setup.compilerSHA, "artifactSHA256": setup.artifactSHA,
		"runtimeCodeHash": crypto.Keccak256Hash(setup.runtime).Hex(), "signatureMode": setup.mode,
		"genericOrPhysicalWalletQualified": false, "authenticatedNoMQualified": false, "physicalWalletOrPublicNetwork": false}
}

func (f *ExecutedMarketFixture) AssertContractSignatureInput(t *testing.T, input *Input) map[string]any {
	t.Helper()
	setup := f.r.scenario.contractSignature
	if setup == nil {
		return nil
	}
	ctx := context.Background()
	hash := f.HeadHash()
	code, err := f.r.eth.CodeAtHash(ctx, setup.wallet, hash)
	if err != nil || !bytes.Equal(code, setup.runtime) || setup.wallet == setup.owner || f.Seller() != setup.wallet {
		t.Fatal("actual contract-trader runtime/authority differs")
	}
	ownerData := setup.call(t, "owner")
	raw, err := f.r.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &setup.wallet, Data: ownerData}, hash)
	if err != nil || len(raw) != 32 || common.BytesToAddress(raw) != setup.owner {
		t.Fatal("actual signing owner differs")
	}
	digest := contractInputDigest(Domain(f.Rollup().L2ChainID, f.Exchange()), input)
	data := setup.call(t, "isValidSignature", digest, []byte(input.Signature))
	raw, err = f.r.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &setup.wallet, Data: data}, hash)
	if err != nil || len(raw) != 32 || !bytes.Equal(raw[:4], []byte{0x16, 0x26, 0xba, 0x7e}) {
		t.Fatal("actual ERC-1271 signature was not valid at the canonical hash")
	}
	recovered, ok := Recover(Domain(f.Rollup().L2ChainID, f.Exchange()), input)
	if !ok || recovered != setup.owner || recovered == setup.wallet || input.Order.Trader != setup.wallet {
		t.Fatal("contract trader was confused with its signing EOA")
	}
	after, err := f.r.eth.HeaderByNumber(ctx, nil)
	if err != nil || after.Hash() != hash {
		t.Fatal("contract-signature observation changed its canonical hash")
	}
	return map[string]any{"hash": hash.Hex(), "orderHash": input.Order.Hash().Hex(), "digest": digest.Hex(), "kind": input.Kind,
		"owner": setup.owner.Hex(), "trader": setup.wallet.Hex(), "recoveredEOA": recovered.Hex(), "magic": "0x1626ba7e",
		"sourceBoundRuntime": true, "actualHashBoundERC1271Call": true, "directStateOrStorageInsertion": 0}
}

func (f *ExecutedMarketFixture) ContractSignatureEvidence(t *testing.T) map[string]any {
	if f.r.scenario.contractSignature == nil {
		return nil
	}
	inputs := []map[string]any{}
	for _, input := range f.r.scenario.inputs {
		inputs = append(inputs, f.AssertContractSignatureInput(t, input))
	}
	return map[string]any{"pass": true, "configuration": f.ContractSignatureDescription(), "inputs": inputs,
		"actualDeployedContractAndOwnerSignatures": true, "genericOrPhysicalWalletQualified": false, "authenticatedNoMQualified": false}
}
