package orderbook_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

type executedBrowserOutput struct {
	raw   bytes.Buffer
	limit int
}

func (b *executedBrowserOutput) Write(raw []byte) (int, error) {
	if len(raw) > b.limit-b.raw.Len() {
		return 0, fmt.Errorf("executed browser output exceeds bound")
	}
	return b.raw.Write(raw)
}

// This opt-in lane uses actual production pages and their EIP-1193 request,
// real signatures from a public test signer, the real node HTTP handler and
// exchange-backed admission. It does not intercept or fabricate market/RPC
// replies in the browser. The actual pinned relayer CLI posts signed EVM
// transactions; NoM authority and the execution RPC adapter remain fixtures.
func TestExecutedMarketBrowserAdmissionAndRecovery(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit pinned-frontend browser CI lane")
	}
	roots := map[string]string{}
	for _, app := range []string{"nft", "karum"} {
		value := os.Getenv("ZVM_BROWSER_" + strings.ToUpper(app) + "_ROOT")
		root, err := filepath.Abs(value)
		if value == "" || err != nil {
			t.Fatal("missing pinned browser application root")
		}
		if info, err := os.Stat(filepath.Join(root, "dist", "index.html")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("production application must be built before execution")
		}
		roots[app] = root
	}
	for _, app := range []string{"nft", "karum"} {
		for _, kind := range []string{"single", "bulk"} {
			for _, action := range []string{"buy", "cancel", "nonce", "approval", "batch", "partial-batch"} {
				name := app + "-" + kind
				if action != "buy" {
					name += "-" + action
				}
				t.Run(name, func(t *testing.T) { executedMarketBrowserProfile(t, roots, app, kind, action) })
			}
		}
	}
	// Preserve every native case and add separately named token cases.
	t.Run("nft-single-qsr", func(t *testing.T) { executedMarketBrowserProfile(t, roots, "nft", "single", "buy", "qsr") })
	t.Run("karum-single-mixed-batch", func(t *testing.T) { executedMarketBrowserProfile(t, roots, "karum", "single", "batch", "mixed") })
	t.Run("karum-single-mixed-partial-batch", func(t *testing.T) {
		executedMarketBrowserProfile(t, roots, "karum", "single", "partial-batch", "mixed")
	})
	t.Run("nft-single-qsr-missing-allowance", func(t *testing.T) { executedMarketBrowserProfile(t, roots, "nft", "single", "buy", "qsr-approval") })
	t.Run("karum-single-mixed-missing-allowance", func(t *testing.T) {
		executedMarketBrowserProfile(t, roots, "karum", "single", "batch", "mixed-approval")
	})
	t.Run("nft-bulk-qsr-missing-allowance", func(t *testing.T) {
		executedMarketBrowserProfile(t, roots, "nft", "bulk", "buy", "qsr-approval")
	})
	t.Run("karum-bulk-qsr-missing-allowance", func(t *testing.T) {
		executedMarketBrowserProfile(t, roots, "karum", "bulk", "batch", "qsr-approval")
	})
	for _, app := range []string{"nft", "karum"} {
		t.Run(app+"-single-external-royalty-batch", func(t *testing.T) { executedMarketBrowserProfile(t, roots, app, "single", "batch", "external-royalty") })
	}

}

func executedMarketBrowserProfile(t *testing.T, roots map[string]string, app, kind, action string, payment ...string) {
	var f *orderbook.ExecutedMarketFixture
	var transport *orderbook.ExecutedMarketProviderFixture
	if len(payment) > 1 {
		t.Fatal("one token payment profile is required")
	}
	if len(payment) == 1 && payment[0] == "external-royalty" {
		if kind != "single" || action != "batch" {
			t.Fatal("undeclared external royalty browser profile")
		}
		f, transport = orderbook.NewExecutedExternalRoyaltyBrowserFixture(t, kind)
	} else if len(payment) == 1 {
		basePayment := strings.TrimSuffix(payment[0], "-approval")
		if !((basePayment == "qsr" && ((app == "nft" && action == "buy") || (app == "karum" && kind == "bulk" && action == "batch"))) || (kind == "single" && app == "karum" && (action == "batch" || action == "partial-batch") && basePayment == "mixed")) {
			t.Fatal("undeclared token browser profile")
		}
		f, transport = orderbook.NewExecutedMarketTokenKindBrowserFixture(t, kind, payment[0], action)
	} else if action == "batch" || action == "partial-batch" {
		f, transport = orderbook.NewExecutedMarketBatchBrowserFixture(t, kind, action)
	} else if action == "buy" {
		f, transport = orderbook.NewExecutedMarketProviderBrowserFixture(t, kind)
	} else {
		f, transport = orderbook.NewExecutedMarketOwnerBrowserFixture(t, kind, action)
	}
	f.Advance(0, 6)
	var count int
	if err := f.DB().QueryRow("SELECT count(*) FROM nft_orders").Scan(&count); err != nil || count != 0 {
		t.Fatal("executed browser must start with an empty book")
	}
	deployment := filepath.Join(t.TempDir(), "apps.json")
	appConfig := &apps.Deployment{ChainID: f.Rollup().L2ChainID, Exchange: f.Exchange(), CollectionFactory: f.CollectionFactory()}
	if f.TokenPaymentProfile() != "" {
		appConfig.RewardToken = f.TokenPaymentAddress()
	}
	if err := appConfig.Save(deployment); err != nil {
		t.Fatal(err)
	}
	service := market.New(market.Config{DB: f.DB(), Rollup: f.Rollup(), Orders: f.Book(),
		Apps: &apps.File{Path: deployment, ChainID: f.Rollup().L2ChainID}})
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relayerURL, joinRelayer := startProductionMarketRelayer(t, ctx, f, transport.SourceURL())
	defer joinRelayer()
	transport.UseRelayer(t, relayerURL)
	type phaseRequest struct {
		name   string
		result chan map[string]any
	}
	phases := make(chan phaseRequest)
	ui := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error("fixture JSON response failed")
		}
	}
	// Optional USD pricing is disabled. The node's info, names, verification
	// marks, market and execution RPC routes are their actual implementations.
	ui.HandleFunc("GET /api/prices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"enabled": false, "tokens": map[string]any{}, "stale": true})
	})
	ui.HandleFunc("POST /__fixture/phase", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ZVM-Fixture") != token {
			http.Error(w, "fixture capability required", 403)
			return
		}
		request := phaseRequest{name: r.URL.Query().Get("name"), result: make(chan map[string]any, 1)}
		select {
		case phases <- request:
		case <-ctx.Done():
			http.Error(w, "fixture stopped", 503)
			return
		}
		select {
		case result := <-request.result:
			writeJSON(w, result)
		case <-ctx.Done():
			http.Error(w, "fixture stopped", 503)
		}
	})
	for _, name := range []string{"nft", "karum"} {
		prefix, dist := "/zvm-"+name+"/", filepath.Join(roots[name], "dist")
		files := http.StripPrefix(prefix, http.FileServer(http.Dir(dist)))
		ui.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, prefix)
			if strings.Contains(path, "..") {
				http.Error(w, "invalid fixture path", 400)
				return
			}
			if info, err := os.Stat(filepath.Join(dist, path)); err == nil && info.Mode().IsRegular() {
				files.ServeHTTP(w, r)
				return
			}
			http.ServeFile(w, r, filepath.Join(dist, "index.html"))
		})
	}
	handler := api.New(api.Config{Rollup: f.Rollup(), DB: f.Store(), Eth: f.ExecutionClient(),
		L1: znn.NewClient(f.SourceURL()), Deriver: f.Deriver(), EthURL: f.ExecutionURL(), L1URL: f.SourceURL(),
		Extensions: []api.Extension{service}, Relayer: relayerURL, UI: ui}).Handler()
	var requestMu sync.Mutex
	var submitted []*orderbook.Input
	posts := 0
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/nft/orders" {
			body := r.Body
			raw, err := io.ReadAll(io.LimitReader(body, (256<<10)+1))
			body.Close()
			if err != nil || len(raw) > 256<<10 {
				http.Error(w, "bounded fixture order request required", 400)
				return
			}
			var capture struct {
				Orders []*orderbook.Input `json:"orders"`
			}
			if err := json.Unmarshal(raw, &capture); err != nil {
				http.Error(w, "invalid fixture order input", 400)
				return
			}
			requestMu.Lock()
			if posts >= 2 || len(submitted)+len(capture.Orders) > 2 {
				requestMu.Unlock()
				http.Error(w, "unexpected repeated fixture order submission", 400)
				return
			}
			posts++
			submitted = append(submitted, capture.Orders...)
			requestMu.Unlock()
			// Replay exactly the same request bytes to the real API handler;
			// the observation copy cannot be mutated by Book.Submit.
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		handler.ServeHTTP(w, r)
	}))
	defer web.Close()
	input, err := json.Marshal(map[string]any{"base": web.URL, "token": token, "roots": roots,
		"app": app, "kind": kind, "collection": f.Collection().Hex(), "exchange": f.Exchange().Hex(),
		"seller": f.Seller().Hex(), "buyer": f.Buyer().Hex(), "chainId": f.Rollup().L2ChainID, "action": action, "actor": transport.Actor().Hex(), "payouts": f.MarketPayoutConfig(), "tokenPayment": f.TokenPaymentDescription(), "externalRoyalty": f.ExternalRoyaltyDescription()})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "node", filepath.Join("..", "scripts", "market-execution-browser.mjs"))
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := &executedBrowserOutput{limit: 1 << 20}, &executedBrowserOutput{limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	finished := make(chan error, 1)
	joined := make(chan struct{})
	go func() { finished <- cmd.Run(); close(joined) }()
	defer func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned signed-browser driver was not joined")
		}
	}()
	expectedPhases := []string{"bind", "ordered", "mined", "replacement", "restored"}
	batch := action == "batch" || action == "partial-batch"
	if action == "partial-batch" {
		expectedPhases = []string{"bind", "cancel-ordered", "cancel-mined", "ordered", "mined", "replacement", "restored"}
	}
	if f.TokenApprovalRequired() {
		expectedPhases = []string{"bind", "approval-ordered", "approval-mined", "ordered", "mined", "replacement", "restored"}
	}
	phaseIndex := 0
	var inputs []*orderbook.Input
	var reference map[string]any
	var referenceHash string
	var epoch uint64
	for {
		select {
		case request := <-phases:
			if phaseIndex >= len(expectedPhases) || request.name != expectedPhases[phaseIndex] {
				t.Fatal("unexpected signed-browser phase ordering")
			}
			phaseIndex++
			filled, batchStage := -1, 0
			included := false
			switch request.name {
			case "bind":
				requestMu.Lock()
				captured, observedPosts := append([]*orderbook.Input(nil), submitted...), posts
				requestMu.Unlock()
				wantPosts := 2
				if kind == "bulk" {
					wantPosts = 1
				}
				if observedPosts != wantPosts {
					t.Fatal("actual browser prompt/submission cardinality differs")
				}
				f.BindAcceptedBrowserInputs(t, captured)
				inputs = f.Inputs(t)
				heights := []uint64{}
				for h := uint64(7); h <= transport.FundingHeight(); h++ {
					heights = append(heights, h)
				}
				f.Advance(0, heights...)
				transport.PinFundedActor(t)
				transport.PinMarketPayouts(t)
				if batch {
					transport.PinBatchBalances(t)
				}
			case "approval-ordered":
				transport.BindTokenApproval(t, ctx)
				f.Advance(0, transport.ActionHeight())
			case "approval-mined":
				transport.AssertTokenApproval(t)
			case "cancel-ordered":
				transport.BindBatchCancellation(t, ctx)
				f.Advance(0, transport.ActionHeight())
				batchStage = 1
			case "cancel-mined":
				transport.AssertBatchCancellation(t)
				batchStage = 1
			case "ordered":
				transport.BindPublishedTransaction(t, ctx)
				heights := []uint64{transport.ActionHeight(), transport.LastHeight()}
				if action == "partial-batch" || f.TokenApprovalRequired() {
					heights = []uint64{transport.ActionHeight() + 1, transport.LastHeight()}
				}
				f.Advance(0, heights...)
				included = true
				if batch {
					batchStage = 2
				}
				if action == "buy" {
					filled = 0
				}
				referenceHash, epoch = f.HeadHash().Hex(), f.Epoch(t)
			case "mined":
				transport.AssertCanonicalTransaction(t, true)
				included = true
				if batch {
					batchStage = 2
				}
				if action == "buy" {
					filled = 0
				}
			case "replacement":
				f.Advance(1, transport.ActionHeight(), transport.LastHeight())
				filled = -1
				transport.AssertCanonicalTransaction(t, false)
				if f.HeadHash().Hex() == referenceHash || f.Epoch(t) <= epoch {
					t.Fatal("browser source replacement did not select and recheck a new canonical branch")
				}
				epoch = f.Epoch(t)
			case "restored":
				heights := []uint64{transport.ActionHeight(), transport.LastHeight()}
				f.Advance(0, heights...)
				transport.AssertCanonicalTransaction(t, true)
				included = true
				if batch {
					batchStage = 2
				}
				if action == "buy" {
					filled = 0
				}
				if f.HeadHash().Hex() != referenceHash || f.Epoch(t) <= epoch {
					t.Fatal("browser source restoration did not select its original canonical branch")
				}
			}
			f.AssertNoOrderApprovalLogs(t)
			var projection map[string]any
			if batch {
				projection = executedBatchMarketProjection(t, f, transport, web.URL, inputs, batchStage)
			} else if action == "buy" {
				projection = executedMarketProjection(t, f, web.URL, inputs, filled)
			} else {
				projection = executedOwnerMarketProjection(t, f, transport, web.URL, inputs, included)
			}
			if request.name == "mined" {
				reference = projection
			}
			if request.name == "restored" && !reflect.DeepEqual(projection, reference) {
				t.Fatal("restored signed-browser HTTP model differs")
			}
			payouts := transport.AssertMarketPayouts(t, request.name, included)
			hashes := []string{inputs[0].Order.Hash().Hex(), inputs[1].Order.Hash().Hex()}
			request.result <- map[string]any{"phase": request.name, "hashes": hashes, "filled": filled,
				"actualAcceptedBrowserInputs": true, "alreadyDerivedSourcePrefixPreserved": true,
				"transaction": transport.TransactionHash().Hex(), "actorNonce": transport.ActorNonce(), "actor": transport.Actor().Hex(), "ownerModel": projection, "included": included,
				"batchStage": batchStage, "sellerNonce": transport.BatchSellerNonce(), "cancellation": transport.BatchCancellationHash().Hex(), "approval": transport.TokenApprovalHash().Hex(), "payouts": payouts}
		case err := <-finished:
			t.Log(strings.TrimSpace(stdout.raw.String()))
			if err != nil {
				t.Fatalf("signed production browser did not pass: %v; bounded stderr=%s", err, stderr.raw.String())
			}
			if phaseIndex != len(expectedPhases) {
				t.Fatal("signed production browser did not complete every canonical phase")
			}
			successful, failed, headers := f.RPCCounts()
			if successful < 3 || failed != 0 || headers < 3 {
				t.Fatalf("signed-browser checker accounting: successful=%d failed=%d headers=%d", successful, failed, headers)
			}
			relayerExit := joinRelayer()
			transport.AssertCompleted(t)
			record := map[string]any{"app": app, "kind": kind, "action": action, "pass": true, "phases": expectedPhases,
				"actualNodeAPIAndExchangeCheckedBook": true, "actualAcceptedBrowserInputs": 2,
				"alreadyDerivedSourcePrefixPreserved": true, "inputBytesPreservedAcrossBothReplacements": true,
				"successfulExchangeChecks": successful, "unexpectedExchangeRPCErrors": failed,
				"cancelledJoinedExchangeCalls": f.RPCCancellations(), "distinctExecutedCheckHeaders": headers,
				"manualSQLOrderOrCacheControls": 0, "manualIndexOrBookSteps": 0, "ownedDriverJoined": true,
				"unsignedSourceAndExecutionRPCFixture": true, "actualOfficialRelayerProgram": true, "actualRelayerJoined": true, "actualRelayerExit": relayerExit, "ownedCancellationMarkerVerified": true,
				"actualProviderEVMTransaction": transport.Evidence(), "actualNonzeroPayouts": transport.MarketPayoutEvidence(t), "realWalletOrPublicNetworkOperations": 0}
			if value := f.ExternalRoyaltyDescription(); value != nil {
				record["externalRoyalty"] = value
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			marker := "EXECUTED-MARKET-BROWSER-GO "
			if f.ExternalRoyaltyDescription() != nil {
				marker = "EXECUTED-EXTERNAL-ROYALTY-BROWSER-GO "
			}
			if f.TokenPaymentProfile() != "" {
				marker = "EXECUTED-TOKEN-PAYMENT-BROWSER-GO "
				if f.TokenApprovalRequired() {
					marker = "EXECUTED-TOKEN-APPROVAL-BROWSER-GO "
				}
			}
			t.Log(marker + string(raw))
			return
		case <-ctx.Done():
			<-finished
			t.Fatal("owned signed-browser integration deadline exceeded")
		}
	}
}
