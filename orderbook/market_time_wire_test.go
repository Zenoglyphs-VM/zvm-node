package orderbook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The actual application module, its own locked Viem, real EIP-712 signatures,
// node HTTP codec, persisted book and hash-bound Solidity check all participate.
// Public fixture keys, synthetic NoM and the custom Engine RPC retain their
// declared scope. No Chrome, wallet transaction or public network is involved.
func TestMarketFullWidthTimeClients(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit pinned frontend SDK CI lane")
	}
	for _, app := range []string{"nft", "karum"} {
		for _, kind := range []string{"single", "bulk"} {
			t.Run(app+"-"+kind, func(t *testing.T) {
				f := orderbook.NewExecutedMarketBrowserFixture(t, kind)
				f.Advance(0, 6)
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
				base := f.Inputs(t)[0].Order
				values := []uint64{1<<53 - 1, 1<<53 + 1, math.MaxInt64 + 1, math.MaxUint64}
				orders := make([]*orderbook.Order, len(values))
				wire := make([]any, len(values))
				for i, value := range values {
					o := base
					o.TokenID = orderbook.U(int64(i%2 + 1))
					o.Salt = orderbook.U(int64(1000 + i))
					o.ListingTime = f.Rollup().L2GenesisTimestamp
					o.ExpirationTime = value
					orders[i] = &o
					wire[i] = orderbook.ToWireInput(&orderbook.Input{Order: o}).Order
				}
				keyBytes := make([]byte, 32)
				keyBytes[31] = 1 // Deliberately public fixture signer.
				key, err := crypto.ToECDSA(keyBytes)
				if err != nil {
					t.Fatal(err)
				}
				if crypto.PubkeyToAddress(key.PublicKey) != f.Seller() {
					t.Fatal("time-wire fixture signer changed")
				}
				domain := orderbook.Domain(f.Rollup().L2ChainID, f.Exchange())
				var expected []*orderbook.Input
				if kind == "bulk" {
					expected, err = orderbook.SignBulk(key, domain, orders)
				} else {
					for _, o := range orders {
						expected = append(expected, f.Sign(t, o, f.Rollup().L2ChainID))
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				input, err := json.Marshal(map[string]any{"app": app, "kind": kind, "base": web.URL, "orders": wire,
					"chainId": f.Rollup().L2ChainID, "exchange": f.Exchange(), "seller": f.Seller(),
					"root": os.Getenv("ZVM_BROWSER_" + strings.ToUpper(app) + "_ROOT")})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "node", "--experimental-vm-modules", filepath.Join("..", "scripts", "market-time-wire.mjs"))
				cmd.Env = append(os.Environ(), "NODE_DISABLE_COMPILE_CACHE=1")
				cmd.Stdin = bytes.NewReader(input)
				stdout, stderr := &executedBrowserOutput{limit: 1 << 20}, &executedBrowserOutput{limit: 128 << 10}
				cmd.Stdout, cmd.Stderr = stdout, stderr
				cmd.WaitDelay = 3 * time.Second
				if err := cmd.Run(); err != nil {
					t.Fatalf("actual SDK/API time-wire client failed: %v; stderr=%s", err, stderr.raw.String())
				}
				line := strings.TrimSpace(stdout.raw.String())
				const marker = "MARKET-TIME-WIRE-CLIENT "
				if !strings.HasPrefix(line, marker) || strings.Contains(line, "\n") {
					t.Fatal("unexpected time-wire client output")
				}
				var record struct {
					App         string                 `json:"app"`
					Kind        string                 `json:"kind"`
					Pass        bool                   `json:"pass"`
					Hashes      []common.Hash          `json:"hashes"`
					Digests     []common.Hash          `json:"digests"`
					Inputs      []*orderbook.WireInput `json:"signedInputs"`
					Future      *orderbook.WireInput   `json:"futureInput"`
					PromptCount int                    `json:"actualTypedDataSignatures"`
					SDKVersion  string                 `json:"SDKVersion"`
					Reads       int                    `json:"successfulRealOrderReads"`
					FutureCode  int                    `json:"futureOrderCode"`
					BadStatus   int                    `json:"unsafeNumericHTTPStatus"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &record); err != nil {
					t.Fatal(err)
				}
				wantPrompts := 5
				if kind == "bulk" {
					wantPrompts = 2
				}
				if record.Future == nil || !record.Pass || record.App != app || record.Kind != kind || record.SDKVersion != "2.56.8" || record.PromptCount != wantPrompts ||
					len(record.Inputs) != 4 || len(record.Hashes) != 4 || len(record.Digests) != wantPrompts || record.Reads != 4 ||
					record.FutureCode != int(orderbook.CodeNotStarted) || record.BadStatus != 400 {
					t.Fatal("time-wire runtime evidence is incomplete")
				}
				for i, item := range record.Inputs {
					actual := item.Input()
					reference := expected[i]
					if actual.Order.Hash() != reference.Order.Hash() || record.Hashes[i] != reference.Order.Hash() ||
						actual.Kind != reference.Kind || !bytes.Equal(actual.Signature, reference.Signature) || !reflect.DeepEqual(actual.Proof, reference.Proof) {
						t.Fatal("actual TypeScript signature/proof/hash differs from original Go full-width input")
					}
					wantDigest := orderbook.OrderDigest(domain, reference.Order.Hash())
					prompt := i
					if kind == "bulk" {
						wantDigest = orderbook.RootDigest(domain, *reference.Root)
						prompt = 0
					}
					if record.Digests[prompt] != wantDigest {
						t.Fatal("TypeScript and Go full-width EIP-712 digest differ")
					}
					stored, status, code, err := f.Book().Get(reference.Order.Hash())
					if err != nil || status != orderbook.StatusOpen || code != orderbook.CodeOK {
						t.Fatal("actual exchange did not admit the exact full-width input")
					}
					original, _ := json.Marshal(reference)
					persisted, _ := json.Marshal(stored)
					if !bytes.Equal(original, persisted) {
						t.Fatal("full-width persisted signature/proof/root changed")
					}
					var row struct {
						Input *orderbook.WireInput `json:"input"`
					}
					executedMarketGET(t, web.URL, "/api/nft/orders/"+reference.Order.Hash().Hex(), &row)
					if row.Input == nil || row.Input.Input().Order.Hash() != reference.Order.Hash() {
						t.Fatal("HTTP response rounded a full-width order")
					}
				}
				future := record.Future.Input()
				if future.Order.ListingTime != 1<<53+1 || future.Order.ExpirationTime != math.MaxUint64 ||
					record.Digests[len(record.Digests)-1] != orderbook.OrderDigest(domain, future.Order.Hash()) {
					t.Fatal("future uint64 listing was not preserved")
				}
				var listings []struct {
					Hash   common.Hash     `json:"hash"`
					Expiry json.RawMessage `json:"expiry"`
				}
				executedMarketGET(t, web.URL, "/api/nft/listings?collection="+f.Collection().Hex(), &listings)
				if len(listings) != 4 {
					t.Fatal("SQL time window lost a full-width expiry")
				}
				byHash := map[common.Hash]uint64{}
				for _, in := range expected {
					byHash[in.Order.Hash()] = in.Order.ExpirationTime
				}
				for _, listing := range listings {
					want, ok := byHash[listing.Hash]
					raw, _ := json.Marshal(orderbook.TimeJSON(want))
					if !ok || !bytes.Equal(listing.Expiry, raw) {
						t.Fatal("listing projection lost its exact expiry")
					}
				}
				calls, failures, _ := f.RPCCounts()
				if calls < 2 || failures != 0 {
					t.Fatal("full-width API admission did not execute hash-bound contract checks")
				}
				t.Log(line)
				t.Logf("MARKET-TIME-WIRE-GO app=%s kind=%s admitted=4 future_code=4 original_inputs_preserved=true real_hash_bound_exchange_calls=%d native_transactions=0 driver_joined=true", app, kind, calls)
			})
		}
	}
}
