package orderbook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
)

func executedMarketGET(t *testing.T, base, path string, out any) {
	t.Helper()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || response.StatusCode != http.StatusOK {
		t.Fatalf("bounded market read %s: status=%d error=%v", path, response.StatusCode, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func executedMarketIntents(t *testing.T, f *orderbook.ExecutedMarketFixture, base string, inputs []*orderbook.Input) {
	t.Helper()
	for _, input := range inputs {
		expected, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := f.DB().QueryRow("SELECT input FROM nft_orders WHERE hash=?", input.Order.Hash().Hex()).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != string(expected) {
			t.Fatal("original signed API intent bytes changed")
		}
		var row struct {
			Hash  common.Hash     `json:"hash"`
			Input json.RawMessage `json:"input"`
		}
		executedMarketGET(t, base, "/api/nft/orders/"+input.Order.Hash().Hex(), &row)
		var compact bytes.Buffer
		if err := json.Compact(&compact, row.Input); err != nil {
			t.Fatal(err)
		}
		if row.Hash != input.Order.Hash() || !bytes.Equal(compact.Bytes(), expected) {
			t.Fatal("HTTP order did not retain original signature, proof and hash")
		}
	}
}

func executedMarketProjection(t *testing.T, f *orderbook.ExecutedMarketFixture, base string, inputs []*orderbook.Input, filled int) map[string]any {
	t.Helper()
	collection := f.Collection().Hex()
	var listings []struct {
		Hash  common.Hash `json:"hash"`
		Price string      `json:"price"`
	}
	executedMarketGET(t, base, "/api/nft/listings?collection="+collection, &listings)
	var want []string
	for i, input := range inputs {
		if i != filled {
			want = append(want, input.Order.Hash().Hex())
		}
	}
	var got []string
	for _, listing := range listings {
		if listing.Price != inputs[0].Order.Price.Big().String() {
			t.Fatal("market listing price lost its exact integer representation")
		}
		got = append(got, listing.Hash.Hex())
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("market listings=%v want=%v", got, want)
	}
	owners := make([]common.Address, 2)
	for i := range inputs {
		var item struct {
			Owner    common.Address `json:"owner"`
			Listings []struct {
				Hash common.Hash `json:"hash"`
			} `json:"listings"`
		}
		executedMarketGET(t, base, "/api/nft/items/"+collection+"/"+inputs[i].Order.TokenID.Big().String(), &item)
		owner, count := f.Seller(), 1
		if i == filled {
			owner, count = f.Buyer(), 0
		}
		if item.Owner != owner || f.ExecutedOwner(t, int64(i+1)) != owner || len(item.Listings) != count {
			t.Fatal("market item owner/listing differs from executed state")
		}
		if count == 1 && item.Listings[0].Hash != inputs[i].Order.Hash() {
			t.Fatal("market item shows another order")
		}
		owners[i] = owner
		var detail struct {
			Status   string `json:"status"`
			Code     uint8  `json:"code"`
			Fillable bool   `json:"fillable"`
		}
		executedMarketGET(t, base, "/api/nft/orders/"+inputs[i].Order.Hash().Hex(), &detail)
		if i == filled {
			if detail.Status != orderbook.StatusFilled || detail.Code != orderbook.CodeFilled || detail.Fillable {
				t.Fatal("filled order is still offered by the HTTP book")
			}
		} else if detail.Status != orderbook.StatusOpen || detail.Code != orderbook.CodeOK || !detail.Fillable {
			t.Fatal("canonical unfilled order is unavailable through HTTP")
		}
	}
	var sales []map[string]json.RawMessage
	executedMarketGET(t, base, "/api/nft/sales?collection="+collection, &sales)
	expectedSales := 0
	if filled >= 0 {
		expectedSales = 1
	}
	if len(sales) != expectedSales {
		t.Fatal("sale history retained an orphan or lost a canonical fill")
	}
	if filled >= 0 {
		var id string
		if err := json.Unmarshal(sales[0]["tokenId"], &id); err != nil || id != inputs[filled].Order.TokenID.Big().String() {
			t.Fatal("sale history names the orphaned token")
		}
	}
	f.AssertCurrentChecks(t, filled)
	executedMarketIntents(t, f, base, inputs)
	return map[string]any{"owners": owners, "listings": got, "sales": sales}
}

// Each profile admits real off-chain signatures over HTTP before any order
// approval log exists. Native transactions then fill the exact signed input.
// Source replacement is detected and reconciled by the production workers;
// this test never writes an order row or calls IndexBlock, Rewind or Book.step.
func TestExecutedMarketAdmissionAndCanonicalRecovery(t *testing.T) {
	for _, kind := range []string{"single", "bulk"} {
		t.Run(kind, func(t *testing.T) {
			f := orderbook.NewExecutedMarketFixture(t, kind)
			deployment := filepath.Join(t.TempDir(), "apps.json")
			if err := (&apps.Deployment{ChainID: f.Rollup().L2ChainID, Exchange: f.Exchange()}).Save(deployment); err != nil {
				t.Fatal(err)
			}
			service := market.New(market.Config{DB: f.DB(), Rollup: f.Rollup(), Orders: f.Book(),
				Apps: &apps.File{Path: deployment, ChainID: f.Rollup().L2ChainID}})
			mux := http.NewServeMux()
			service.Register(mux)
			web := httptest.NewServer(mux)
			defer web.Close()
			checks := []string{}
			f.Advance(0, 6)
			var count int
			if err := f.DB().QueryRow("SELECT count(*) FROM nft_orders").Scan(&count); err != nil || count != 0 {
				t.Fatal("fixture supplied an order instead of admitting it")
			}
			var exchange struct {
				DomainSeparator common.Hash `json:"domainSeparator"`
			}
			executedMarketGET(t, web.URL, "/api/nft/exchange", &exchange)
			if exchange.DomainSeparator != orderbook.Domain(f.Rollup().L2ChainID, f.Exchange()) {
				t.Fatal("HTTP signing domain differs from the book")
			}
			checks = append(checks, "empty book and actual HTTP signing domain")

			inputs := f.Inputs(t)
			before, _, _ := f.RPCCounts()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results, err := orderbook.Post(ctx, web.URL, inputs)
			if err != nil || len(results) != 2 {
				t.Fatalf("signed HTTP admission failed: results=%d error=%v", len(results), err)
			}
			for i, result := range results {
				if !result.OK || result.Code != orderbook.CodeOK || result.Hash != inputs[i].Order.Hash() {
					t.Fatal("actual exchange refused the expected signed input")
				}
			}
			after, failures, _ := f.RPCCounts()
			if after <= before || failures != 0 {
				t.Fatal("HTTP admission did not use successful block-hash exchange RPC")
			}
			executedMarketProjection(t, f, web.URL, inputs, -1)
			checks = append(checks, "signed HTTP admission executes hash-bound Multicall3/checkOrder", "exact signature proof hash price and ownership reach HTTP")

			order := inputs[0].Order
			order.Salt = orderbook.U(101)
			wrongDomain := f.Sign(t, &order, f.Rollup().L2ChainID+1)
			order.Salt, order.ListingTime, order.ExpirationTime = orderbook.U(102), 0, f.Rollup().L2GenesisTimestamp+1
			expired := f.Sign(t, &order, f.Rollup().L2ChainID)
			order = inputs[0].Order
			order.Salt, order.TokenID = orderbook.U(103), orderbook.U(999)
			notHeld := f.Sign(t, &order, f.Rollup().L2ChainID)
			corrupt := f.Inputs(t)[0]
			corrupt.Signature[0] ^= 0x80
			for _, negative := range []struct {
				name     string
				input    *orderbook.Input
				code     uint8
				mustCall bool
			}{
				{"wrong chain signing domain refused", wrongDomain, orderbook.CodeBadSignature, false},
				{"expired signed order refused by exchange", expired, orderbook.CodeExpired, true},
				{"unowned signed token refused by exchange", notHeld, orderbook.CodeNotHeld, true},
				{"rejected replacement preserves accepted intent", corrupt, orderbook.CodeBadSignature, false},
			} {
				prior, _, _ := f.RPCCounts()
				result, err := orderbook.Post(ctx, web.URL, []*orderbook.Input{negative.input})
				if err == nil || len(result) != 1 || result[0].OK || result[0].Code != negative.code {
					t.Fatalf("negative control %s: error=%v result=%v", negative.name, err, result)
				}
				next, failed, _ := f.RPCCounts()
				if failed != 0 || (negative.mustCall && next <= prior) {
					t.Fatal("exchange refusal was not an executed RPC decision")
				}
				checks = append(checks, negative.name)
			}
			if err := f.DB().QueryRow("SELECT count(*) FROM nft_orders").Scan(&count); err != nil || count != 2 {
				t.Fatal("rejected HTTP submission changed book cardinality")
			}
			executedMarketIntents(t, f, web.URL, inputs)
			checks = append(checks, "refused submissions retain exactly two original API intents")

			f.Advance(0, 7, 8, 9)
			f.AssertNoOrderApprovalLogs(t)
			// With no on-chain order approval, both admission and the buy's
			// authorization depend on the submitted single or bulk signature.
			reference := executedMarketProjection(t, f, web.URL, inputs, 0)
			oldHash, oldEpoch := f.HeadHash(), f.Epoch(t)
			checks = append(checks, "production workers follow executed signed fill", "filled order disappears and canonical sale appears")

			f.Advance(1, 8, 9)
			if f.HeadHash() == oldHash || f.Epoch(t) <= oldEpoch {
				t.Fatal("quiet source replacement did not select a new executed head and rewind epoch")
			}
			executedMarketProjection(t, f, web.URL, inputs, 1)
			checks = append(checks, "quiet same-height source replacement triggers automatic recovery", "orphaned fill reopens with a fresh real exchange check", "replacement owner sale and listing agree through HTTP")

			replacementEpoch := f.Epoch(t)
			f.Advance(0, 8, 9)
			if f.HeadHash() != oldHash || f.Epoch(t) <= replacementEpoch {
				t.Fatal("returning source branch did not recover its executed head")
			}
			restored := executedMarketProjection(t, f, web.URL, inputs, 0)
			if !reflect.DeepEqual(restored, reference) {
				t.Fatal("restored HTTP read model differs from the same executed canonical branch")
			}
			calls, failures, hashes := f.RPCCounts()
			if calls < 3 || failures != 0 || hashes < 3 {
				t.Fatalf("canonical checker accounting: successful=%d failures=%d headers=%d joined_cancelled=%d", calls, failures, hashes, f.RPCCancellations())
			}
			checks = append(checks, "returning branch converges to original HTTP state", "every original signature and proof survives both replacements", "all owned production workers joined")
			record := map[string]any{"profile": kind, "checks": checks, "checkCount": len(checks),
				"hashBoundExchangeCalls": calls, "exchangeRPCFailures": failures, "distinctCheckHeaders": hashes,
				"cancelledJoinedExchangeCalls": f.RPCCancellations(), "totalExchangeCallAttempts": calls + failures + f.RPCCancellations(),
				"signedInputs": 2, "SQLOrderInsertsByTest": 0, "manualIndexOrBookSteps": 0,
				"realWalletOrPublicNetworkOperations": 0, "unsignedNoMObservationAdapter": true,
				"customLoopbackEngineRPCAdapter": true, "workersJoined": true, "pass": true}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("EXECUTED-MARKET-QUALIFICATION " + string(raw))
		})
	}
}
