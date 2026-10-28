package orderbook

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// Separate public fixture recipients make a royalty transfer distinguishable
// from seller proceeds. All values come from executed calls and balances.
type executedMarketPayouts struct {
	feeRecipient, royaltyRecipient common.Address
	feeBps, royaltyBps             uint64
	before                         map[common.Address]*big.Int
	observations                   []map[string]any
}

func newExecutedMarketPayouts() *executedMarketPayouts {
	return &executedMarketPayouts{feeRecipient: common.Address{18: 0xf0, 19: 1},
		royaltyRecipient: common.Address{18: 0xf0, 19: 2}, feeBps: 200, royaltyBps: 375}
}

func (f *ExecutedMarketFixture) MarketPayoutConfig() map[string]any {
	p := f.r.scenario.payouts
	if p == nil {
		return nil
	}
	return map[string]any{"feeRecipient": p.feeRecipient.Hex(), "royaltyRecipient": p.royaltyRecipient.Hex(),
		"feeBps": p.feeBps, "royaltyBps": p.royaltyBps}
}

func (p *ExecutedMarketProviderFixture) PinMarketPayouts(t *testing.T) {
	t.Helper()
	setup := p.f.r.scenario.payouts
	if setup == nil {
		return
	}
	if setup.before != nil {
		t.Fatal("payout balances were already pinned")
	}
	setup.before = map[common.Address]*big.Int{}
	for _, address := range []common.Address{p.f.Buyer(), p.f.Seller(), setup.feeRecipient, setup.royaltyRecipient} {
		if _, exists := setup.before[address]; exists || address == (common.Address{}) {
			t.Fatal("payout actors must be nonzero and independently observable")
		}
		balance, err := p.f.r.eth.BalanceAtHash(context.Background(), address, p.f.HeadHash())
		if err != nil {
			t.Fatal(err)
		}
		setup.before[address] = new(big.Int).Set(balance)
	}
	if setup.before[setup.feeRecipient].Sign() != 0 || setup.before[setup.royaltyRecipient].Sign() != 0 {
		t.Fatal("payout recipients have undeclared initial funds")
	}
}

func payoutShare(amount *big.Int, bps uint64) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(amount, new(big.Int).SetUint64(bps)), big.NewInt(10_000))
}

func (p *ExecutedMarketProviderFixture) AssertMarketPayouts(t *testing.T, phase string, included bool) map[string]any {
	t.Helper()
	setup := p.f.r.scenario.payouts
	if setup == nil {
		return nil
	}
	if len(setup.before) != 4 {
		t.Fatal("canonical payout prefix was not pinned")
	}
	header := p.f.HeadHash()
	read := func(artifact, method string, address common.Address, args ...any) []byte {
		data, err := p.f.r.scenario.arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		value, err := p.f.r.eth.CallContractAtHash(context.Background(), ethereum.CallMsg{To: &address, Data: data}, header)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	feeRecipient := read("ZVMExchange", "feeRecipient", p.f.Exchange())
	feeBps := read("ZVMExchange", "feeBps", p.f.Exchange())
	if len(feeRecipient) != 32 || common.BytesToAddress(feeRecipient) != setup.feeRecipient ||
		len(feeBps) != 32 || new(big.Int).SetBytes(feeBps).Cmp(new(big.Int).SetUint64(setup.feeBps)) != 0 {
		t.Fatal("executed protocol fee configuration differs")
	}
	price := big.NewInt(250_000_000_000_000_000)
	for i, input := range p.f.r.scenario.inputs {
		if input.Order.PaymentToken != (common.Address{}) || input.Order.Price.Big().Cmp(price) != 0 {
			t.Fatal("native payout profile changed the original exact price or currency")
		}
		royalty := read("ZVMCollection", "royaltyInfo", p.f.Collection(), big.NewInt(int64(i+1)), price)
		if len(royalty) != 64 || common.BytesToAddress(royalty[:32]) != setup.royaltyRecipient ||
			new(big.Int).SetBytes(royalty[32:]).Cmp(payoutShare(price, setup.royaltyBps)) != 0 {
			t.Fatal("executed per-token royalty receiver or amount differs")
		}
	}
	count := int64(0)
	if included {
		count = 1
		if p.Action() == "batch" {
			count = 2
		}
	}
	gross := new(big.Int).Mul(price, big.NewInt(count))
	royalty := new(big.Int).Mul(payoutShare(price, setup.royaltyBps), big.NewInt(count))
	fee := new(big.Int).Mul(payoutShare(price, setup.feeBps), big.NewInt(count))
	proceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(gross), royalty), fee)
	buyerGas, sellerGas := new(big.Int), new(big.Int)
	if included {
		buyerGas = batchTransactionFee(t, p.batchAccountingReceipt(t, p.TransactionHash()))
	}
	if p.partial != nil && (included || phase == "cancel-ordered" || phase == "cancel-mined") {
		sellerGas = batchTransactionFee(t, p.batchAccountingReceipt(t, p.BatchCancellationHash()))
	}
	wanted := map[common.Address]*big.Int{
		p.f.Buyer():            new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(setup.before[p.f.Buyer()]), gross), buyerGas),
		p.f.Seller():           new(big.Int).Sub(new(big.Int).Add(new(big.Int).Set(setup.before[p.f.Seller()]), proceeds), sellerGas),
		setup.feeRecipient:     new(big.Int).Add(new(big.Int).Set(setup.before[setup.feeRecipient]), fee),
		setup.royaltyRecipient: new(big.Int).Add(new(big.Int).Set(setup.before[setup.royaltyRecipient]), royalty),
	}
	balances := map[string]string{}
	for name, address := range map[string]common.Address{"buyer": p.f.Buyer(), "seller": p.f.Seller(),
		"feeRecipient": setup.feeRecipient, "royaltyRecipient": setup.royaltyRecipient} {
		actual, err := p.f.r.eth.BalanceAtHash(context.Background(), address, header)
		if err != nil || actual.Cmp(wanted[address]) != 0 {
			t.Fatalf("canonical %s payout balance differs: got=%v want=%v error=%v", name, actual, wanted[address], err)
		}
		credit := read("ZVMExchange", "credits", p.f.Exchange(), address)
		if len(credit) != 32 || new(big.Int).SetBytes(credit).Sign() != 0 {
			t.Fatal("an independently payable EOA unexpectedly retained exchange credit")
		}
		balances[name] = actual.String()
	}
	value := map[string]any{"pass": true, "canonicalHash": header.Hex(), "expectedFills": count,
		"nativeGross": gross.String(), "nativeRoyalty": royalty.String(), "nativeProtocolFee": fee.String(),
		"nativeSellerProceeds": proceeds.String(), "buyerReceiptFee": buyerGas.String(), "sellerReceiptFee": sellerGas.String(),
		"balances": balances, "hashBoundContractAndBalanceReads": true, "allPayoutCreditsZero": true}
	setup.observations = append(setup.observations, map[string]any{"phase": phase, "state": value})
	return value
}

func (p *ExecutedMarketProviderFixture) MarketPayoutEvidence(t *testing.T) map[string]any {
	t.Helper()
	setup := p.f.r.scenario.payouts
	if setup == nil {
		return nil
	}
	want := 5
	if p.partial != nil {
		want = 7
	}
	if len(setup.observations) != want {
		t.Fatal("nonzero payout evidence lacks a canonical phase")
	}
	return map[string]any{"pass": true, "configuration": p.f.MarketPayoutConfig(),
		"phases": setup.observations, "distinctRecipients": true, "nativeOnly": true,
		"directStateOrStorageInsertion": 0, "realWalletOrPublicNetwork": false}
}
