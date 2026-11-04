package orderbook_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
)

func executedOwnerMarketProjection(t *testing.T, f *orderbook.ExecutedMarketFixture, p *orderbook.ExecutedMarketProviderFixture,
	base string, inputs []*orderbook.Input, included bool) map[string]any {
	t.Helper()
	executed := p.AssertOwnerAction(t, included)
	states := executed["states"].([]map[string]any)
	collection := f.Collection().Hex()
	want := []string{}
	for i, input := range inputs {
		var detail struct {
			Status   string `json:"status"`
			Code     uint8  `json:"code"`
			Fillable bool   `json:"fillable"`
		}
		executedMarketGET(t, base, "/api/nft/orders/"+input.Order.Hash().Hex(), &detail)
		if detail.Status != states[i]["status"].(string) || detail.Code != states[i]["code"].(uint8) || detail.Fillable != states[i]["fillable"].(bool) {
			t.Fatal("owner order HTTP view differs from actual contract state")
		}
		var item struct {
			Owner    common.Address `json:"owner"`
			Listings []struct {
				Hash common.Hash `json:"hash"`
			} `json:"listings"`
		}
		executedMarketGET(t, base, "/api/nft/items/"+collection+"/"+input.Order.TokenID.Big().String(), &item)
		count := 0
		if detail.Fillable {
			count = 1
			want = append(want, input.Order.Hash().Hex())
		}
		if item.Owner != f.Seller() || len(item.Listings) != count || (count == 1 && item.Listings[0].Hash != input.Order.Hash()) {
			t.Fatal("owner invalidation changed ownership or left a stale listing")
		}
	}
	var listings []struct {
		Hash common.Hash `json:"hash"`
	}
	executedMarketGET(t, base, "/api/nft/listings?collection="+collection, &listings)
	got := []string{}
	for _, row := range listings {
		got = append(got, row.Hash.Hex())
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("collection listings retained an invalid owner order")
	}
	var sales []any
	executedMarketGET(t, base, "/api/nft/sales?collection="+collection, &sales)
	if len(sales) != 0 {
		t.Fatal("owner invalidation created a sale")
	}
	executedMarketIntents(t, f, base, inputs)
	return map[string]any{"executed": executed, "listings": got, "sales": 0, "originalInputsPreserved": true}
}
