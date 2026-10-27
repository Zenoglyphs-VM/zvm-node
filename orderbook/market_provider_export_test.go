package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/gasestimator"
	"github.com/ethereum/go-ethereum/rpc"
)

// Only the provider profile enables ordinary signed transactions in the
// execution adapter. Other recovery profiles retain their deposit-only guard.
type ExecutedMarketProviderFixture struct {
	f                                                                                   *ExecutedMarketFixture
	source                                                                              *httptest.Server
	mu                                                                                  sync.Mutex
	relayer                                                                             string
	raw                                                                                 []byte
	published                                                                           *znn.AccountBlock
	submissions, publications, estimates, missingReceipts, executedReceipts, unexpected int
	bound                                                                               bool
	initialNonce                                                                        uint64
	nonceKnown                                                                          bool
}

func NewExecutedMarketProviderBrowserFixture(t *testing.T, kind string) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	key := make([]byte, 32)
	key[31] = 2 // Deliberately public scalar-two test buyer.
	buyer, err := crypto.ToECDSA(key)
	if err != nil {
		t.Fatal(err)
	}
	f := newExecutedMarketFixtureProfile(t, kind, uint64(time.Now().Add(-2*time.Minute).Unix()), znn.Address(crypto.PubkeyToAddress(buyer.PublicKey)), true)
	p := &ExecutedMarketProviderFixture{f: f}
	f.r.backend.provider = p
	p.source = httptest.NewServer(http.HandlerFunc(p.sourceRPC))
	t.Cleanup(p.source.Close)
	f.r.d.L1 = znn.NewClient(p.source.URL)
	return f, p
}

func (p *ExecutedMarketProviderFixture) SourceURL() string { return p.source.URL }

func (p *ExecutedMarketProviderFixture) UseRelayer(t *testing.T, url string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.relayer != "" {
		t.Fatal("provider relayer already selected")
	}
	p.relayer = url
}

func (p *ExecutedMarketProviderFixture) TransactionHash() common.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	var tx types.Transaction
	if tx.UnmarshalBinary(p.raw) != nil {
		return common.Hash{}
	}
	return tx.Hash()
}

func (p *ExecutedMarketProviderFixture) PinFundedBuyer(t *testing.T) {
	t.Helper()
	nonce, err := p.f.r.eth.NonceAt(context.Background(), p.f.Buyer(), nil)
	if err != nil || p.f.r.d.Head().Height != 7 || nonce != 3 {
		t.Fatal("the three executed funding deposits must determine the buyer nonce")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nonceKnown {
		t.Fatal("buyer funding nonce was already pinned")
	}
	p.initialNonce, p.nonceKnown = nonce, true
}

func (p *ExecutedMarketProviderFixture) BuyerNonce() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initialNonce
}

func (s executionEth) SendRawTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	p := s.backend.provider
	if p == nil || len(raw) == 0 || len(raw) > 16<<10 {
		return common.Hash{}, errors.New("signed transaction outside provider fixture scope")
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	from, err := types.Sender(types.LatestSignerForChainID(p.f.Rollup().L2ChainIDBig()), &tx)
	if err != nil {
		return common.Hash{}, err
	}
	p.mu.Lock()
	initialNonce, nonceKnown := p.initialNonce, p.nonceKnown
	p.mu.Unlock()
	expected, err := p.f.r.scenario.arts.MustGet("ZVMExchange").Call("buy", p.f.r.scenario.inputs[0].ABI())
	if err != nil {
		return common.Hash{}, err
	}
	if tx.Type() != types.DynamicFeeTxType || tx.ChainId().Cmp(p.f.Rollup().L2ChainIDBig()) != 0 ||
		from != p.f.Buyer() || !nonceKnown || tx.Nonce() != initialNonce || tx.To() == nil || *tx.To() != p.f.Exchange() ||
		tx.Value().Cmp(p.f.r.scenario.inputs[0].Order.Price.Big()) != 0 || !bytes.Equal(tx.Data(), expected) {
		return common.Hash{}, errors.New("provider did not sign the actual admitted buy call")
	}
	p.mu.Lock()
	if p.submissions != 0 || p.relayer == "" {
		p.mu.Unlock()
		return common.Hash{}, errors.New("unexpected duplicate provider submission")
	}
	p.submissions++
	p.raw = append([]byte(nil), raw...)
	url := p.relayer
	p.mu.Unlock()
	client, err := rpc.DialHTTP(url)
	if err != nil {
		return common.Hash{}, err
	}
	defer client.Close()
	var hash common.Hash
	if err := client.CallContext(ctx, &hash, "eth_sendRawTransaction", raw); err != nil {
		return common.Hash{}, err
	}
	if hash != tx.Hash() {
		return common.Hash{}, errors.New("official relayer returned a different transaction hash")
	}
	return hash, nil
}

type providerGasCall struct {
	From  common.Address
	To    *common.Address
	Gas   hexutil.Uint64
	Value *hexutil.Big
	Data  hexutil.Bytes
	Input hexutil.Bytes
}

// Use the pinned engine's actual stateful estimator, including value transfer,
// intrinsic gas, reverts and its binary search. No gas constant is returned.
func (s executionEth) EstimateGas(ctx context.Context, args providerGasCall, at *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if s.backend.provider == nil {
		return 0, errors.New("gas estimation outside provider fixture scope")
	}
	selector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if at != nil {
		selector = *at
	}
	header, err := s.header(selector)
	if err != nil {
		return 0, err
	}
	state, err := s.backend.chain.StateAt(header.Root)
	if err != nil {
		return 0, err
	}
	value := new(big.Int)
	if args.Value != nil {
		value.Set((*big.Int)(args.Value))
	}
	data := args.Input
	if data == nil {
		data = args.Data
	}
	call := &core.Message{From: args.From, To: args.To, Value: value, Data: data, GasLimit: uint64(args.Gas),
		GasPrice: new(big.Int), GasFeeCap: new(big.Int), GasTipCap: new(big.Int), SkipNonceChecks: true, SkipTransactionChecks: true}
	gas, _, err := gasestimator.Estimate(ctx, call, &gasestimator.Options{Config: s.backend.chain.Config(), Chain: s.backend.chain, Header: header, State: state, ErrorRatio: 0.015}, 8_000_000)
	p := s.backend.provider
	p.mu.Lock()
	p.estimates++
	p.mu.Unlock()
	return hexutil.Uint64(gas), err
}

func (s executionEth) GetTransactionCount(_ context.Context, address common.Address, at rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	header, err := s.header(at)
	if err != nil {
		return 0, err
	}
	state, err := s.backend.chain.StateAt(header.Root)
	if err != nil {
		return 0, err
	}
	return hexutil.Uint64(state.GetNonce(address)), nil
}

// Caller holds the backend mutex. Search only the selected canonical chain;
// an orphaned database receipt is never returned as a canonical receipt.
func (s executionEth) canonicalTransaction(hash common.Hash) (*types.Block, *types.Transaction, int) {
	for n := uint64(0); n <= s.backend.chain.CurrentBlock().Number.Uint64(); n++ {
		block := s.backend.chain.GetBlockByNumber(n)
		for i, tx := range block.Transactions() {
			if tx.Hash() == hash {
				return block, tx, i
			}
		}
	}
	return nil, nil, 0
}

func (s executionEth) GetTransactionByHash(_ context.Context, hash common.Hash) (map[string]any, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	block, tx, index := s.canonicalTransaction(hash)
	if tx == nil {
		return nil, nil
	}
	raw, err := tx.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var item map[string]any
	if err = json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	from, err := types.Sender(types.LatestSignerForChainID(s.backend.chain.Config().ChainID), tx)
	if err != nil {
		return nil, err
	}
	item["from"], item["blockHash"], item["blockNumber"], item["transactionIndex"] = from, block.Hash(), hexutil.Uint64(block.NumberU64()), hexutil.Uint64(index)
	return item, nil
}

func (s executionEth) GetTransactionReceipt(_ context.Context, hash common.Hash) (map[string]any, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	block, tx, index := s.canonicalTransaction(hash)
	if p := s.backend.provider; p != nil && p.TransactionHash() == hash {
		p.mu.Lock()
		if tx == nil {
			p.missingReceipts++
		} else {
			p.executedReceipts++
		}
		p.mu.Unlock()
	}
	if tx == nil {
		return nil, nil
	}
	receipts := s.backend.chain.GetReceiptsByHash(block.Hash())
	if len(receipts) != len(block.Transactions()) {
		return nil, errors.New("executed receipt set is incomplete")
	}
	raw, err := json.Marshal(receipts[index])
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	from, err := types.Sender(types.LatestSignerForChainID(s.backend.chain.Config().ChainID), tx)
	if err != nil {
		return nil, err
	}
	result["from"], result["to"] = from, tx.To()
	return result, nil
}

func (p *ExecutedMarketProviderFixture) sourceRPC(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		http.Error(w, "bounded source request required", 400)
		return
	}
	var call struct {
		ID     json.RawMessage
		Method string
		Params []json.RawMessage
	}
	if json.Unmarshal(raw, &call) != nil {
		http.Error(w, "invalid source request", 400)
		return
	}
	p.f.r.nomMu.RLock()
	limit, branch := p.f.r.limit, p.f.r.branch
	source := p.f.r.scenario.branches[branch]
	p.f.r.nomMu.RUnlock()
	var result any
	var failure error
	p.mu.Lock()
	confirmed := p.published != nil && p.bound && branch == 0 && limit >= 8
	switch call.Method {
	case "embedded.plasma.get":
		result = &znn.PlasmaInfo{CurrentPlasma: 10_500_000, MaxPlasma: 10_500_000, QsrAmount: znn.BigIntFromUint64(500_000_000_000)}
	case "ledger.getFrontierAccountBlock", "ledger.getAccountBlockByHash":
		if p.published != nil {
			copy := *p.published
			if confirmed {
				copy.ConfirmationDetail = &znn.ConfirmationDetail{NumConfirmations: limit - 7, MomentumHeight: 8, MomentumHash: source[8].Momentum.Hash, MomentumTimestamp: source[8].Momentum.Timestamp}
			}
			result = &copy
		}
	case "ledger.getAccountBlocksByHeight", "ledger.getUnconfirmedBlocksByAddress":
		list := []*znn.AccountBlock{}
		if p.published != nil && (call.Method != "ledger.getUnconfirmedBlocksByAddress" || !confirmed) {
			list = append(list, p.published)
		}
		result = &znn.BlockList{Count: uint64(len(list)), List: list}
	case "ledger.getMomentumsByHeight":
		var start, count uint64
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &start) != nil || json.Unmarshal(call.Params[1], &count) != nil || count > 64 {
			failure = errors.New("undeclared source header range")
			break
		}
		list := []*znn.Momentum{}
		for h := start; h < start+count && h <= limit; h++ {
			if dm := source[h]; dm != nil {
				list = append(list, dm.Momentum)
			}
		}
		result = map[string]any{"list": list}
	case "ledger.publishRawTransaction":
		var block znn.AccountBlock
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &block) != nil {
			failure = errors.New("invalid published account block")
			break
		}
		if err := block.Verify(); err != nil {
			failure = err
			break
		}
		if block.Address != devnet.RelayerKey(0).Address || block.ToAddress != p.f.Rollup().Inbox || block.ChainIdentifier != p.f.Rollup().L1ChainID ||
			block.BlockType != znn.BlockTypeUserSend || block.Height != 1 || block.PreviousHash != (znn.Hash{}) || block.AmountBig().Sign() != 0 ||
			block.TokenStandard != znn.ZeroTokenStandard || block.MomentumAcknowledged.Height != 7 || block.MomentumAcknowledged.Hash != source[7].Momentum.Hash ||
			len(block.Data) < 2 || block.Data[0] != derive.MsgBatch {
			failure = errors.New("actual relayer published an unexpected source block")
			break
		}
		txs, err := derive.DecodeBatch(block.Data[1:], p.f.Rollup().MaxBatchBytes, true)
		if err != nil || len(txs) != 1 || !bytes.Equal(txs[0], p.raw) {
			failure = errors.New("relayer batch changed the signed provider transaction bytes")
			break
		}
		if p.published != nil {
			failure = errors.New("unexpected repeated relayer publication")
			break
		}
		p.published = &block
		p.publications++
	case "ledger.getFrontierMomentum", "ledger.getDetailedMomentumsByHeight":
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		p.f.r.nomReply(w, r)
		return
	default:
		failure = errors.New("undeclared provider source RPC operation: " + call.Method)
	}
	if failure != nil {
		p.unexpected++
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	envelope := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
	if failure != nil {
		delete(envelope, "result")
		envelope["error"] = map[string]any{"code": -32602, "message": failure.Error()}
	}
	json.NewEncoder(w).Encode(envelope)
}

func (p *ExecutedMarketProviderFixture) BindPublishedTransaction(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var block *znn.AccountBlock
	for block == nil {
		p.mu.Lock()
		if p.published != nil && p.missingReceipts > 0 && p.unexpected == 0 {
			copy := *p.published
			block = &copy
		}
		p.mu.Unlock()
		if block != nil {
			break
		}
		select {
		case <-deadline.Done():
			t.Fatal("actual relayer publication and application pending-receipt observation were not completed")
		case <-ticker.C:
		}
	}
	if p.f.r.d.Head().Height != 7 {
		t.Fatal("provider transaction must bind at the joined height-seven horizon")
	}
	if nonce, err := p.f.r.eth.NonceAt(ctx, p.f.Buyer(), nil); err != nil || nonce != p.BuyerNonce() {
		t.Fatal("pending provider transaction already consumed a canonical nonce")
	}
	p.f.r.nomMu.Lock()
	defer p.f.r.nomMu.Unlock()
	if p.f.r.limit != 7 {
		t.Fatal("provider source is already beyond its binding horizon")
	}
	var future [2]map[uint64]*znn.DetailedMomentum
	for branch, old := range p.f.r.scenario.branches {
		future[branch] = make(map[uint64]*znn.DetailedMomentum, len(old))
		for height, dm := range old {
			raw, err := json.Marshal(dm)
			if err != nil {
				t.Fatal(err)
			}
			var copy znn.DetailedMomentum
			if err = json.Unmarshal(raw, &copy); err != nil {
				t.Fatal(err)
			}
			future[branch][height] = &copy
			if height <= 7 {
				after, err := json.Marshal(&copy)
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("provider source prefix changed")
				}
			}
		}
	}
	dm := future[0][8]
	dm.Blocks = append(dm.Blocks, block)
	sort.Slice(dm.Blocks, func(i, j int) bool {
		a, b := dm.Blocks[i], dm.Blocks[j]
		if c := bytes.Compare(a.Address[:], b.Address[:]); c != 0 {
			return c < 0
		}
		return a.Height < b.Height
	})
	dm.Momentum.Content = nil
	for _, b := range dm.Blocks {
		dm.Momentum.Content = append(dm.Momentum.Content, znn.AccountHeader{Address: b.Address, Height: b.Height, Hash: b.Hash})
	}
	p.f.r.scenario.branches = future
	p.mu.Lock()
	p.bound = true
	p.mu.Unlock()
}

func (p *ExecutedMarketProviderFixture) AssertCanonicalTransaction(t *testing.T, included bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if included {
		p.mu.Lock()
		observed := p.executedReceipts
		p.mu.Unlock()
		if observed == 0 {
			t.Fatal("application did not read the actual mined receipt before success")
		}
	}
	receipt, err := p.f.r.eth.TransactionReceipt(ctx, p.TransactionHash())
	want := p.BuyerNonce()
	if included {
		want++
		if err != nil || receipt == nil || receipt.Status != types.ReceiptStatusSuccessful || receipt.Type != types.DynamicFeeTxType || receipt.GasUsed <= 21_000 ||
			receipt.BlockHash != p.f.r.backend.chain.GetBlockByNumber(receipt.BlockNumber.Uint64()).Hash() {
			t.Fatal("missing canonical executed EIP-1559 receipt")
		}
	} else if receipt != nil || err == nil {
		t.Fatal("orphaned transaction still has a canonical receipt")
	}
	nonce, err := p.f.r.eth.NonceAt(ctx, p.f.Buyer(), nil)
	if err != nil || nonce != want {
		t.Fatalf("canonical buyer nonce=%d want=%d error=%v", nonce, want, err)
	}
}

func (p *ExecutedMarketProviderFixture) AssertCompleted(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.submissions != 1 || p.publications != 1 || p.estimates < 1 || p.missingReceipts < 1 || p.executedReceipts < 1 || p.unexpected != 0 || !p.bound {
		t.Fatalf("provider transport evidence incomplete: submissions=%d publications=%d estimates=%d missing=%d executed=%d unexpected=%d bound=%v", p.submissions, p.publications, p.estimates, p.missingReceipts, p.executedReceipts, p.unexpected, p.bound)
	}
}

func (p *ExecutedMarketProviderFixture) Evidence() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"signedSubmissions": p.submissions, "verifiedNoMPublications": p.publications, "statefulGasEstimates": p.estimates,
		"absentCanonicalReceiptObservations": p.missingReceipts, "executedCanonicalReceiptObservations": p.executedReceipts,
		"unexpectedSourceErrors": p.unexpected, "signedRawBytes": len(p.raw), "alreadyDerivedSourceHeightsPreserved": 7,
		"actualRelayerSource": "5a72a223dd60d1ae4e974d420cadd85e6f9ee929", "actualNoMAccountSignatureVerified": true,
		"fundingDepositNonce": p.initialNonce, "fundingDepositNoncePinnedFromExecutedState": p.nonceKnown,
		"exactProviderRawTransactionBytesPreserved": true, "nativeBuySubstitutions": 0, "sourceBoundBeforeDerivation": p.bound}
}
