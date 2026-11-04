package orderbook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const creditSaleHeight = uint64(14)
const creditAcceptHeight = uint64(15)
const creditFundingHeight = uint64(16)

type executedCreditSetup struct {
	wallet                              common.Address
	init, runtime                       []byte
	abi                                 abi.ABI
	rawABI                              json.RawMessage
	sourceSHA, compilerSHA, artifactSHA string
	minedRevert                         bool
}

type executedCreditTransfer struct {
	setup             *executedCreditSetup
	actorBalance      *big.Int
	rejectedEstimates int
	rejectedData      []string
}

func creditSourceSHA(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func creditRegularFile(t *testing.T, path string, maximum int64) []byte {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maximum {
		t.Fatal("credit fixture input is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(before, after) || int64(len(raw)) != before.Size() ||
		after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("credit fixture input changed or exceeded its byte bound")
	}
	return raw
}

func loadCreditWallet(t *testing.T, owner common.Address) *executedCreditSetup {
	t.Helper()
	path := os.Getenv("ZVM_BROWSER_CREDIT_WALLET")
	if path == "" {
		t.Fatal("explicit compiled credit-wallet input is required")
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
		t.Fatal("credit wallet artifact has trailing input")
	}
	source := creditRegularFile(t, "testdata/CreditFixtureWallet.sol", 16<<10)
	support := creditRegularFile(t, "../scripts/market-credit-build-support.cjs", 64<<10)
	if artifact.Schema != 1 || artifact.SourceName != "CreditFixtureWallet.sol" || artifact.SourceSHA != creditSourceSHA(source) ||
		artifact.SupportSHA != creditSourceSHA(support) || artifact.CompilerVersion != "0.8.30+commit.73712a01.Emscripten.clang" ||
		artifact.CompilerSHA != "81475c98b6d2094a821fd9d7b6278556d8095ccc23e0b8a1029b1c08a89cd4b2" {
		t.Fatal("compiled credit wallet differs from its exact source/compiler inputs")
	}
	contractABI, err := abi.JSON(bytes.NewReader(artifact.ABI))
	if err != nil {
		t.Fatal(err)
	}
	init, err := hexutil.Decode(artifact.Bytecode)
	if err != nil || len(init) == 0 || len(init) > 8<<10 {
		t.Fatal("invalid bounded credit wallet init code")
	}
	runtime, err := hexutil.Decode(artifact.Runtime)
	if err != nil || len(runtime) == 0 || len(runtime) > 8<<10 {
		t.Fatal("invalid bounded credit wallet runtime")
	}
	constructor, err := contractABI.Pack("", owner)
	if err != nil {
		t.Fatal(err)
	}
	return &executedCreditSetup{wallet: crypto.CreateAddress(owner, 0), init: append(init, constructor...),
		runtime: runtime, abi: contractABI, rawABI: append(json.RawMessage(nil), artifact.ABI...),
		sourceSHA: artifact.SourceSHA, compilerSHA: artifact.CompilerSHA, artifactSHA: creditSourceSHA(raw)}
}

// The source sale is declared setup, not a substitute for a browser withdrawal.
// The actual page requests a call from this contract account; its test provider
// signs an owner EOA outer transaction. The execution adapter never fabricates
// an EVM transaction whose sender is a contract.
func NewExecutedMarketCreditBrowserFixture(t *testing.T, variants ...bool) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	if len(variants) > 1 {
		t.Fatal("one credit receiver variant is required")
	}
	keyBytes := make([]byte, 32)
	keyBytes[31] = 2
	ownerKey, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	owner := crypto.PubkeyToAddress(ownerKey.PublicKey)
	setup := loadCreditWallet(t, owner)
	setup.minedRevert = len(variants) == 1 && variants[0]
	sellerKey := executedMarketKey(t)
	scenario := newExecutionScenarioInputs(t, true, false, executionScenarioInputs{
		seller: znn.Address(crypto.PubkeyToAddress(sellerKey.PublicKey)), buyer: znn.Address(owner),
		timestamp: uint64(time.Now().Add(-4 * time.Minute).Unix()), provider: true, action: "credit", credit: setup,
		sign: func(s *executionScenario) []*Input {
			var inputs []*Input
			for _, order := range s.orders {
				input, err := Sign(sellerKey, Domain(s.cfg.L2ChainID, s.exchange), order)
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, input)
			}
			return inputs
		},
	})
	run := newExecutionRun(t, scenario)
	probe := &executedMarketRPC{Client: run.eth, hashes: map[common.Hash]int{}}
	run.book.cfg.Eth = probe
	f := &ExecutedMarketFixture{r: run, probe: probe}
	p := &ExecutedMarketProviderFixture{f: f, credit: &executedCreditTransfer{setup: setup}}
	run.backend.provider = p
	p.source = httptest.NewServer(http.HandlerFunc(p.sourceRPC))
	t.Cleanup(p.source.Close)
	run.d.L1 = znn.NewClient(p.source.URL)
	return f, p
}

func (f *ExecutedMarketFixture) CreditWallet() common.Address { return f.r.scenario.credit.wallet }
func (f *ExecutedMarketFixture) CreditWalletABI() json.RawMessage {
	return append(json.RawMessage(nil), f.r.scenario.credit.rawABI...)
}

func (p *ExecutedMarketProviderFixture) creditWalletCall() ([]byte, common.Address, *big.Int, error) {
	data, err := p.f.r.scenario.arts.MustGet("ZVMExchange").Call("withdrawCredits")
	if err != nil {
		return nil, p.f.CreditWallet(), nil, err
	}
	wrapped, err := p.credit.setup.abi.Pack("execute", p.f.Exchange(), data)
	return wrapped, p.f.CreditWallet(), new(big.Int), err
}

func (p *ExecutedMarketProviderFixture) pinCreditActor(t *testing.T) {
	t.Helper()
	if p.f.r.d.Head().Height != creditFundingHeight {
		t.Fatal("credit owner must bind at the joined setup prefix")
	}
	ctx := context.Background()
	nonce, err := p.f.r.eth.NonceAt(ctx, p.f.Buyer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.f.r.backend.mu.Lock()
	chain := p.f.r.backend.chain
	genesis, err := chain.StateAt(chain.GetBlockByNumber(0).Root())
	if err != nil {
		p.f.r.backend.mu.Unlock()
		t.Fatal(err)
	}
	expected := genesis.GetNonce(p.f.Buyer())
	for number := uint64(1); number <= chain.CurrentBlock().Number.Uint64(); number++ {
		for _, tx := range chain.GetBlockByNumber(number).Transactions() {
			if tx.Type() == types.DepositTxType && tx.From() == p.f.Buyer() {
				expected++
			}
		}
	}
	p.f.r.backend.mu.Unlock()
	balance, err := p.f.r.eth.BalanceAt(ctx, p.f.Buyer(), nil)
	if err != nil || nonce != expected || balance.Cmp(big.NewInt(1_000_000_000_000_000_000)) < 0 {
		t.Fatal("credit owner nonce and funding do not match the actual executed setup")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonceKnown || p.credit.actorBalance != nil {
		t.Fatal("credit owner was already bound")
	}
	p.initialNonce, p.nonceKnown = nonce, true
	p.credit.actorBalance = new(big.Int).Set(balance)
}

func (p *ExecutedMarketProviderFixture) AssertCreditReceiverRefusal(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	rejected, submissions, publications := p.credit.rejectedEstimates, p.submissions, p.publications
	p.mu.Unlock()
	if rejected < 1 || rejected > 8 || submissions != 0 || publications != 0 || p.f.r.d.Head().Height != creditSaleHeight {
		t.Fatal("rejecting receiver did not refuse the actual page estimate before a wallet submission")
	}
	p.assertCreditRejectionData(t)
	p.AssertCreditState(t, false)
}

func (p *ExecutedMarketProviderFixture) assertCreditRejectionData(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	selector := hexutil.Encode(crypto.Keccak256([]byte("PayoutFailed()"))[:4])
	if len(p.credit.rejectedData) != p.credit.rejectedEstimates || len(p.credit.rejectedData) == 0 {
		t.Fatal("credit receiver rejection data was not retained")
	}
	for _, data := range p.credit.rejectedData {
		if data != selector {
			t.Fatal("credit estimate failed for a different reason than recipient payout rejection")
		}
	}
}

func (p *ExecutedMarketProviderFixture) creditAccountingReceipt(t *testing.T) *types.Receipt {
	t.Helper()
	hash := p.TransactionHash()
	p.f.r.backend.mu.Lock()
	defer p.f.r.backend.mu.Unlock()
	block, tx, index := (executionEth{p.f.r.backend}).canonicalTransaction(hash)
	if block == nil || tx == nil {
		t.Fatal("credit accounting transaction is not canonical")
	}
	receipts := p.f.r.backend.chain.GetReceiptsByHash(block.Hash())
	want := uint64(types.ReceiptStatusSuccessful)
	if p.credit.setup.minedRevert {
		want = types.ReceiptStatusFailed
	}
	if index >= len(receipts) || receipts[index] == nil || receipts[index].TxHash != hash ||
		receipts[index].Status != want || receipts[index].Type != types.DynamicFeeTxType ||
		receipts[index].GasUsed <= 21_000 || receipts[index].GasUsed >= tx.Gas() {
		t.Fatal("credit receipt status or gas headroom differs from the intended executed outcome")
	}
	copy := *receipts[index]
	return &copy
}

func (p *ExecutedMarketProviderFixture) assertCreditRevertedCanonical(t *testing.T, included bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if included {
		p.mu.Lock()
		observed := p.executedReceipts
		p.mu.Unlock()
		if observed == 0 {
			t.Fatal("application did not observe the actual reverted credit receipt")
		}
		p.creditAccountingReceipt(t)
	} else if receipt, err := p.f.r.eth.TransactionReceipt(ctx, p.TransactionHash()); err == nil || receipt != nil {
		t.Fatal("removed reverted credit transaction still has a canonical receipt")
	}
	want := p.ActorNonce()
	if included {
		want++
	}
	nonce, err := p.f.r.eth.NonceAt(ctx, p.Actor(), nil)
	if err != nil || nonce != want {
		t.Fatal("reverted credit transaction did not retain its actual owner nonce semantics")
	}
}

// Independent hash-selected code, ABI and native-state reads check every phase.
// Internal accounting receipt reads do not increment application observations.
func (p *ExecutedMarketProviderFixture) AssertCreditState(t *testing.T, included bool) map[string]any {
	t.Helper()
	f, setup := p.f, p.credit.setup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hash := f.HeadHash()
	runtime, err := f.r.eth.CodeAtHash(ctx, setup.wallet, hash)
	if err != nil || !bytes.Equal(runtime, setup.runtime) {
		t.Fatal("executed credit wallet runtime differs from compiled source")
	}
	call := func(to common.Address, data []byte) []byte {
		raw, err := f.r.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &to, Data: data}, hash)
		if err != nil || len(raw) != 32 {
			t.Fatal("hash-selected credit state call failed")
		}
		return raw
	}
	walletCall := func(method string) []byte {
		data, err := setup.abi.Pack(method)
		if err != nil {
			t.Fatal(err)
		}
		return call(setup.wallet, data)
	}
	if common.BytesToAddress(walletCall("owner")) != f.Buyer() {
		t.Fatal("credit wallet owner differs")
	}
	rejects := new(big.Int).SetBytes(walletCall("rejectsNative")).Sign() != 0
	wantRejects := f.r.d.Head().Height < creditAcceptHeight || (setup.minedRevert && f.r.d.Head().Height > creditFundingHeight)
	if rejects != wantRejects {
		t.Fatal("executed credit receiver mode differs from its source phase")
	}
	scheduled := new(big.Int).SetBytes(walletCall("rejectFromBlock"))
	wantScheduled := new(big.Int)
	if setup.minedRevert && f.r.d.Head().Height >= creditAcceptHeight {
		wantScheduled.SetUint64(creditFundingHeight)
	}
	if scheduled.Cmp(wantScheduled) != 0 {
		t.Fatal("credit receiver rejection threshold differs from its source call")
	}
	data, err := f.r.scenario.arts.MustGet("ZVMExchange").Call("credits", setup.wallet)
	if err != nil {
		t.Fatal(err)
	}
	credit := new(big.Int).SetBytes(call(f.Exchange(), data))
	feeData, err := f.r.scenario.arts.MustGet("ZVMExchange").Call("feeBps")
	if err != nil {
		t.Fatal(err)
	}
	if new(big.Int).SetBytes(call(f.Exchange(), feeData)).Cmp(big.NewInt(200)) != 0 {
		t.Fatal("credit setup fee differs")
	}
	recipientData, err := f.r.scenario.arts.MustGet("ZVMExchange").Call("feeRecipient")
	if err != nil {
		t.Fatal(err)
	}
	if common.BytesToAddress(call(f.Exchange(), recipientData)) != setup.wallet {
		t.Fatal("credit setup fee recipient differs")
	}
	balance := func(address common.Address) *big.Int {
		var value hexutil.Big
		err := f.r.eth.Client().CallContext(ctx, &value, "eth_getBalance", address, rpc.BlockNumberOrHashWithHash(hash, true))
		if err != nil {
			t.Fatal(err)
		}
		return new(big.Int).Set((*big.Int)(&value))
	}
	contract, exchange, owner := balance(setup.wallet), balance(f.Exchange()), balance(f.Buyer())
	amount := big.NewInt(5_000_000_000_000_000)
	wantCredit, wantWallet := new(big.Int).Set(amount), new(big.Int)
	paid := included && !setup.minedRevert
	if paid {
		wantCredit.SetInt64(0)
		wantWallet.Set(amount)
	}
	if credit.Cmp(wantCredit) != 0 || exchange.Cmp(wantCredit) != 0 || contract.Cmp(wantWallet) != 0 {
		t.Fatalf("executed credit backing/payout differs: credit=%s exchange=%s recipient=%s", credit, exchange, contract)
	}
	if f.ExecutedOwner(t, 1) != f.Buyer() || f.ExecutedOwner(t, 2) != f.Seller() {
		t.Fatal("withdrawal recovery changed the already executed setup sale")
	}
	p.mu.Lock()
	var initial *big.Int
	if p.credit.actorBalance != nil {
		initial = new(big.Int).Set(p.credit.actorBalance)
	}
	p.mu.Unlock()
	if initial != nil {
		expected := new(big.Int).Set(initial)
		if included {
			expected.Sub(expected, batchTransactionFee(t, p.creditAccountingReceipt(t)))
		}
		if owner.Cmp(expected) != 0 {
			t.Fatal("outer owner debit differs from the executed receipt fee")
		}
	}
	header, err := f.r.eth.HeaderByHash(ctx, hash)
	if err != nil || header == nil {
		t.Fatal("credit canonical header is unavailable")
	}
	return map[string]any{"canonicalHeader": hash.Hex(), "canonicalL2Number": header.Number.String(), "credit": credit.String(), "exchangeNativeBalance": exchange.String(),
		"recipientNativeBalance": contract.String(), "ownerNativeBalance": owner.String(), "rejectsNative": rejects,
		"withdrawalIncluded": included, "actualCompiledWalletRuntimeChecked": true, "hashSelectedReads": true,
		"withdrawalReverted": included && setup.minedRevert, "nativePayoutExecuted": paid, "rejectFromL2Block": scheduled.String(),
		"sourceSalePreserved": true, "exchangeFeeBps": 200, "directStateOrStorageInsertion": false}
}

func (p *ExecutedMarketProviderFixture) CreditEvidence(t *testing.T) map[string]any {
	t.Helper()
	p.assertCreditRejectionData(t)
	value := p.Evidence()
	delete(value, "nativeBuySubstitutions")
	p.mu.Lock()
	value["rejectingReceiverStatefulEstimates"] = p.credit.rejectedEstimates
	value["statefulPayoutRejectionData"] = append([]string(nil), p.credit.rejectedData...)
	p.mu.Unlock()
	value["declaredNativeSaleSetupTransactions"] = 1
	value["setupSaleHeight"], value["acceptingReceiverHeight"] = creditSaleHeight, creditAcceptHeight
	value["contractAccount"], value["outerEOAOwner"] = p.f.CreditWallet().Hex(), p.f.Buyer().Hex()
	value["walletSourceSha256"], value["compilerSha256"] = p.credit.setup.sourceSHA, p.credit.setup.compilerSHA
	value["compilerArtifactSha256"] = p.credit.setup.artifactSHA
	value["walletRuntimeSha256"] = creditSourceSHA(p.credit.setup.runtime)
	value["accountContractSenderFabricated"] = false
	value["genericSafeUserOperationOrPhysicalWalletQualified"] = false
	value["expectedCredit"] = "5000000000000000"
	value["minedRevert"] = p.credit.setup.minedRevert
	value["expectedPayoutFailedSelector"] = hexutil.Encode(crypto.Keccak256([]byte("PayoutFailed()"))[:4])
	return value
}
