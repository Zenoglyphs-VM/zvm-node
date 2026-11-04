package orderbook

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// This profile retains actual production listing signatures, the node API,
// official relayer publication and executed EVM receipts. NoM authority and
// the execution RPC adapter remain explicitly declared fixture boundaries.
func NewExecutedExternalRoyaltyBrowserFixture(t *testing.T, kind string) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	if kind != "single" {
		t.Fatal("external royalty browser profile requires independent item listings")
	}
	f, p := newExecutedMarketActionRoyaltyBrowserFixture(t, kind, "batch", "", loadExternalRoyaltyCollection(t))
	p.batchBalances = &executedBatchBalances{}
	return f, p
}

func (f *ExecutedMarketFixture) ExternalRoyaltyDescription() map[string]any {
	setup := f.r.scenario.royalty
	if setup == nil {
		return nil
	}
	return map[string]any{"profile": "cap-and-revert", "price": "200000000000000018", "reportedBps": 5000,
		"capBps": 1000, "sourceSHA256": setup.sourceSHA, "compilerSHA256": setup.compilerSHA, "artifactSHA256": setup.artifactSHA,
		"implementation": crypto.CreateAddress(f.Seller(), 1).Hex(), "runtimeSHA256": creditSourceSHA(setup.runtime), "productionBrowserProviderQualified": false,
		"authenticatedNoMQualified": false, "physicalWalletOrPublicNetwork": false}
}

func externalRoyaltyExpected(input *Input) *big.Int {
	if input.Order.TokenID.Big().Cmp(big.NewInt(1)) == 0 {
		return payoutShare(input.Order.Price.Big(), 1000)
	}
	return new(big.Int)
}

func (p *ExecutedMarketProviderFixture) assertExternalRoyaltyPayouts(t *testing.T, phase string, included bool) map[string]any {
	t.Helper()
	scenario := p.f.r.scenario
	setup, fixture := scenario.payouts, scenario.royalty
	if setup == nil || fixture == nil || len(setup.before) != 4 || p.Action() != "batch" || len(scenario.inputs) != 2 {
		t.Fatal("external royalty payout prefix or batch scope differs")
	}
	ctx := context.Background()
	hash := p.f.HeadHash()
	read := func(artifact, method string, to common.Address, args ...any) ([]byte, error) {
		data, err := scenario.arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		return p.f.r.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &to, Data: data}, hash)
	}
	scalar := func(method string, args ...any) *big.Int {
		raw, err := read("ZVMExchange", method, p.f.Exchange(), args...)
		if err != nil || len(raw) != 32 {
			t.Fatal("external royalty exchange scalar differs")
		}
		return new(big.Int).SetBytes(raw)
	}
	code, err := p.f.r.eth.CodeAtHash(ctx, crypto.CreateAddress(p.f.Seller(), 1), hash)
	if err != nil || !bytes.Equal(code, fixture.runtime) {
		t.Fatal("external royalty browser implementation differs from its source-bound runtime")
	}
	if scalar("feeBps").Cmp(big.NewInt(200)) != 0 || scalar("MAX_ROYALTY_BPS").Cmp(big.NewInt(1000)) != 0 {
		t.Fatal("external royalty browser fee or cap differs")
	}
	recipient, err := read("ZVMExchange", "feeRecipient", p.f.Exchange())
	if err != nil || len(recipient) != 32 || common.BytesToAddress(recipient) != setup.feeRecipient {
		t.Fatal("external royalty fee recipient differs")
	}
	price, ok := new(big.Int).SetString("200000000000000018", 10)
	if !ok {
		t.Fatal("invalid exact external price")
	}
	for i, input := range scenario.inputs {
		if input.Order.TokenID.Big().Cmp(big.NewInt(int64(i+1))) != 0 || input.Order.PaymentToken != (common.Address{}) || input.Order.Price.Big().Cmp(price) != 0 {
			t.Fatal("accepted external royalty input lost its exact price, currency or token")
		}
	}
	reported, err := read("ZVMCollection", "royaltyInfo", p.f.Collection(), big.NewInt(1), price)
	if err != nil || len(reported) != 64 || common.BytesToAddress(reported[:32]) != setup.royaltyRecipient || new(big.Int).SetBytes(reported[32:]).Cmp(payoutShare(price, 5000)) != 0 {
		t.Fatal("actual external royalty report differs")
	}
	_, err = read("ZVMCollection", "royaltyInfo", p.f.Collection(), big.NewInt(2), price)
	if err == nil || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatal("actual external royalty call did not revert")
	}
	count := int64(0)
	gross, royalty, fee := new(big.Int), new(big.Int), new(big.Int)
	buyerFee := new(big.Int)
	if included {
		count = 2
		gross.Mul(price, big.NewInt(2))
		royalty = payoutShare(price, 1000)
		fee.Mul(payoutShare(price, 200), big.NewInt(2))
		buyerFee = batchTransactionFee(t, p.batchAccountingReceipt(t, p.TransactionHash()))
	}
	proceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(gross), royalty), fee)
	wanted := map[common.Address]*big.Int{
		p.f.Buyer():            new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(setup.before[p.f.Buyer()]), gross), buyerFee),
		p.f.Seller():           new(big.Int).Add(new(big.Int).Set(setup.before[p.f.Seller()]), proceeds),
		setup.feeRecipient:     new(big.Int).Add(new(big.Int).Set(setup.before[setup.feeRecipient]), fee),
		setup.royaltyRecipient: new(big.Int).Add(new(big.Int).Set(setup.before[setup.royaltyRecipient]), royalty),
	}
	balances := map[string]string{}
	for name, address := range map[string]common.Address{"buyer": p.f.Buyer(), "seller": p.f.Seller(), "feeRecipient": setup.feeRecipient, "royaltyRecipient": setup.royaltyRecipient} {
		actual, err := p.f.r.eth.BalanceAtHash(ctx, address, hash)
		if err != nil || actual.Cmp(wanted[address]) != 0 {
			t.Fatalf("external royalty balance differs: phase=%s role=%s actual=%v want=%v error=%v", phase, name, actual, wanted[address], err)
		}
		if scalar("credits", address).Sign() != 0 {
			t.Fatal("external royalty payout retained credit")
		}
		balances[name] = actual.String()
	}
	value := map[string]any{"pass": true, "canonicalHash": hash.Hex(), "expectedFills": count, "nativeGross": gross.String(), "nativeRoyalty": royalty.String(),
		"nativeProtocolFee": fee.String(), "nativeSellerProceeds": proceeds.String(), "buyerReceiptFee": buyerFee.String(), "sellerReceiptFee": "0",
		"balances": balances, "hashBoundContractAndBalanceReads": true, "allPayoutCreditsZero": true, "actualAboveCapReport": true,
		"actualRevertingRoyaltyCall": true, "sourceBoundExternalRuntime": true, "sourceSHA256": fixture.sourceSHA, "artifactSHA256": fixture.artifactSHA}
	setup.observations = append(setup.observations, map[string]any{"phase": phase, "state": value})
	return value
}
