package indexer_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

type marketBrowserOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *marketBrowserOutput) Len() int       { return b.buffer.Len() }
func (b *marketBrowserOutput) String() string { return b.buffer.String() }

func (b *marketBrowserOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("browser output limit exceeded")
	}
	return b.buffer.Write(p)
}

// The opt-in CI lane builds both pinned applications, then runs their real
// pages against this live Go market/index/book fixture. The ordinary Go suite
// does not silently install a browser or frontend dependencies.
func TestMarketBrowserHTTP(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit pinned-frontend browser CI lane")
	}
	roots := map[string]string{}
	for _, app := range []string{"nft", "karum"} {
		root, err := filepath.Abs(os.Getenv("ZVM_BROWSER_" + strings.ToUpper(app) + "_ROOT"))
		if err != nil || os.Getenv("ZVM_BROWSER_"+strings.ToUpper(app)+"_ROOT") == "" {
			t.Fatal("missing browser application root")
		}
		if info, err := os.Stat(filepath.Join(root, "dist", "index.html")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("pinned application must be built before the browser test")
		}
		roots[app] = root
	}
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedOperator(nftAlice, nftExchange, true))
	f := a08HTTPIndex(t, chain)
	nftIndexThrough(t, f.index, 1, 1)
	f.storeListing(t)
	intent := storedRecoveryIntent(t, f.db.SQL(), f.hash)
	tokenBytes := make([]byte, 32)
	// A per-test capability protects only the disposable fixture controller.
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	type phaseRequest struct {
		name string
		done chan map[string]any
	}
	phases := make(chan phaseRequest)
	var requestsMu sync.Mutex
	requests := map[string]int{}
	count := func(name string) {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		requests[name]++
	}
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error("fixture JSON response failed")
		}
	}
	mux := http.NewServeMux()
	marketURL, err := url.Parse(f.url)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(marketURL)
	for _, path := range []string{"/api/nft", "/api/nft/"} {
		mux.Handle(path, proxy)
	}
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		head, err := f.index.Head()
		if err != nil {
			http.Error(w, "fixture index unavailable", 500)
			return
		}
		writeJSON(w, map[string]any{
			"l1":         map[string]any{"chainId": 69, "frontier": head},
			"l2":         map[string]any{"chainId": 69, "head": head, "finalized": 1, "timestamp": 1010 + head*10},
			"derivation": map[string]any{"momentum": head, "lag": 0},
			"predeploys": map[string]any{}, "apps": map[string]any{"exchange": nftExchange.Hex()},
			"prices": map[string]any{"enabled": false},
		})
	})
	mux.HandleFunc("/api/prices", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"enabled": false, "tokens": map[string]any{}, "stale": true})
	})
	mux.HandleFunc("/api/contracts/verified", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		// Only page transport metadata is declared here. This is not an EVM
		// exchange checker or transaction broadcaster.
		raw, err := io.ReadAll(io.LimitReader(r.Body, (128<<10)+1))
		if err != nil || len(raw) > 128<<10 || !json.Valid(raw) {
			http.Error(w, "bounded fixture RPC required", 400)
			return
		}
		var calls []map[string]json.RawMessage
		batch := len(raw) > 0 && raw[0] == '['
		if batch {
			err = json.Unmarshal(raw, &calls)
		} else {
			var one map[string]json.RawMessage
			err = json.Unmarshal(raw, &one)
			calls = []map[string]json.RawMessage{one}
		}
		if err != nil || len(calls) > 32 {
			http.Error(w, "invalid fixture batch", 400)
			return
		}
		out := make([]map[string]any, 0, len(calls))
		for _, call := range calls {
			var method string
			_ = json.Unmarshal(call["method"], &method)
			count(method)
			row := map[string]any{"jsonrpc": "2.0", "id": call["id"]}
			switch method {
			case "eth_chainId":
				row["result"] = "0x45"
			case "eth_blockNumber":
				head, _ := f.index.Head()
				row["result"] = fmt.Sprintf("0x%x", head)
			case "eth_getBlockByNumber":
				head, _ := f.index.Head()
				header, err := f.index.Eth().HeaderByNumber(r.Context(), new(big.Int).SetUint64(head))
				if err != nil {
					row["error"] = map[string]any{"code": -32000, "message": "fixture header unavailable"}
				} else {
					row["result"] = header
				}
			case "eth_getBalance":
				row["result"] = "0x0"
			case "eth_call":
				row["result"] = "0x" + strings.Repeat("0", 128)
			default:
				row["error"] = map[string]any{"code": -32601, "message": "unsupported fixture RPC; no writes allowed"}
			}
			out = append(out, row)
		}
		if batch {
			writeJSON(w, out)
		} else {
			writeJSON(w, out[0])
		}
	})
	mux.HandleFunc("POST /__fixture/phase", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-ZVM-Fixture") != token {
			http.Error(w, "fixture capability required", 403)
			return
		}
		request := phaseRequest{name: r.URL.Query().Get("name"), done: make(chan map[string]any, 1)}
		select {
		case phases <- request:
		case <-ctx.Done():
			http.Error(w, "fixture stopped", 503)
			return
		}
		select {
		case result := <-request.done:
			writeJSON(w, result)
		case <-ctx.Done():
			http.Error(w, "fixture stopped", 503)
		}
	})
	for _, name := range []string{"nft", "karum"} {
		prefix := "/zvm-" + name + "/"
		dist := filepath.Join(roots[name], "dist")
		files := http.StripPrefix(prefix, http.FileServer(http.Dir(dist)))
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
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
	web := httptest.NewServer(mux)
	defer web.Close()
	input, err := json.Marshal(map[string]any{"base": web.URL, "token": token, "collection": nftCollection.Hex(),
		"alice": nftAlice.Hex(), "bob": nftBob.Hex(), "roots": roots})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "node", filepath.Join("..", "scripts", "market-browser.mjs"))
	cmd.Stdin = bytes.NewReader(input)
	stdout, stderr := marketBrowserOutput{limit: 1 << 20}, marketBrowserOutput{limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	finished := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		finished <- cmd.Run()
		close(joined)
	}()
	defer func() {
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("owned browser driver was not joined")
		}
	}()
	for {
		select {
		case request := <-phases:
			owner, listed := nftAlice, false
			switch request.name {
			case "prefix":
				head, err := f.index.Head()
				if err != nil {
					t.Fatal(err)
				}
				if head > 1 {
					if err := f.index.Rewind(1); err != nil {
						t.Fatal(err)
					}
				}
				f.declareCurrentCheck(t)
				listed = true
			case "orphan":
				chain.Put(2, 0, nftTransfer(nftAlice, nftBob))
				nftIndexThrough(t, f.index, 2, 2)
				f.declareCurrentCheck(t)
				owner = nftBob
			case "unchecked":
				if err := f.index.Rewind(1); err != nil {
					t.Fatal(err)
				}
				f.assertUnchecked(t)
			case "rechecked":
				f.declareCurrentCheck(t)
				listed = true
			case "replacement":
				chain.Put(2, 1, retainedOperator(nftAlice, nftExchange, false))
				nftIndexThrough(t, f.index, 2, 2)
				f.declareCurrentCheck(t)
			default:
				t.Fatal("unknown browser fixture phase")
			}
			f.assertProjection(t, listed, owner)
			if storedRecoveryIntent(t, f.db.SQL(), f.hash) != intent {
				t.Fatal("browser phase changed original order intent")
			}
			request.done <- map[string]any{"phase": request.name, "owner": owner.Hex(), "listed": listed, "hash": f.hash.Hex()}
		case err := <-finished:
			if stdout.Len() > 1<<20 || stderr.Len() > 128<<10 {
				t.Fatal("browser diagnostics exceeded declared bounds")
			}
			t.Log(strings.TrimSpace(stdout.String()))
			if err != nil {
				t.Fatalf("owned browser driver did not pass: %v; stderr bytes=%d", err, stderr.Len())
			}
			requestsMu.Lock()
			defer requestsMu.Unlock()
			for method, count := range requests {
				if strings.Contains(method, "send") && count != 0 {
					t.Fatal("browser attempted a transaction")
				}
			}
			t.Log("MARKET-BROWSER-GO-BOUNDARY actual Go NFT/index/book/HTTP, original intent preserved, no transaction attempts")
			return
		case <-ctx.Done():
			<-finished
			t.Fatal("owned browser integration deadline exceeded")
		}
	}
}
