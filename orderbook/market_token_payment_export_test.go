package orderbook

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Token balances come from the actual existing predeploy after a declared QSR
// source deposit. Setup allowance is finite and executed before browser buying.
type executedTokenPayment struct {
	before    map[common.Address]*big.Int
	allowance *big.Int
	codeHash  common.Hash
}

func tokenPaymentAddress(address common.Address) *big.Int {
	return new(big.Int).SetBytes(address.Bytes())
}
func tokenPaymentCall(signature string, args ...*big.Int) []byte {
	data := append([]byte(nil), crypto.Keccak256([]byte(signature))[:4]...)
	for _, arg := range args {
		data = append(data, common.LeftPadBytes(arg.Bytes(), 32)...)
	}
	return data
}
func (f *ExecutedMarketFixture) TokenPaymentProfile() string         { return f.r.scenario.payment }
func (f *ExecutedMarketFixture) TokenPaymentAddress() common.Address { return rollup.QSRTokenAddr }
func (f *ExecutedMarketFixture) TokenPaymentDescription() map[string]any {
	if f.TokenPaymentProfile() == "" {
		return nil
	}
	orders := map[string]any{}
	for _, order := range f.r.scenario.orders {
		orders[order.TokenID.Big().String()] = map[string]any{"price": order.Price.Big().String(), "paymentToken": order.PaymentToken.Hex()}
	}
	return map[string]any{"profile": f.TokenPaymentProfile(), "token": rollup.QSRTokenAddr.Hex(), "orders": orders}
}
func NewExecutedMarketTokenBrowserFixture(t *testing.T, profile string) (*ExecutedMarketFixture, *ExecutedMarketProviderFixture) {
	t.Helper()
	action := "buy"
	if profile == "mixed" {
		action = "batch"
	} else if profile != "qsr" {
		t.Fatal("unsupported token profile")
	}
	f, p := newExecutedMarketActionPaymentBrowserFixture(t, "single", action, profile)
	if action == "batch" {
		p.batchBalances = &executedBatchBalances{}
	}
	return f, p
}
func (p *ExecutedMarketProviderFixture) tokenPaymentScalar(t *testing.T, signature string, args ...*big.Int) *big.Int {
	t.Helper()
	to := rollup.QSRTokenAddr
	raw, err := p.f.r.eth.CallContractAtHash(context.Background(), ethereum.CallMsg{To: &to, Data: tokenPaymentCall(signature, args...)}, p.f.HeadHash())
	if err != nil || len(raw) != 32 {
		t.Fatalf("hash-selected QSR scalar failed: %s bytes=%d error=%v", signature, len(raw), err)
	}
	return new(big.Int).SetBytes(raw)
}
func (p *ExecutedMarketProviderFixture) pinTokenPayment(t *testing.T) {
	t.Helper()
	if p.tokenPayment != nil {
		t.Fatal("token payment prefix already pinned")
	}
	header := p.f.HeadHash()
	var code hexutil.Bytes
	selector := map[string]any{"blockHash": header, "requireCanonical": true}
	if err := p.f.r.eth.Client().CallContext(context.Background(), &code, "eth_getCode", rollup.QSRTokenAddr, selector); err != nil {
		t.Fatal(err)
	}
	genesisCode := p.f.r.scenario.genesis.Alloc[rollup.QSRTokenAddr].Code
	if len(genesisCode) == 0 || !bytes.Equal(code, genesisCode) {
		t.Fatal("QSR code differs from the pinned existing genesis predeploy")
	}
	if p.tokenPaymentScalar(t, "decimals()").Cmp(big.NewInt(8)) != 0 {
		t.Fatal("QSR decimals differ from the declared source units")
	}
	setup := p.f.r.scenario.payouts
	state := &executedTokenPayment{before: map[common.Address]*big.Int{}, codeHash: crypto.Keccak256Hash(code)}
	for _, address := range []common.Address{p.f.Buyer(), p.f.Seller(), setup.feeRecipient, setup.royaltyRecipient} {
		state.before[address] = p.tokenPaymentScalar(t, "balanceOf(address)", tokenPaymentAddress(address))
	}
	if state.before[p.f.Buyer()].Cmp(big.NewInt(100_000_000)) != 0 {
		t.Fatal("the actual QSR bridge deposit differs")
	}
	for _, address := range []common.Address{p.f.Seller(), setup.feeRecipient, setup.royaltyRecipient} {
		if state.before[address].Sign() != 0 {
			t.Fatal("QSR recipient has undeclared initial funds")
		}
	}
	state.allowance = p.tokenPaymentScalar(t, "allowance(address,address)", tokenPaymentAddress(p.f.Buyer()), tokenPaymentAddress(p.f.Exchange()))
	wanted := new(big.Int)
	for _, input := range p.f.r.scenario.inputs {
		if input.Order.PaymentToken == rollup.QSRTokenAddr {
			wanted.Add(wanted, input.Order.Price.Big())
		}
	}
	if state.allowance.Cmp(wanted) != 0 {
		t.Fatal("finite executed QSR allowance differs from the intended orders")
	}
	p.tokenPayment = state
}
func (p *ExecutedMarketProviderFixture) assertTokenPayments(t *testing.T, phase string, included bool) map[string]any {
	t.Helper()
	setup := p.f.r.scenario.payouts
	token := p.tokenPayment
	if setup == nil || token == nil || len(setup.before) != 4 {
		t.Fatal("token payment prefix is missing")
	}
	read := func(artifact, method string, to common.Address, args ...any) []byte {
		data, err := p.f.r.scenario.arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := p.f.r.eth.CallContractAtHash(context.Background(), ethereum.CallMsg{To: &to, Data: data}, p.f.HeadHash())
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	feeBps := read("ZVMExchange", "feeBps", p.f.Exchange())
	recipient := read("ZVMExchange", "feeRecipient", p.f.Exchange())
	if len(feeBps) != 32 || new(big.Int).SetBytes(feeBps).Cmp(big.NewInt(200)) != 0 || len(recipient) != 32 || common.BytesToAddress(recipient) != setup.feeRecipient {
		t.Fatal("actual token profile fee configuration differs")
	}
	nativeGross, nativeRoyalty, nativeFee := new(big.Int), new(big.Int), new(big.Int)
	tokenGross, tokenRoyalty, tokenFee := new(big.Int), new(big.Int), new(big.Int)
	fills := 0
	for i, input := range p.f.r.scenario.inputs {
		order := input.Order
		price := order.Price.Big()
		if order.PaymentToken != (common.Address{}) && order.PaymentToken != rollup.QSRTokenAddr {
			t.Fatal("undeclared payment token")
		}
		royalty := read("ZVMCollection", "royaltyInfo", p.f.Collection(), big.NewInt(int64(i+1)), price)
		if len(royalty) != 64 || common.BytesToAddress(royalty[:32]) != setup.royaltyRecipient || new(big.Int).SetBytes(royalty[32:]).Cmp(payoutShare(price, 375)) != 0 {
			t.Fatal("per-currency executed royalty differs")
		}
		if !included || (p.Action() == "buy" && i > 0) {
			continue
		}
		fills++
		gross, r, f := nativeGross, nativeRoyalty, nativeFee
		if order.PaymentToken == rollup.QSRTokenAddr {
			gross, r, f = tokenGross, tokenRoyalty, tokenFee
		}
		gross.Add(gross, price)
		r.Add(r, payoutShare(price, 375))
		f.Add(f, payoutShare(price, 200))
	}
	nativeProceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(nativeGross), nativeRoyalty), nativeFee)
	tokenProceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(tokenGross), tokenRoyalty), tokenFee)
	gas := new(big.Int)
	if included {
		gas = batchTransactionFee(t, p.batchAccountingReceipt(t, p.TransactionHash()))
	}
	nativeWanted := map[common.Address]*big.Int{
		p.f.Buyer():            new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(setup.before[p.f.Buyer()]), nativeGross), gas),
		p.f.Seller():           new(big.Int).Add(new(big.Int).Set(setup.before[p.f.Seller()]), nativeProceeds),
		setup.feeRecipient:     new(big.Int).Add(new(big.Int).Set(setup.before[setup.feeRecipient]), nativeFee),
		setup.royaltyRecipient: new(big.Int).Add(new(big.Int).Set(setup.before[setup.royaltyRecipient]), nativeRoyalty)}
	tokenWanted := map[common.Address]*big.Int{
		p.f.Buyer():            new(big.Int).Sub(new(big.Int).Set(token.before[p.f.Buyer()]), tokenGross),
		p.f.Seller():           new(big.Int).Add(new(big.Int).Set(token.before[p.f.Seller()]), tokenProceeds),
		setup.feeRecipient:     new(big.Int).Add(new(big.Int).Set(token.before[setup.feeRecipient]), tokenFee),
		setup.royaltyRecipient: new(big.Int).Add(new(big.Int).Set(token.before[setup.royaltyRecipient]), tokenRoyalty)}
	nativeBalances, tokenBalances := map[string]string{}, map[string]string{}
	for name, address := range map[string]common.Address{"buyer": p.f.Buyer(), "seller": p.f.Seller(), "feeRecipient": setup.feeRecipient, "royaltyRecipient": setup.royaltyRecipient} {
		got, err := p.f.r.eth.BalanceAtHash(context.Background(), address, p.f.HeadHash())
		if err != nil || got.Cmp(nativeWanted[address]) != 0 {
			t.Fatalf("native/token separation differs for %s: error=%v", name, err)
		}
		qsr := p.tokenPaymentScalar(t, "balanceOf(address)", tokenPaymentAddress(address))
		if qsr.Cmp(tokenWanted[address]) != 0 {
			t.Fatalf("QSR payout differs for %s: got=%s want=%s", name, qsr, tokenWanted[address])
		}
		credit := read("ZVMExchange", "credits", p.f.Exchange(), address)
		if len(credit) != 32 || new(big.Int).SetBytes(credit).Sign() != 0 {
			t.Fatal("token profile has undeclared native credit")
		}
		nativeBalances[name], tokenBalances[name] = got.String(), qsr.String()
	}
	allowance := p.tokenPaymentScalar(t, "allowance(address,address)", tokenPaymentAddress(p.f.Buyer()), tokenPaymentAddress(p.f.Exchange()))
	if allowance.Cmp(new(big.Int).Sub(new(big.Int).Set(token.allowance), tokenGross)) != 0 {
		t.Fatal("canonical QSR allowance differs from exact transfers")
	}
	value := map[string]any{"pass": true, "canonicalHash": p.f.HeadHash().Hex(), "expectedFills": fills,
		"nativeGross": nativeGross.String(), "nativeRoyalty": nativeRoyalty.String(), "nativeProtocolFee": nativeFee.String(), "nativeSellerProceeds": nativeProceeds.String(),
		"qsrGross": tokenGross.String(), "qsrRoyalty": tokenRoyalty.String(), "qsrProtocolFee": tokenFee.String(), "qsrSellerProceeds": tokenProceeds.String(),
		"buyerReceiptFee": gas.String(), "sellerReceiptFee": "0", "balances": nativeBalances, "qsrBalances": tokenBalances,
		"qsrAllowance": allowance.String(), "qsrAllowanceBefore": token.allowance.String(), "qsrCodeHash": token.codeHash.Hex(), "qsrDecimals": 8,
		"hashBoundContractAndBalanceReads": true, "allPayoutCreditsZero": true}
	setup.observations = append(setup.observations, map[string]any{"phase": phase, "state": value})
	return value
}
func (p *ExecutedMarketProviderFixture) tokenPaymentEvidence(t *testing.T) map[string]any {
	t.Helper()
	setup := p.f.r.scenario.payouts
	if setup == nil || p.tokenPayment == nil || len(setup.observations) != 5 {
		t.Fatal("token payment evidence lacks canonical phases")
	}
	return map[string]any{"pass": true, "configuration": p.f.MarketPayoutConfig(), "tokenPayment": p.f.TokenPaymentDescription(),
		"phases": setup.observations, "distinctRecipients": true, "nativeOnly": false, "QSRPredeployMatchesExecutedGenesis": true,
		"allowanceWasExecutedBeforeBuying": true, "missingAllowanceApprovalPathQualified": false, "directStateOrStorageInsertion": 0, "realWalletOrPublicNetwork": false}
}
