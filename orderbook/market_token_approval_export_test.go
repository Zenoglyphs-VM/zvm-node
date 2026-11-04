package orderbook

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// These two original buyer transactions run in separate source momentums.
// The approval is a real finite ERC-20 call, never a prefix or state insertion.
type executedTokenApproval struct{ legs [2]executedBatchLeg }

func tokenPaymentBase(profile string) string {
	switch profile {
	case "qsr-approval":
		return "qsr"
	case "mixed-approval":
		return "mixed"
	}
	return profile
}
func tokenNeedsApproval(profile string) bool {
	return profile == "qsr-approval" || profile == "mixed-approval"
}
func (f *ExecutedMarketFixture) TokenApprovalRequired() bool {
	return tokenNeedsApproval(f.r.scenario.payment)
}
func (f *ExecutedMarketFixture) TokenApprovalAmount() *big.Int {
	amount := new(big.Int)
	for i, input := range f.r.scenario.inputs {
		if f.r.scenario.providerAction == "buy" && i > 0 {
			break
		}
		if input.Order.PaymentToken == rollup.QSRTokenAddr {
			amount.Add(amount, input.Order.Price.Big())
		}
	}
	return amount
}
func (p *ExecutedMarketProviderFixture) hasProviderLegs() bool {
	return p.partial != nil || p.tokenApproval != nil
}
func (p *ExecutedMarketProviderFixture) providerLegs() *[2]executedBatchLeg {
	if (p.partial == nil) == (p.tokenApproval == nil) {
		panic("exactly one two-transaction provider profile is required")
	}
	if p.partial != nil {
		return &p.partial.legs
	}
	return &p.tokenApproval.legs
}
func (p *ExecutedMarketProviderFixture) TokenApprovalHash() common.Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokenApproval == nil {
		return common.Hash{}
	}
	return p.tokenApproval.legs[0].hash()
}
func (p *ExecutedMarketProviderFixture) tokenApprovalGas(t *testing.T) *big.Int {
	t.Helper()
	if p.tokenApproval == nil {
		t.Fatal("actual token approval profile is required")
	}
	return batchTransactionFee(t, p.batchAccountingReceipt(t, p.TokenApprovalHash()))
}
func (p *ExecutedMarketProviderFixture) sendTokenApprovalPurchase(ctx context.Context, raw hexutil.Bytes, tx *types.Transaction, from common.Address) (common.Hash, error) {
	p.mu.Lock()
	index := p.submissions
	p.mu.Unlock()
	if index < 0 || index > 1 || from != p.f.Buyer() {
		return common.Hash{}, errors.New("undeclared approval/purchase actor or transaction count")
	}
	var data []byte
	to, value := rollup.QSRTokenAddr, new(big.Int)
	matching := false
	if index == 0 {
		data = tokenPaymentCall("approve(address,uint256)", tokenPaymentAddress(p.f.Exchange()), p.f.TokenApprovalAmount())
		matching = bytes.Equal(tx.Data(), data)
	} else {
		var err error
		data, to, value, err = p.expectedActionCall()
		if err != nil {
			return common.Hash{}, err
		}
		matching = bytes.Equal(tx.Data(), data)
		if p.Action() == "batch" {
			matching = p.exactBatchCall(tx.Data())
		}
	}
	p.mu.Lock()
	leg := &p.tokenApproval.legs[index]
	if !p.nonceKnown || p.relayer == "" || tx.Type() != types.DynamicFeeTxType ||
		tx.ChainId().Cmp(p.f.Rollup().L2ChainIDBig()) != 0 || tx.Nonce() != p.initialNonce+uint64(index) || tx.Nonce() != leg.nonce ||
		tx.To() == nil || *tx.To() != to || tx.Value().Cmp(value) != 0 || !matching || len(leg.raw) != 0 || p.submissions != index ||
		(index == 1 && (!p.tokenApproval.legs[0].bound || p.tokenApproval.legs[0].mined == 0)) {
		p.mu.Unlock()
		return common.Hash{}, errors.New("provider changed the exact finite approval or original purchase sequence")
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
	if err = client.CallContext(ctx, &hash, "eth_sendRawTransaction", raw); err != nil {
		return common.Hash{}, err
	}
	if hash != tx.Hash() {
		return common.Hash{}, errors.New("actual relayer changed the approval/purchase transaction hash")
	}
	return hash, nil
}
func (p *ExecutedMarketProviderFixture) BindTokenApproval(t *testing.T, ctx context.Context) {
	t.Helper()
	if p.tokenApproval == nil {
		t.Fatal("token approval profile is required")
	}
	p.bindProviderLeg(t, ctx, 0)
}
func (p *ExecutedMarketProviderFixture) AssertTokenApproval(t *testing.T) {
	t.Helper()
	if p.tokenApproval == nil {
		t.Fatal("token approval profile is required")
	}
	p.assertProviderLeg(t, 0, true, p.f.Buyer(), p.ActorNonce()+1)
	p.mu.Lock()
	submitted := p.submissions
	second := len(p.tokenApproval.legs[1].raw)
	p.mu.Unlock()
	if submitted != 1 || second != 0 {
		t.Fatal("purchase was signed or published before the actual approval receipt gate")
	}
	if p.tokenPaymentScalar(t, "allowance(address,address)", tokenPaymentAddress(p.f.Buyer()), tokenPaymentAddress(p.f.Exchange())).Cmp(p.f.TokenApprovalAmount()) != 0 {
		t.Fatal("actual provider approval did not establish the exact finite allowance")
	}
}
func (p *ExecutedMarketProviderFixture) assertTokenApprovalCanonical(t *testing.T, included bool) {
	t.Helper()
	want := p.ActorNonce()
	if included {
		want += 2
	}
	p.assertProviderLeg(t, 0, included, p.f.Buyer(), want)
	p.assertProviderLeg(t, 1, included, p.f.Buyer(), want)
}
