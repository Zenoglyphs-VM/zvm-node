package nodecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/core/types"
)

// This exercises the actual command setup and derivation path against declared
// loopback RPC responses. No payload is imported and no VM or live NoM runs.
func TestNodeCommandEngineProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the owned child shutdown control requires interrupt delivery")
	}
	for _, test := range []struct{ name, profile, method string }{
		{"legacy", "", "engine_getPayloadV4"},
		{"isthmus", rollup.EVMIsthmus, "engine_getPayloadV4"},
		{"karst", rollup.EVMKarst, "engine_getPayloadV5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			genesis := &types.Header{Number: new(big.Int), Difficulty: new(big.Int), GasLimit: 30000000, Time: 1800000000}
			parent := &znn.Momentum{Height: 1, Hash: znn.Hash{1}, Timestamp: 1800000000}
			momentum := &znn.Momentum{Height: 2, Hash: znn.Hash{2}, PreviousHash: parent.Hash, Timestamp: 1800000001}
			var mu sync.Mutex
			calls := map[string]int{}
			seen := make(chan struct{})
			var once sync.Once
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     json.RawMessage   `json:"id"`
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request) != nil {
					http.Error(w, "invalid probe request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				calls[request.Method]++
				mu.Unlock()
				response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
				switch request.Method {
				case "eth_chainId":
					response["result"] = fmt.Sprintf("0x%x", 7340470)
				case "eth_getBlockByNumber":
					response["result"] = genesis
				case "ledger.getFrontierMomentum":
					response["result"] = momentum
				case "ledger.getDetailedMomentumsByHeight":
					var height uint64
					if len(request.Params) < 1 || json.Unmarshal(request.Params[0], &height) != nil {
						t.Error("missing momentum height")
						return
					}
					var list []*znn.DetailedMomentum
					if height == 1 {
						list = []*znn.DetailedMomentum{{Momentum: parent}}
					}
					if height == 2 {
						list = []*znn.DetailedMomentum{{Momentum: momentum}}
					}
					response["result"] = map[string]any{"count": len(list), "list": list}
				case "engine_forkchoiceUpdatedV3":
					id := engine.PayloadID{1}
					response["result"] = engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: engine.VALID}, PayloadID: &id}
				case "engine_getPayloadV4", "engine_getPayloadV5":
					response["error"] = map[string]any{"code": -38005, "message": "declared transport probe stops before VM execution"}
				default:
					t.Errorf("unexpected probe RPC method %q", request.Method)
					response["error"] = map[string]any{"code": -32601, "message": "unexpected probe method"}
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
				if strings.HasPrefix(request.Method, "engine_getPayload") {
					once.Do(func() { close(seen) })
				}
			}))
			t.Cleanup(web.Close)
			root := t.TempDir()
			config := rollup.Config{L1ChainID: 1, L2ChainID: 7340470, L1StartHeight: 2, GasLimit: 30000000,
				L2GenesisHash: genesis.Hash(), L2GenesisTimestamp: genesis.Time, EVM: test.profile}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "rollup.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "jwt.hex"), []byte(strings.Repeat("0", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			child := exec.CommandContext(ctx, executable, "-test.run=^TestNodeCommandEngineProbeChild$", "-test.v")
			child.Env = append(os.Environ(), "ZVM_NODE_ENGINE_PROBE_CHILD=1", "ZVM_NODE_ENGINE_PROBE_ROOT="+root, "ZVM_NODE_ENGINE_PROBE_URL="+web.URL)
			var output engineProbeOutput
			child.Stdout, child.Stderr = &output, &output
			child.WaitDelay = 2 * time.Second
			if err := child.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan struct{})
			var waitErr error
			go func() { waitErr = child.Wait(); close(done) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("owned command child did not join")
				}
			})
			select {
			case <-seen:
				if err := child.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			case <-done:
				t.Fatalf("command stopped before payload retrieval: %v: %s", waitErr, output.String())
			case <-ctx.Done():
				t.Fatal("command did not reach payload retrieval within its bound")
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("owned command did not stop after interrupt")
			}
			if waitErr != nil || output.truncated {
				t.Fatalf("owned command failed: %v: %s", waitErr, output.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if calls[test.method] != 1 || calls["engine_getPayloadV4"]+calls["engine_getPayloadV5"] != 1 || calls["engine_forkchoiceUpdatedV3"] != 1 || calls["engine_newPayloadV4"] != 0 {
				t.Fatalf("configured command selected unexpected calls: %v", calls)
			}
			record, err := json.Marshal(map[string]any{"profile": test.name, "expectedMethod": test.method, "payloadRequests": 1,
				"actualChildExit": 0, "childJoined": true, "actualCommandPath": "nodecmd.Run", "VMExecution": false, "payloadImport": false})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("NODE-ENGINE-PROFILE %s", record)
		})
	}
}

func TestNodeCommandEngineProbeChild(t *testing.T) {
	if os.Getenv("ZVM_NODE_ENGINE_PROBE_CHILD") != "1" {
		return
	}
	root, endpoint := os.Getenv("ZVM_NODE_ENGINE_PROBE_ROOT"), os.Getenv("ZVM_NODE_ENGINE_PROBE_URL")
	if root == "" || !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatal("missing owned loopback probe inputs")
	}
	err := Run([]string{"-config", filepath.Join(root, "rollup.json"), "-jwt", filepath.Join(root, "jwt.hex"),
		"-db", filepath.Join(root, "node.db"), "-l1", endpoint, "-engine", endpoint, "-eth", endpoint,
		"-mainnet-rpc", "off", "-no-index", "-verify=false", "-listen", ""})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("command did not preserve cancellation: %v", err)
	}
}

func TestNodeCommandRejectsUnknownEngineProfile(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "rollup.json")
	if err := os.WriteFile(config, []byte(`{"l1StartHeight":2,"l2ChainId":1,"gasLimit":30000000,"evm":"unsupported-test-profile"}`), 0600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "must-not-exist.db")
	err := Run([]string{"-config", config, "-db", db, "-jwt", filepath.Join(root, "absent-jwt"), "-mainnet-rpc", "off"})
	if err == nil || !strings.Contains(err.Error(), "unsupported-test-profile") {
		t.Fatalf("unknown profile was not refused before JWT/network/database setup: %v", err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown profile created a database")
	}
}

type engineProbeOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	truncated bool
}

func (b *engineProbeOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if remaining := (64 << 10) - b.data.Len(); len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.data.Write(p)
	return n, nil
}
func (b *engineProbeOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprint(b.data.String())
}
