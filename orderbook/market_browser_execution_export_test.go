package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/node"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// A fresh source clock stays within the production pages' five-minute listing
// allowance. The applications keep their own wall clock and random salt.
func NewExecutedMarketBrowserFixture(t *testing.T, kind string) *ExecutedMarketFixture {
	t.Helper()
	return newExecutedMarketFixtureAt(t, kind, uint64(time.Now().Add(-2*time.Minute).Unix()))
}

func (f *ExecutedMarketFixture) Store() *store.DB                   { return f.r.db }
func (f *ExecutedMarketFixture) Deriver() *node.Deriver             { return f.r.d }
func (f *ExecutedMarketFixture) ExecutionClient() *ethclient.Client { return f.r.eth }
func (f *ExecutedMarketFixture) ExecutionURL() string               { return f.r.web.URL }
func (f *ExecutedMarketFixture) SourceURL() string                  { return f.r.nom.URL }
func (f *ExecutedMarketFixture) CollectionFactory() common.Address  { return f.r.scenario.factory }

// This RPC method reads the executed state, rather than returning a declared
// browser balance. The existing loopback RPC adapter remains test authority.
func (s executionEth) GetBalance(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	header, err := s.header(selector)
	if err != nil {
		return nil, err
	}
	state, err := s.backend.chain.StateAt(header.Root)
	if err != nil {
		return nil, err
	}
	return (*hexutil.Big)(state.GetBalance(address).ToBig()), nil
}

// BindAcceptedBrowserInputs changes only unobserved future synthetic source
// calldata at the joined height-six horizon. It does not insert orders, sign
// them again, change an executed block or alter the accepted input bytes.
func (f *ExecutedMarketFixture) BindAcceptedBrowserInputs(t *testing.T, submitted []*Input) {
	t.Helper()
	f.r.nomMu.RLock()
	limit := f.r.limit
	f.r.nomMu.RUnlock()
	if f.r.d.Head().Height != 6 || limit != 6 || len(submitted) != 2 {
		t.Fatal("browser inputs must bind at the joined pre-fill source horizon")
	}
	var count int
	if err := f.DB().QueryRow("SELECT count(*) FROM nft_orders WHERE source='api'").Scan(&count); err != nil || count != 2 {
		t.Fatal("browser did not admit exactly two real API orders")
	}
	expectedKind := f.r.scenario.inputs[0].Kind
	canonical := func(input *Input) []byte {
		copy := *input
		// Root is a derived local cache field, absent from the browser input
		// and the Solidity ABI. The order/signature/proof/kind remain exact.
		copy.Root = nil
		raw, err := json.Marshal(&copy)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	accepted := make([]*Input, 2)
	for _, original := range submitted {
		if original == nil || original.Kind != expectedKind || len(original.Signature) != 65 {
			t.Fatal("unexpected actual browser signature profile")
		}
		current, status, code, err := f.Book().Get(original.Order.Hash())
		if err != nil || status != StatusOpen || code != CodeOK {
			t.Fatal("actual browser input was not accepted by the exchange-backed book")
		}
		if !bytes.Equal(canonical(original), canonical(current)) {
			t.Fatal("HTTP admission changed the actual browser order/signature/proof")
		}
		signer, ok := Recover(Domain(f.Rollup().L2ChainID, f.Exchange()), current)
		if !ok || signer != f.SigningOwner() {
			t.Fatal("accepted browser signature does not recover the fixture trader")
		}
		f.AssertContractSignatureInput(t, current)
		order := &current.Order
		if order.Collection != f.Collection() || order.Trader != f.Seller() || order.Side != SideSell || !order.TokenID.Big().IsInt64() {
			t.Fatal("browser input names an unexpected market asset")
		}
		id := order.TokenID.Big().Int64()
		if id < 1 || id > 2 || accepted[id-1] != nil {
			t.Fatal("browser inputs must name the two distinct selected tokens")
		}
		if f.TokenPaymentProfile() != "" || f.r.scenario.royalty != nil || f.r.scenario.contractSignature != nil {
			want := f.r.scenario.orders[id-1]
			if order.PaymentToken != want.PaymentToken || order.Price.Big().Cmp(want.Price.Big()) != 0 {
				t.Fatal("token listing changed its original currency or exact base units")
			}
		}
		accepted[id-1] = current
	}
	if accepted[0] == nil || accepted[1] == nil {
		t.Fatal("actual browser input set is incomplete")
	}
	if expectedKind == KindBulk && !bytes.Equal(accepted[0].Signature, accepted[1].Signature) {
		t.Fatal("bulk browser listing did not use one root signature")
	}
	orders := []*Order{&accepted[0].Order, &accepted[1].Order}
	original := f.r.scenario
	future := newExecutionScenarioInputs(t, true, false, executionScenarioInputs{
		seller: original.seller, buyer: original.buyer, provider: original.provider, action: original.providerAction, payment: original.payment, timestamp: original.cfg.L2GenesisTimestamp, orders: orders, royalty: original.royalty, contractSignature: original.contractSignature,
		sign: func(*executionScenario) []*Input { return accepted },
	})
	if future.cfg.L2GenesisHash != original.cfg.L2GenesisHash || future.exchange != original.exchange || future.token != original.token {
		t.Fatal("future browser fill changed the executed genesis or applications")
	}
	for branch := range original.branches {
		for height := uint64(1); height <= 6; height++ {
			before, err := json.Marshal(original.branches[branch][height])
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(future.branches[branch][height])
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("browser input binding changed already-derived source bytes")
			}
		}
	}
	// No source handler observes partially replaced future maps. Existing
	// handlers retain their immutable selected map from the read-lock snapshot.
	f.r.nomMu.Lock()
	original.orders, original.inputs, original.branches = future.orders, future.inputs, future.branches
	f.r.nomMu.Unlock()
}
