package orderbook_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

func TestExecutedMarketCreditBrowserRecovery(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit pinned-frontend and compiled credit-wallet CI lane")
	}
	roots := map[string]string{}
	for _, app := range []string{"nft", "karum"} {
		value := os.Getenv("ZVM_BROWSER_" + strings.ToUpper(app) + "_ROOT")
		root, err := filepath.Abs(value)
		if value == "" || err != nil {
			t.Fatal("explicit pinned credit interface root is required")
		}
		if info, err := os.Stat(filepath.Join(root, "dist", "index.html")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("the real credit interface must be built before browser execution")
		}
		roots[app] = root
	}
	for _, app := range []string{"nft", "karum"} {
		t.Run(app, func(t *testing.T) { executedMarketCreditBrowserProfile(t, roots, app, false) })
	}
	for _, app := range []string{"nft", "karum"} {
		t.Run(app+"-mined-revert", func(t *testing.T) { executedMarketCreditBrowserProfile(t, roots, app, true) })
	}
	for _, app := range []string{"nft", "karum"} {
		t.Run(app+"-wallet-calls-response-loss", func(t *testing.T) { executedMarketCreditBrowserProfile(t, roots, app, false, true) })
	}
}

func executedMarketCreditBrowserProfile(t *testing.T, roots map[string]string, app string, minedRevert bool, walletCalls ...bool) {
	if len(walletCalls) > 1 {
		t.Fatal("one wallet protocol variant is required")
	}
	useWalletCalls := len(walletCalls) == 1 && walletCalls[0]
	if useWalletCalls && minedRevert {
		t.Fatal("wallet response-loss profiles use the successful payout variant")
	}

	f, transport := orderbook.NewExecutedMarketCreditBrowserFixture(t, minedRevert)
	f.Advance(0, 6)
	deployment := filepath.Join(t.TempDir(), "apps.json")
	if err := (&apps.Deployment{ChainID: f.Rollup().L2ChainID, Exchange: f.Exchange(), CollectionFactory: f.CollectionFactory()}).Save(deployment); err != nil {
		t.Fatal(err)
	}
	service := market.New(market.Config{DB: f.DB(), Rollup: f.Rollup(), Orders: f.Book(),
		Apps: &apps.File{Path: deployment, ChainID: f.Rollup().L2ChainID}})
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(capability)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	relayerURL, joinRelayer := startProductionMarketRelayer(t, ctx, f, transport.SourceURL())
	defer joinRelayer()
	transport.UseRelayer(t, relayerURL)
	type phaseRequest struct {
		name   string
		result chan map[string]any
	}
	phases := make(chan phaseRequest)
	rpcProgress := newMarketRPCProgress()
	ui := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error("credit fixture JSON response failed")
		}
	}
	ui.HandleFunc("GET /api/prices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"enabled": false, "tokens": map[string]any{}, "stale": true})
	})
	ui.HandleFunc("GET /__fixture/rpc-progress", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ZVM-Fixture") != token {
			http.Error(w, "fixture capability required", 403)
			return
		}
		writeJSON(w, rpcProgress.snapshot())
	})
	ui.HandleFunc("POST /__fixture/credit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ZVM-Fixture") != token {
			http.Error(w, "fixture capability required", 403)
			return
		}
		request := phaseRequest{name: r.URL.Query().Get("name"), result: make(chan map[string]any, 1)}
		select {
		case phases <- request:
		case <-ctx.Done():
			http.Error(w, "credit fixture stopped", 503)
			return
		}
		select {
		case result := <-request.result:
			writeJSON(w, result)
		case <-ctx.Done():
			http.Error(w, "credit fixture stopped", 503)
		}
	})
	for _, name := range []string{"nft", "karum"} {
		prefix, dist := "/zvm-"+name+"/", filepath.Join(roots[name], "dist")
		files := http.StripPrefix(prefix, http.FileServer(http.Dir(dist)))
		ui.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			path := strings.TrimPrefix(r.URL.Path, prefix)
			if strings.Contains(path, "..") {
				http.Error(w, "invalid credit fixture path", 400)
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
	web := httptest.NewServer(rpcProgress.observe(handler))
	defer web.Close()
	// These public-test-key orders are explicit source-sale setup. Their actual
	// HTTP admission is retained separately from the browser withdrawal call.
	rawOrders, err := json.Marshal(map[string]any{"orders": f.Inputs(t)})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, web.URL+"/api/nft/orders", bytes.NewReader(rawOrders))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || len(body) > 64<<10 {
		t.Fatal("credit setup orders were not admitted through actual HTTP")
	}
	var count int
	if err := f.DB().QueryRow("SELECT count(*) FROM nft_orders WHERE source='api'").Scan(&count); err != nil || count != 2 {
		t.Fatal("credit setup did not admit exactly two source-bound signed orders")
	}
	stages := []uint64{}
	for height := uint64(7); height <= 14; height++ {
		stages = append(stages, height)
	}
	f.Advance(0, stages...)
	initial := transport.AssertCreditState(t, false)
	input, err := json.Marshal(map[string]any{"base": web.URL, "token": token, "roots": roots, "app": app,
		"chainId": f.Rollup().L2ChainID, "wallet": f.CreditWallet().Hex(), "owner": f.Buyer().Hex(),
		"exchange": f.Exchange().Hex(), "walletABI": f.CreditWalletABI(), "expectedCredit": "5000000000000000", "minedRevert": minedRevert, "walletCalls": useWalletCalls})
	if err != nil {
		t.Fatal(err)
	}
	script := "market-credit-browser.mjs"
	if useWalletCalls {
		script = "market-wallet-credit-browser.mjs"
	}
	cmd := exec.CommandContext(ctx, "node", filepath.Join("..", "scripts", script))
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := &executedBrowserOutput{limit: 1 << 20}, &executedBrowserOutput{limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	finished, joined := make(chan error, 1), make(chan struct{})
	go func() { finished <- cmd.Run(); close(joined) }()
	defer func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned credit browser was not joined")
		}
	}()
	expected := []string{"refused", "allow", "ordered", "mined", "replacement", "restored"}
	phaseIndex := 0
	var canonical map[string]any
	var canonicalHash string
	var epoch uint64
	observations := []map[string]any{{"phase": "initial", "state": initial}}
	for {
		select {
		case request := <-phases:
			if phaseIndex >= len(expected) || request.name != expected[phaseIndex] {
				t.Fatal("unexpected credit browser phase ordering")
			}
			phaseIndex++
			included := false
			switch request.name {
			case "refused":
				transport.AssertCreditReceiverRefusal(t)
			case "allow":
				f.Advance(0, 15, 16)
				transport.PinFundedActor(t)
			case "ordered":
				transport.BindPublishedTransaction(t, ctx)
				f.Advance(0, 17, 18)
				included = true
				canonicalHash, epoch = f.HeadHash().Hex(), f.Epoch(t)
			case "mined":
				transport.AssertCanonicalTransaction(t, true)
				included = true
			case "replacement":
				f.Advance(1, 17, 18)
				transport.AssertCanonicalTransaction(t, false)
				if f.HeadHash().Hex() == canonicalHash || f.Epoch(t) <= epoch {
					t.Fatal("credit withdrawal replacement did not select a new canonical epoch")
				}
				epoch = f.Epoch(t)
			case "restored":
				f.Advance(0, 17, 18)
				transport.AssertCanonicalTransaction(t, true)
				included = true
				if f.HeadHash().Hex() != canonicalHash || f.Epoch(t) <= epoch {
					t.Fatal("credit withdrawal restoration did not recover its original canonical branch")
				}
			}
			state := transport.AssertCreditState(t, included)
			if request.name == "mined" {
				canonical = state
			}
			if request.name == "restored" && !reflect.DeepEqual(canonical, state) {
				t.Fatal("restored credit contract and balance state differs")
			}
			observations = append(observations, map[string]any{"phase": request.name, "state": state})
			request.result <- map[string]any{"phase": request.name, "state": state, "hash": transport.TransactionHash().Hex(),
				"actorNonce": transport.ActorNonce(), "actualCompiledContractAndBalances": true,
				"sourcePrefixPreserved": true, "included": included}
		case err := <-finished:
			t.Log(strings.TrimSpace(stdout.raw.String()))
			rpcProgress.log(t)
			if err != nil {
				t.Fatalf("actual credit browser failed: %v; bounded stderr=%s", err, stderr.raw.String())
			}
			if phaseIndex != len(expected) {
				t.Fatal("credit browser did not finish every canonical phase")
			}
			transport.AssertCompleted(t)
			exit := joinRelayer()
			record := map[string]any{"app": app, "minedRevert": minedRevert, "walletCalls": useWalletCalls, "pass": true, "phases": expected, "observations": observations,
				"actualProductionCreditPage": true, "actualNodeAPIAndEVMContractState": true,
				"actualOfficialRelayerProgram": true, "actualRelayerJoined": true, "actualRelayerExit": exit,
				"ownedCancellationMarkerVerified": true, "ownedBrowserJoined": true,
				"sourceSaleInputsAdmittedThroughHTTP": 2, "sourceSaleUsesPublicTestSignatures": true,
				"declaredNativeSaleSetup": true, "manualSQLOrStorageWrites": 0,
				"creditTransport": transport.CreditEvidence(t), "realWalletOrPublicNetworkOperations": 0}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			marker := "EXECUTED-CREDIT-BROWSER-GO "
			if useWalletCalls {
				marker = "EXECUTED-WALLET-CREDIT-BROWSER-GO "
			}
			t.Log(marker + string(raw))
			return
		case <-ctx.Done():
			<-finished
			rpcProgress.log(t)
			t.Fatal("owned positive credit integration deadline exceeded")
		}
	}
}
