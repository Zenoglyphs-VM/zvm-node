package orderbook_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
)

func executedBatchMarketProjection(t *testing.T, f *orderbook.ExecutedMarketFixture, p *orderbook.ExecutedMarketProviderFixture,
	base string, inputs []*orderbook.Input, stage int) map[string]any {
	t.Helper()
	executed := p.AssertBatchState(t, stage)
	states := executed["states"].([]map[string]any)
	collection := f.Collection().Hex()
	wantListings, wantSales := []string{}, []string{}
	prices := map[string]string{}
	currencies := map[string]common.Address{}
	for _, input := range inputs {
		prices[input.Order.Hash().Hex()] = input.Order.Price.Big().String()
		prices[input.Order.TokenID.Big().String()] = input.Order.Price.Big().String()
		currencies[input.Order.Hash().Hex()] = input.Order.PaymentToken
		currencies[input.Order.TokenID.Big().String()] = input.Order.PaymentToken
	}
	for i, input := range inputs {
		state := states[i]
		var detail struct {
			Status   string
			Code     uint8
			Fillable bool
		}
		executedMarketGET(t, base, "/api/nft/orders/"+input.Order.Hash().Hex(), &detail)
		if detail.Status != state["status"].(string) || detail.Code != state["code"].(uint8) ||
			detail.Fillable != state["fillable"].(bool) {
			t.Fatal("batch HTTP order differs from independent executed state")
		}
		var item struct {
			Owner    common.Address
			Listings []struct{ Hash common.Hash }
		}
		executedMarketGET(t, base, "/api/nft/items/"+collection+"/"+input.Order.TokenID.Big().String(), &item)
		count := 0
		if detail.Fillable {
			count = 1
			wantListings = append(wantListings, input.Order.Hash().Hex())
		}
		if item.Owner.Hex() != state["owner"].(string) || len(item.Listings) != count ||
			(count == 1 && item.Listings[0].Hash != input.Order.Hash()) {
			t.Fatal("batch HTTP ownership or listing differs from execution")
		}
		if state["filled"].(int64) == 1 {
			wantSales = append(wantSales, input.Order.TokenID.Big().String())
		}
	}
	var listings []struct {
		Hash     common.Hash
		Price    string
		Currency common.Address
	}
	executedMarketGET(t, base, "/api/nft/listings?collection="+collection, &listings)
	gotListings := []string{}
	for _, row := range listings {
		if row.Price != prices[row.Hash.Hex()] || row.Currency != currencies[row.Hash.Hex()] {
			t.Fatal("batch listing lost its exact integer price")
		}
		gotListings = append(gotListings, row.Hash.Hex())
	}
	sort.Strings(wantListings)
	sort.Strings(gotListings)
	if !reflect.DeepEqual(gotListings, wantListings) {
		t.Fatal("batch collection listings disagree with actual fills and cancellation")
	}
	var sales []map[string]json.RawMessage
	executedMarketGET(t, base, "/api/nft/sales?collection="+collection, &sales)
	gotSales := []string{}
	for _, row := range sales {
		var id, price string
		var currency common.Address
		if json.Unmarshal(row["tokenId"], &id) != nil || json.Unmarshal(row["price"], &price) != nil ||
			price != prices[id] || json.Unmarshal(row["currency"], &currency) != nil || currency != currencies[id] {
			t.Fatal("batch sale lost its exact token or price")
		}
		gotSales = append(gotSales, id)
	}
	sort.Strings(wantSales)
	sort.Strings(gotSales)
	if !reflect.DeepEqual(gotSales, wantSales) {
		t.Fatal("batch sale history retained an orphan or lost an actual fill")
	}
	executedMarketIntents(t, f, base, inputs)
	return map[string]any{"executed": executed, "listings": gotListings, "sales": sales, "originalInputsPreserved": true}
}
