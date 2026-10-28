package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type executedRoyaltySetup struct {
	init, runtime                       []byte
	sourceSHA, compilerSHA, artifactSHA string
}

func loadExternalRoyaltyCollection(t *testing.T) *executedRoyaltySetup {
	t.Helper()
	path := os.Getenv("ZVM_BROWSER_ROYALTY_COLLECTION")
	if path == "" {
		t.Fatal("explicit compiled external royalty collection is required")
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
		t.Fatal("external royalty artifact has trailing input")
	}
	source := creditRegularFile(t, "testdata/RoyaltyFixtureCollection.sol", 16<<10)
	support := creditRegularFile(t, "../scripts/market-credit-build-support.cjs", 64<<10)
	if artifact.Schema != 1 || artifact.SourceName != "RoyaltyFixtureCollection.sol" ||
		artifact.SourceSHA != creditSourceSHA(source) || artifact.SupportSHA != creditSourceSHA(support) ||
		artifact.CompilerVersion != "0.8.30+commit.73712a01.Emscripten.clang" ||
		artifact.CompilerSHA != "81475c98b6d2094a821fd9d7b6278556d8095ccc23e0b8a1029b1c08a89cd4b2" {
		t.Fatal("external royalty artifact differs from its source/compiler inputs")
	}
	init, err := hexutil.Decode(artifact.Bytecode)
	if err != nil || len(init) == 0 || len(init) > 8<<10 {
		t.Fatal("invalid external royalty init code")
	}
	runtime, err := hexutil.Decode(artifact.Runtime)
	if err != nil || len(runtime) == 0 || len(runtime) > 8<<10 {
		t.Fatal("invalid external royalty runtime")
	}
	return &executedRoyaltySetup{init: init, runtime: runtime, sourceSHA: artifact.SourceSHA,
		compilerSHA: artifact.CompilerSHA, artifactSHA: creditSourceSHA(raw)}
}

// This is actual EVM settlement and replay under declared NoM observations.
// It does not qualify production pages, provider transactions or authenticated NoM.
func TestExecutedExternalRoyaltySettlementAndRecovery(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit compiled external-royalty CI fixture")
	}
	setup := loadExternalRoyaltyCollection(t)
	scenario := newExecutionScenarioInputs(t, false, false, executionScenarioInputs{
		seller: znn.Address{19: 161}, timestamp: 1_800_000_000, royalty: setup,
	})
	run := newExecutionRun(t, scenario)
	// Vault deposits declared at height 7 are executed by the following
	// derivation. Pin a fully funded branch that contains no sale first.
	run.nomMu.Lock()
	run.branch = 1
	run.nomMu.Unlock()
	run.through(9, false)
	ctx := context.Background()
	before := map[common.Address]*big.Int{}
	payout := scenario.payouts
	if payout == nil || payout.feeBps != 200 || payout.royaltyBps != 5000 {
		t.Fatal("external royalty setup differs")
	}
	actors := []common.Address{common.Address(scenario.buyer), common.Address(scenario.seller), payout.feeRecipient, payout.royaltyRecipient}
	for _, actor := range actors {
		balance, err := run.eth.BalanceAt(ctx, actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		before[actor] = new(big.Int).Set(balance)
	}
	implementation := crypto.CreateAddress(common.Address(scenario.seller), 0)
	code, err := run.eth.CodeAt(ctx, implementation, nil)
	if err != nil || !bytes.Equal(code, setup.runtime) {
		t.Fatal("the declared external royalty runtime was not deployed")
	}
	price, ok := new(big.Int).SetString("200000000000000018", 10)
	if !ok {
		t.Fatal("invalid exact fixture price")
	}
	if before[actors[0]].Cmp(new(big.Int).Mul(new(big.Int).Set(price), big.NewInt(2))) < 0 {
		t.Fatal("native balance baseline is not fully funded")
	}
	cap := payoutShare(price, 1000)
	fee := payoutShare(price, 200)
	combined := payoutShare(price, 1200)
	if new(big.Int).Sub(combined, new(big.Int).Add(new(big.Int).Set(cap), fee)).Cmp(big.NewInt(1)) != 0 {
		t.Fatal("native price does not expose independent rounding")
	}
	var transaction common.Hash
	observations := []map[string]any{}
	observe := func(phase string, included bool) map[string]any {
		head, err := run.eth.HeaderByNumber(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		hash := head.Hash()
		read := func(artifact, method string, to common.Address, args ...any) ([]byte, error) {
			data, err := scenario.arts.MustGet(artifact).Call(method, args...)
			if err != nil {
				t.Fatal(err)
			}
			return run.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &to, Data: data}, hash)
		}
		scalar := func(method string, args ...any) *big.Int {
			raw, err := read("ZVMExchange", method, scenario.exchange, args...)
			if err != nil || len(raw) != 32 {
				t.Fatal("malformed executed exchange scalar")
			}
			return new(big.Int).SetBytes(raw)
		}
		if scalar("feeBps").Cmp(big.NewInt(200)) != 0 || scalar("MAX_ROYALTY_BPS").Cmp(big.NewInt(1000)) != 0 {
			t.Fatal("exchange fee/cap differs")
		}
		raw, err := read("ZVMCollection", "royaltyInfo", scenario.token, big.NewInt(1), price)
		if err != nil || len(raw) != 64 || common.BytesToAddress(raw[:32]) != payout.royaltyRecipient ||
			new(big.Int).SetBytes(raw[32:]).Cmp(payoutShare(price, 5000)) != 0 {
			t.Fatal("the external contract did not report an above-cap royalty")
		}
		_, royaltyError := read("ZVMCollection", "royaltyInfo", scenario.token, big.NewInt(2), price)
		if royaltyError == nil || !strings.Contains(royaltyError.Error(), "execution reverted") {
			t.Fatal("the second external royalty call did not execute its revert")
		}
		for index, input := range scenario.inputs {
			owner, err := read("ZVMCollection", "ownerOf", scenario.token, big.NewInt(int64(index+1)))
			wantOwner, filled := common.Address(scenario.seller), int64(0)
			if included {
				wantOwner = common.Address(scenario.buyer)
				filled = 1
			}
			if err != nil || len(owner) != 32 || common.BytesToAddress(owner) != wantOwner || scalar("filled", input.Order.Hash()).Cmp(big.NewInt(filled)) != 0 {
				t.Fatal("executed external royalty fill/ownership differs")
			}
		}
		gross, paidRoyalty, paidFee := new(big.Int), new(big.Int), new(big.Int)
		if included {
			gross.Mul(price, big.NewInt(2))
			paidRoyalty.Set(cap)
			paidFee.Mul(fee, big.NewInt(2))
		}
		proceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(gross), paidRoyalty), paidFee)
		wanted := []*big.Int{
			new(big.Int).Sub(new(big.Int).Set(before[actors[0]]), gross),
			new(big.Int).Add(new(big.Int).Set(before[actors[1]]), proceeds),
			new(big.Int).Add(new(big.Int).Set(before[actors[2]]), paidFee),
			new(big.Int).Add(new(big.Int).Set(before[actors[3]]), paidRoyalty),
		}
		balances := []string{}
		for index, actor := range actors {
			balance, err := run.eth.BalanceAtHash(ctx, actor, hash)
			credit := scalar("credits", actor)
			if err != nil || balance.Cmp(wanted[index]) != 0 || credit.Sign() != 0 {
				t.Fatalf("executed capped/fallback payout differs: phase=%s role=%d balance=%v want=%s credit=%s error=%v", phase, index, balance, wanted[index], credit, err)
			}
			balances = append(balances, balance.String())
		}
		if balance, err := run.eth.BalanceAtHash(ctx, scenario.exchange, hash); err != nil || balance.Sign() != 0 {
			t.Fatal("settlement retained an undeclared native balance")
		}
		logs, err := run.eth.FilterLogs(ctx, ethereum.FilterQuery{Addresses: []common.Address{scenario.token}, Topics: [][]common.Hash{
			{crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))},
			{common.BytesToHash(actors[1].Bytes())}, {common.BytesToHash(actors[0].Bytes())},
			{common.BigToHash(big.NewInt(1)), common.BigToHash(big.NewInt(2))},
		}})
		if err != nil || len(logs) != (map[bool]int{true: 2, false: 0}[included]) {
			t.Fatal("actual external royalty sale transfer logs differ")
		}
		if included {
			if logs[0].TxHash != logs[1].TxHash || logs[0].TxHash == (common.Hash{}) {
				t.Fatal("both royalties were not settled in one actual buyMany")
			}
			if transaction != (common.Hash{}) && transaction != logs[0].TxHash {
				t.Fatal("restoration changed the original native transaction")
			}
			transaction = logs[0].TxHash
			receipt, err := run.eth.TransactionReceipt(ctx, transaction)
			if err != nil || receipt.Status != types.ReceiptStatusSuccessful || receipt.GasUsed <= 21000 || receipt.Type != types.DepositTxType {
				t.Fatal("native royalty batch lacks a successful actual receipt")
			}
		} else if transaction != (common.Hash{}) {
			receipt, err := run.eth.TransactionReceipt(ctx, transaction)
			if receipt != nil || err == nil {
				t.Fatal("replacement retained the orphaned royalty transaction")
			}
		}
		after, err := run.eth.HeaderByNumber(ctx, head.Number)
		if err != nil || after.Hash() != hash {
			t.Fatal("the observation did not retain its canonical hash")
		}
		result := map[string]any{"phase": phase, "included": included, "canonicalHash": hash.Hex(), "balances": balances,
			"gross": gross.String(), "royalty": paidRoyalty.String(), "fee": paidFee.String(), "sellerProceeds": proceeds.String(),
			"aboveCapReportedRoyalty": payoutShare(price, 5000).String(), "cap": cap.String(), "feePerItem": fee.String(),
			"royaltyCallReverted": true, "successfulTransferLogs": len(logs), "hashBoundReads": true, "allCreditsZero": true}
		observations = append(observations, result)
		return result
	}
	observe("prefix", false)
	run.nomMu.Lock()
	run.branch = 0
	run.nomMu.Unlock()
	run.through(9, false)
	mined := observe("mined", true)
	run.nomMu.Lock()
	run.branch = 1
	run.nomMu.Unlock()
	run.through(9, false)
	observe("replacement", false)
	run.nomMu.Lock()
	run.branch = 0
	run.nomMu.Unlock()
	run.through(9, false)
	restored := observe("restored", true)
	for _, key := range []string{"canonicalHash", "balances", "gross", "royalty", "fee", "sellerProceeds"} {
		if !reflect.DeepEqual(mined[key], restored[key]) {
			t.Fatal("restored royalty state differs")
		}
	}
	run.close()
	run.open()
	run.catchUp()
	observe("reopened", true)
	record := map[string]any{"pass": true, "sourceSHA256": setup.sourceSHA, "compilerSHA256": setup.compilerSHA, "artifactSHA256": setup.artifactSHA,
		"price": price.String(), "fundedBaselineSourceHeight": 9, "fundedBaselineBranch": 1, "reportedBps": 5000, "royaltyCapBps": 1000, "feeBps": 200, "separateRoundingDifference": "1",
		"transaction": transaction.Hex(), "observations": observations, "actualEVMSettlement": true, "nativeCallsUnderDeclaredNoMObservations": true,
		"directStateOrStorageInsertion": 0, "physicalWalletOrPublicNetwork": false, "productionBrowserOrProviderExecutionQualified": false, "authenticatedNoMQualified": false}
	rawRecord, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("EXTERNAL-ROYALTY-NATIVE-QUALIFICATION " + string(rawRecord))
}
