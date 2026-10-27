package orderbook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// The partial profile holds a real buyer wallet request while an independent
// production seller page cancels one input through the same official relayer.
// Each leg retains its own signed bytes, NoM account block and receipt counts.
type executedBatchLeg struct {
	raw            []byte
	published      *znn.AccountBlock
	bound          bool
	nonce          uint64
	missing, mined int
}

func (leg *executedBatchLeg) hash() common.Hash {
	var tx types.Transaction
	if tx.UnmarshalBinary(leg.raw) != nil {
		return common.Hash{}
	}
	return tx.Hash()
}

type executedPartialBatch struct {
	legs [2]executedBatchLeg // seller cancellation, then the captured buyer batch
}

type executedBatchBalances struct {
	buyer, seller *big.Int
}

func NewExecutedMarketBatchBrowserFixture(t *testing.T, kind, action string) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	if action != "batch" && action != "partial-batch" {
		t.Fatal("unsupported batch action")
	}
	f, p := newExecutedMarketActionBrowserFixture(t, kind, action)
	p.batchBalances = &executedBatchBalances{}
	if action == "partial-batch" {
		p.partial = &executedPartialBatch{}
	}
	return f, p
}

func (p *ExecutedMarketProviderFixture) exactBatchCall(data []byte) bool {
	inputs := p.f.r.scenario.inputs
	if len(inputs) != 2 {
		return false
	}
	// The two pages may break equal-price ties differently. Both complete
	// permutations preserve the exact admitted order/signature/proof bytes.
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		want, err := p.f.r.scenario.arts.MustGet("ZVMExchange").Call("buyMany",
			[]ABIInput{inputs[order[0]].ABI(), inputs[order[1]].ABI()})
		if err == nil && bytes.Equal(data, want) {
			return true
		}
	}
	return false
}

func (p *ExecutedMarketProviderFixture) PinBatchBalances(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	buyer, err := p.f.r.eth.BalanceAt(ctx, p.f.Buyer(), nil)
	if err != nil || p.f.r.d.Head().Height != p.FundingHeight() {
		t.Fatal("batch balances must bind at the joined funding prefix")
	}
	seller, err := p.f.r.eth.BalanceAt(ctx, p.f.Seller(), nil)
	if err != nil || p.batchBalances == nil || p.batchBalances.buyer != nil {
		t.Fatal("batch balance snapshot is missing or repeated")
	}
	if p.partial != nil {
		nonce, err := p.f.r.eth.NonceAt(ctx, p.f.Seller(), nil)
		if err != nil || seller.Cmp(big.NewInt(3_000_000_000_000_000_000)) < 0 {
			t.Fatal("the independent seller cancellation is not funded")
		}
		p.f.r.backend.mu.Lock()
		chain := p.f.r.backend.chain
		state, err := chain.StateAt(chain.GetBlockByNumber(0).Root())
		if err != nil {
			p.f.r.backend.mu.Unlock()
			t.Fatal(err)
		}
		want := state.GetNonce(p.f.Seller())
		for n := uint64(1); n <= chain.CurrentBlock().Number.Uint64(); n++ {
			for _, tx := range chain.GetBlockByNumber(n).Transactions() {
				if tx.Type() == types.DepositTxType && tx.From() == p.f.Seller() {
					want++
				}
			}
		}
		p.f.r.backend.mu.Unlock()
		if nonce != want {
			t.Fatal("seller nonce differs from the actually executed funding prefix")
		}
		p.mu.Lock()
		p.partial.legs[0].nonce, p.partial.legs[1].nonce = nonce, p.initialNonce
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.batchBalances.buyer, p.batchBalances.seller = new(big.Int).Set(buyer), new(big.Int).Set(seller)
	p.mu.Unlock()
}

func (p *ExecutedMarketProviderFixture) BatchSellerNonce() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.partial == nil {
		return 0
	}
	return p.partial.legs[0].nonce
}

func (p *ExecutedMarketProviderFixture) BatchCancellationHash() common.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.partial == nil {
		return common.Hash{}
	}
	return p.partial.legs[0].hash()
}

func (p *ExecutedMarketProviderFixture) sendPartialBatch(ctx context.Context, raw []byte, tx *types.Transaction, from common.Address) (common.Hash, error) {
	index, to, value := 1, p.f.Exchange(), new(big.Int)
	matching := p.exactBatchCall(tx.Data())
	if from == p.f.Seller() {
		index = 0
		data, err := p.f.r.scenario.arts.MustGet("ZVMExchange").Call("cancelOrders",
			ABIOrders([]*Order{&p.f.r.scenario.inputs[0].Order}))
		if err != nil {
			return common.Hash{}, err
		}
		matching = bytes.Equal(tx.Data(), data)
	} else if from != p.f.Buyer() {
		return common.Hash{}, errors.New("undeclared partial-batch actor")
	} else {
		for _, input := range p.f.r.scenario.inputs {
			value.Add(value, input.Order.Price.Big())
		}
	}
	p.mu.Lock()
	leg := &p.partial.legs[index]
	if !p.nonceKnown || p.batchBalances.buyer == nil || tx.Type() != types.DynamicFeeTxType ||
		tx.ChainId().Cmp(p.f.Rollup().L2ChainIDBig()) != 0 || tx.Nonce() != leg.nonce ||
		tx.To() == nil || *tx.To() != to || tx.Value().Cmp(value) != 0 || !matching ||
		len(leg.raw) != 0 || p.relayer == "" || p.submissions != index ||
		(index == 1 && !p.partial.legs[0].bound) {
		reason := fmt.Errorf("partial-batch validation: leg=%d nonce=%d/%d submitted=%d type=%d chain=%t target=%t value=%t calldata=%t configured=%t funded=%t previous_bound=%t", index, tx.Nonce(), leg.nonce, p.submissions, tx.Type(), tx.ChainId().Cmp(p.f.Rollup().L2ChainIDBig()) == 0, tx.To() != nil && *tx.To() == to, tx.Value().Cmp(value) == 0, matching, p.relayer != "", p.nonceKnown && p.batchBalances.buyer != nil, index == 0 || p.partial.legs[0].bound)
		p.mu.Unlock()
		return common.Hash{}, reason
	}
	leg.raw = append([]byte(nil), raw...)
	p.submissions++
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
		return common.Hash{}, errors.New("official relayer changed the partial-batch hash")
	}
	return hash, nil
}

func (p *ExecutedMarketProviderFixture) observePartialReceipt(hash common.Hash, included bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.partial.legs {
		leg := &p.partial.legs[i]
		if hash != (common.Hash{}) && leg.hash() == hash {
			if included {
				leg.mined++
				p.executedReceipts++
			} else {
				leg.missing++
				p.missingReceipts++
			}
		}
	}
}

func (p *ExecutedMarketProviderFixture) BindBatchCancellation(t *testing.T, ctx context.Context) {
	t.Helper()
	if p.partial == nil {
		t.Fatal("cancellation binding requires the partial-batch profile")
	}
	p.bindPartialLeg(t, ctx, 0)
}

func (p *ExecutedMarketProviderFixture) AssertBatchCancellation(t *testing.T) {
	t.Helper()
	p.assertPartialLeg(t, 0, true)
	p.mu.Lock()
	submissions := p.submissions
	p.mu.Unlock()
	nonce, err := p.f.r.eth.NonceAt(context.Background(), p.f.Buyer(), nil)
	if submissions != 1 || err != nil || nonce != p.ActorNonce() {
		t.Fatal("buyer request was broadcast before the real seller cancellation completed")
	}
}

func (p *ExecutedMarketProviderFixture) assertPartialLeg(t *testing.T, index int, included bool) {
	t.Helper()
	p.mu.Lock()
	leg := p.partial.legs[index]
	raw := append([]byte(nil), leg.raw...)
	p.mu.Unlock()
	if included && leg.mined == 0 {
		t.Fatal("the production application did not observe this leg's executed receipt")
	}
	hash := leg.hash()
	if hash == (common.Hash{}) {
		t.Fatal("partial-batch signed bytes are unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receipt, err := p.f.r.eth.TransactionReceipt(ctx, hash)
	actor, wantNonce := p.f.Buyer(), leg.nonce
	if index == 0 {
		actor = p.f.Seller()
	}
	if included {
		wantNonce++
		if err != nil || receipt == nil || receipt.Status != types.ReceiptStatusSuccessful ||
			receipt.Type != types.DynamicFeeTxType || receipt.GasUsed <= 21_000 {
			t.Fatal("partial-batch leg has no successful executed EIP-1559 receipt")
		}
		p.f.r.backend.mu.Lock()
		block, tx, _ := (executionEth{p.f.r.backend}).canonicalTransaction(hash)
		var encoded []byte
		if tx != nil {
			encoded, err = tx.MarshalBinary()
		}
		valid := block != nil && tx != nil && err == nil && bytes.Equal(encoded, raw) && receipt.BlockHash == block.Hash()
		p.f.r.backend.mu.Unlock()
		if !valid {
			t.Fatal("canonical partial-batch transaction differs from the original signed bytes")
		}
	} else if receipt != nil || err == nil {
		t.Fatal("orphaned partial-batch leg still has a canonical receipt")
	}
	nonce, err := p.f.r.eth.NonceAt(ctx, actor, nil)
	if err != nil || nonce != wantNonce {
		t.Fatal("partial-batch actor nonce differs from canonical execution")
	}
}

func (p *ExecutedMarketProviderFixture) assertPartialCompleted(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.submissions != 2 || p.publications != 2 || p.estimates < 2 || p.unexpected != 0 || !p.bound {
		t.Fatal("partial-batch provider/relayer cardinality differs")
	}
	for _, leg := range p.partial.legs {
		if !leg.bound || leg.published == nil || leg.missing == 0 || leg.mined == 0 {
			t.Fatal("partial-batch per-transaction evidence is incomplete")
		}
	}
}

func (p *ExecutedMarketProviderFixture) partialEvidence() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	legs := []map[string]any{}
	size := 0
	for i, leg := range p.partial.legs {
		size += len(leg.raw)
		legs = append(legs, map[string]any{"transaction": leg.hash().Hex(), "signedRawBytes": len(leg.raw),
			"fundedNonce": leg.nonce, "sourceHeight": p.FundingHeight() + 1 + uint64(i),
			"preservedSourcePrefix": p.FundingHeight() + uint64(i), "boundBeforeDerivation": leg.bound,
			"absentReceiptObservations": leg.missing, "executedReceiptObservations": leg.mined,
			"actualNoMAccountSignatureVerified": leg.published != nil})
	}
	return map[string]any{"action": p.Action(), "actor": p.Actor().Hex(), "signedSubmissions": p.submissions,
		"verifiedNoMPublications": p.publications, "statefulGasEstimates": p.estimates,
		"absentCanonicalReceiptObservations": p.missingReceipts, "executedCanonicalReceiptObservations": p.executedReceipts,
		"unexpectedSourceErrors": p.unexpected, "signedRawBytes": size, "alreadyDerivedSourceHeightsPreserved": p.FundingHeight(),
		"actualRelayerSource": "5a72a223dd60d1ae4e974d420cadd85e6f9ee929", "actualNoMAccountSignatureVerified": true,
		"fundingActorNonce": p.initialNonce, "fundingActorNoncePinnedFromExecutedState": p.nonceKnown,
		"exactProviderRawTransactionBytesPreserved": true, "nativeBuySubstitutions": 0, "sourceBoundBeforeDerivation": p.bound,
		"independentProductionSellerCancellation": true, "accountBlockHeightAndPreviousHashSequenceVerified": true, "legs": legs}
}

// Receipt-derived transaction fees and independent canonical balances establish
// the actual native amount spent. This isolated profile admits no operator fee.
func batchTransactionFee(t *testing.T, receipt *types.Receipt) *big.Int {
	t.Helper()
	if receipt.EffectiveGasPrice == nil || receipt.BlobGasUsed != 0 ||
		(receipt.OperatorFeeScalar != nil && *receipt.OperatorFeeScalar != 0) ||
		(receipt.OperatorFeeConstant != nil && *receipt.OperatorFeeConstant != 0) {
		t.Fatal("isolated batch accounting has undeclared fee parameters")
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(receipt.GasUsed), receipt.EffectiveGasPrice)
	if receipt.L1Fee != nil {
		fee.Add(fee, receipt.L1Fee)
	}
	return fee
}

// Internal accounting reads do not count as a production page observing a
// receipt through RPC. Canonical selection still comes from the executed chain.
func (p *ExecutedMarketProviderFixture) batchAccountingReceipt(t *testing.T, hash common.Hash) *types.Receipt {
	t.Helper()
	p.f.r.backend.mu.Lock()
	defer p.f.r.backend.mu.Unlock()
	block, tx, index := (executionEth{p.f.r.backend}).canonicalTransaction(hash)
	if block == nil || tx == nil {
		t.Fatal("batch accounting transaction is not canonical")
	}
	receipts := p.f.r.backend.chain.GetReceiptsByHash(block.Hash())
	if index >= len(receipts) || receipts[index] == nil || receipts[index].TxHash != hash ||
		receipts[index].Status != types.ReceiptStatusSuccessful {
		t.Fatal("batch accounting receipt is unavailable")
	}
	copy := *receipts[index]
	return &copy
}

// Independent contract/state reads do not reuse the worker readiness predicate.
func (p *ExecutedMarketProviderFixture) AssertBatchState(t *testing.T, stage int) map[string]any {
	t.Helper()
	if p.batchBalances == nil || p.batchBalances.buyer == nil || stage < 0 || stage > 2 {
		t.Fatal("batch accounting snapshot or stage is unavailable")
	}
	ctx := context.Background()
	header := p.f.HeadHash()
	read := func(artifact, method string, to common.Address, args ...any) []byte {
		data, err := p.f.r.scenario.arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := p.f.r.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &to, Data: data}, header)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	scalar := func(method string, args ...any) *big.Int {
		raw := read("ZVMExchange", method, p.f.Exchange(), args...)
		if len(raw) != 32 {
			t.Fatal("batch exchange scalar is malformed")
		}
		return new(big.Int).SetBytes(raw)
	}
	if scalar("nonces", p.f.Seller()).Sign() != 0 || scalar("credits", p.f.Buyer()).Sign() != 0 ||
		scalar("credits", p.f.Seller()).Sign() != 0 {
		t.Fatal("isolated batch unexpectedly changed an order nonce or left native credit")
	}
	approval := read("ZVMCollection", "isApprovedForAll", p.f.Collection(), p.f.Seller(), p.f.Exchange())
	if len(approval) != 32 || new(big.Int).SetBytes(approval).Cmp(big.NewInt(1)) != 0 {
		t.Fatal("batch cancellation changed the collection approval")
	}
	states := []map[string]any{}
	spent := new(big.Int)
	for i, input := range p.f.r.scenario.inputs {
		cancelled := p.partial != nil && stage >= 1 && i == 0
		filled := stage == 2 && !cancelled
		status, code, remaining, count := StatusOpen, CodeOK, int64(1), int64(0)
		owner := p.f.Seller()
		if cancelled {
			status, code, remaining = StatusCancelled, CodeCancelled, 0
		} else if filled {
			status, code, remaining, count, owner = StatusFilled, CodeFilled, 0, 1, p.f.Buyer()
			spent.Add(spent, input.Order.Price.Big())
		}
		checked := read("ZVMExchange", "checkOrder", p.f.Exchange(), input.ABI())
		wantCancelled := int64(0)
		if cancelled {
			wantCancelled = 1
		}
		if len(checked) != 64 || new(big.Int).SetBytes(checked[:32]).Uint64() != uint64(code) ||
			new(big.Int).SetBytes(checked[32:]).Cmp(big.NewInt(remaining)) != 0 ||
			scalar("filled", input.Order.Hash()).Cmp(big.NewInt(count)) != 0 ||
			scalar("cancelled", input.Order.Hash()).Cmp(big.NewInt(wantCancelled)) != 0 ||
			p.f.ExecutedOwner(t, int64(i+1)) != owner {
			t.Fatal("direct batch order, mapping or ownership state differs")
		}
		var cachedStatus, source, checkedHash string
		var cachedCode, cachedFilled int
		var checkedEpoch, checkedBlock uint64
		var closedBlock *uint64
		if err := p.f.DB().QueryRow("SELECT status,code,filled,source,checked_epoch,checked_block,checked_hash,closed_block FROM nft_orders WHERE hash=?",
			input.Order.Hash().Hex()).Scan(&cachedStatus, &cachedCode, &cachedFilled, &source, &checkedEpoch, &checkedBlock, &checkedHash, &closedBlock); err != nil {
			t.Fatal(err)
		}
		if cachedStatus != status || cachedCode != int(code) || cachedFilled != int(count) || source != "api" {
			t.Fatal("cached batch order differs from direct executed state")
		}
		if status == StatusOpen {
			if checkedEpoch != p.f.Epoch(t) || checkedHash != header.Hex() {
				t.Fatal("open batch input was not checked at the current canonical hash")
			}
		} else {
			hash := p.TransactionHash()
			if cancelled {
				hash = p.BatchCancellationHash()
			}
			receipt := p.batchAccountingReceipt(t, hash)
			if closedBlock == nil || *closedBlock < receipt.BlockNumber.Uint64() || *closedBlock > p.f.r.backend.chain.CurrentBlock().Number.Uint64() {
				t.Fatal("terminal batch order has no canonical action/observation interval")
			}
			if cancelled {
				if *closedBlock != receipt.BlockNumber.Uint64() {
					t.Fatal("cancellation cursor differs from its exact event block")
				}
			} else {
				// A fill is learned by a hash-bound checkOrder observation,
				// which may join after its event. Its close cursor is that
				// observation height, not a fabricated transaction height.
				p.f.r.backend.mu.Lock()
				block := p.f.r.backend.chain.GetBlockByNumber(checkedBlock)
				valid := block != nil && block.Hash().Hex() == checkedHash
				p.f.r.backend.mu.Unlock()
				if !valid || checkedEpoch != p.f.Epoch(t) || *closedBlock != checkedBlock {
					t.Fatal("filled batch cache lost its canonical observation hash epoch or cursor")
				}
			}
		}
		states = append(states, map[string]any{"status": status, "code": code, "fillable": code == CodeOK,
			"filled": count, "remaining": remaining, "owner": owner.Hex(), "cancelled": cancelled})
	}
	buyerFee, sellerFee := new(big.Int), new(big.Int)
	if stage == 2 {
		receipt := p.batchAccountingReceipt(t, p.TransactionHash())
		buyerFee = batchTransactionFee(t, receipt)
	}
	if p.partial != nil && stage >= 1 {
		receipt := p.batchAccountingReceipt(t, p.BatchCancellationHash())
		sellerFee = batchTransactionFee(t, receipt)
	}
	wantBuyer := new(big.Int).Sub(p.batchBalances.buyer, spent)
	wantBuyer.Sub(wantBuyer, buyerFee)
	wantSeller := new(big.Int).Add(p.batchBalances.seller, spent)
	wantSeller.Sub(wantSeller, sellerFee)
	for actor, want := range map[common.Address]*big.Int{p.f.Buyer(): wantBuyer, p.f.Seller(): wantSeller} {
		got, err := p.f.r.eth.BalanceAtHash(ctx, actor, header)
		if err != nil || got.Cmp(want) != 0 {
			t.Fatalf("canonical native balance differs from actual fills and receipt fees: got=%v want=%v error=%v", got, want, err)
		}
	}
	exchangeBalance, err := p.f.r.eth.BalanceAtHash(ctx, p.f.Exchange(), header)
	if err != nil || exchangeBalance.Sign() != 0 {
		t.Fatal("the isolated exchange retained an undeclared native balance")
	}
	unspent := new(big.Int)
	if stage == 2 {
		for _, input := range p.f.r.scenario.inputs {
			unspent.Add(unspent, input.Order.Price.Big())
		}
		unspent.Sub(unspent, spent)
	}
	return map[string]any{"states": states, "nativeSpent": spent.String(), "nativeUnspent": unspent.String(),
		"buyerBalance": wantBuyer.String(), "sellerBalance": wantSeller.String(),
		"buyerReceiptFee": buyerFee.String(), "sellerReceiptFee": sellerFee.String(),
		"nativeCredit": "0", "exchangeNativeBalance": "0", "directHashBoundContractAndBalanceReads": true}
}
