package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A wallet's pending-nonce lookup has to be answered by the relayer, because
// the execution client's pool is not this chain's pool: a relayed transaction
// goes to the relayer and on to NoM, and the client never sees it until it is
// derived. Everything else must still reach the client untouched.
func TestPendingNonceComesFromTheRelayer(t *testing.T) {
	const fromRelayer = `{"jsonrpc":"2.0","id":1,"result":"0x2a"}`
	const fromClient = `{"jsonrpc":"2.0","id":1,"result":"0x7"}`

	relayer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "eth_getTransactionCount") {
			t.Errorf("the relayer was asked %q; only the nonce lookup belongs here", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fromRelayer))
	}))
	defer relayer.Close()

	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fromClient))
	}))
	defer client.Close()

	s := &Server{cfg: Config{Relayer: relayer.URL, EthURL: client.URL}}
	h := s.ethRPC(s.proxy(client.URL, ""))

	ask := func(body string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		return strings.TrimSpace(rec.Body.String())
	}

	for _, tc := range []struct {
		name, body, want string
	}{
		{
			"pending goes to the relayer",
			`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","pending"]}`,
			fromRelayer,
		},
		{
			"latest is the chain's own answer",
			`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","latest"]}`,
			fromClient,
		},
		{
			"another method passes through",
			`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
			fromClient,
		},
	} {
		if got := ask(tc.body); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A batch is split: pending-nonce lookups to the relayer, everything else to
// the client, and the replies merged back in the caller's order.
func TestABatchIsSplitAndMerged(t *testing.T) {
	var relayerSaw, clientSaw []string
	echo := func(tag string, saw *[]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var reqs []struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &reqs); err != nil {
				t.Errorf("%s was sent a non-batch %s", tag, body)
			}
			out := make([]string, len(reqs))
			for i, q := range reqs {
				*saw = append(*saw, q.Method)
				out[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"%s"}`, q.ID, tag)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[" + strings.Join(out, ",") + "]"))
		}))
	}
	relayer := echo("relayer", &relayerSaw)
	defer relayer.Close()
	client := echo("client", &clientSaw)
	defer client.Close()

	s := &Server{cfg: Config{Relayer: relayer.URL, EthURL: client.URL}}
	h := s.ethRPC(s.proxy(client.URL, ""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(`[
		{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]},
		{"jsonrpc":"2.0","id":2,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","pending"]},
		{"jsonrpc":"2.0","id":3,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","latest"]},
		{"jsonrpc":"2.0","id":"x","method":"eth_getTransactionCount","params":["0x2222222222222222222222222222222222222222","pending"]}
	]`)))

	var got []struct {
		ID     json.RawMessage `json:"id"`
		Result string          `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("reply is not a batch: %s", rec.Body.String())
	}
	want := []struct{ id, result string }{{"1", "client"}, {"2", "relayer"}, {"3", "client"}, {`"x"`, "relayer"}}
	if len(got) != len(want) {
		t.Fatalf("got %d replies, want %d: %s", len(got), len(want), rec.Body.String())
	}
	for i, w := range want {
		if string(got[i].ID) != w.id || got[i].Result != w.result {
			t.Errorf("reply %d: id %s from %s, want id %s from %s", i, got[i].ID, got[i].Result, w.id, w.result)
		}
	}
	if len(relayerSaw) != 2 || len(clientSaw) != 2 {
		t.Errorf("relayer saw %v, client saw %v; want two each", relayerSaw, clientSaw)
	}
}

// A batch with no pending-nonce lookup must reach the client byte for byte, and
// so must one the relayer cannot answer — the relayer is the better answer, not
// the only one.
func TestBatchFallbacks(t *testing.T) {
	const fromClient = `[{"jsonrpc":"2.0","id":1,"result":"0x7"}]`
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fromClient))
	}))
	defer client.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()

	s := &Server{cfg: Config{Relayer: down.URL, EthURL: client.URL}}
	h := s.ethRPC(s.proxy(client.URL, ""))
	for _, body := range []string{
		`[{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}]`,
		`[{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","pending"]}]`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body)))
		if got := strings.TrimSpace(rec.Body.String()); got != fromClient {
			t.Errorf("%s: got %s, want the client's %s", body, got, fromClient)
		}
	}
}

// With no relayer configured the wrapper must be inert, so a node started
// without one behaves exactly as it did before.
func TestWithoutARelayerEverythingPassesThrough(t *testing.T) {
	const fromClient = `{"jsonrpc":"2.0","id":1,"result":"0x7"}`
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fromClient))
	}))
	defer client.Close()

	s := &Server{cfg: Config{EthURL: client.URL}}
	h := s.ethRPC(s.proxy(client.URL, ""))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/rpc",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x1111111111111111111111111111111111111111","pending"]}`))
	h.ServeHTTP(rec, req)
	if got := strings.TrimSpace(rec.Body.String()); got != fromClient {
		t.Errorf("got %s, want the client's answer %s", got, fromClient)
	}
}

// A node on a public hostname refuses the execution client's debug namespaces,
// in a single request and in a batch, and everything else still goes through.
// With no deny list the handler is the one it wraps.
func TestRPCDenyRefusesNamespaces(t *testing.T) {
	const upstream = `{"jsonrpc":"2.0","id":1,"result":"0x1"}`
	reached := 0
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstream))
	}))
	defer client.Close()

	s := &Server{cfg: Config{EthURL: client.URL, RPCDeny: []string{"debug_", "txpool_"}}}
	h := s.denyRPC(s.ethRPC(s.proxy(client.URL, "")))
	ask := func(body string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body)))
		return strings.TrimSpace(rec.Body.String())
	}

	for _, tc := range []struct {
		name, body string
		wantReach  bool
		wantIn     string
	}{
		{"debug is refused under its own id",
			`{"jsonrpc":"2.0","id":7,"method":"debug_traceCall","params":[]}`,
			false, `"id":7,"jsonrpc":"2.0"`},
		{"an ordinary call goes through",
			`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`,
			true, `"result":"0x1"`},
		{"a batch with one denied method is refused whole",
			`[{"jsonrpc":"2.0","id":1,"method":"eth_chainId"},{"jsonrpc":"2.0","id":2,"method":"txpool_content"}]`,
			false, `"code":-32601`},
		{"a clean batch goes through",
			`[{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}]`,
			true, `"result":"0x1"`},
		// The filter fails closed: what it cannot read, the execution client
		// might read differently.
		{"a body that is not JSON is refused, not forwarded",
			`not json`,
			false, `"code":-32700`},
		// The two shapes that reached txpool_ on the public devnet past
		// -rpc-deny. Go's decoder folds key case and keeps the last duplicate;
		// op-reth reads the exact key "method".
		{"a second method key in another case does not hide the first",
			`{"jsonrpc":"2.0","id":1,"method":"txpool_status","Method":"eth_chainId","params":[]}`,
			false, `"code":-32601`},
		{"one malformed element does not wave a batch through",
			`[{"jsonrpc":"2.0","id":1,"method":"txpool_status","params":[]},{"jsonrpc":"2.0","id":2,"method":5}]`,
			false, `"code":-32601`},
		{"a denied method in a case variant is refused too",
			`{"jsonrpc":"2.0","id":1,"METHOD":"txpool_status","method":"eth_chainId"}`,
			false, `"code":-32601`},
		{"an exact duplicate key cannot hide a denied method",
			`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","method":"txpool_status"}`,
			false, `"code":-32601`},
		{"an escaped key is read as the key it spells",
			`{"jsonrpc":"2.0","id":1,"method":"txpool_status"}`,
			false, `"code":-32601`},
		{"a batch element that is not an object is refused",
			`[{"jsonrpc":"2.0","id":1,"method":"eth_chainId"},5]`,
			false, `"code":-32600`},
		{"a request with trailing data is refused",
			`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"} {"jsonrpc":"2.0","id":2,"method":"txpool_status"}`,
			false, `"code":-32600`},
		{"an empty batch is refused",
			`[]`,
			false, `"code":-32700`},
	} {
		before := reached
		got := ask(tc.body)
		if (reached > before) != tc.wantReach {
			t.Errorf("%s: reached upstream = %v, want %v (answer %s)", tc.name, reached > before, tc.wantReach, got)
		}
		if !strings.Contains(got, tc.wantIn) {
			t.Errorf("%s: answer %s lacks %s", tc.name, got, tc.wantIn)
		}
	}

	// Refused whole rather than cut short: a truncated body that happened to
	// parse would be another request than the one the filter read.
	rec := httptest.NewRecorder()
	before := reached
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(strings.Repeat(" ", maxRPCBody+1))))
	if rec.Code != http.StatusRequestEntityTooLarge || reached != before {
		t.Errorf("an oversized body: status %d, reached upstream %v; want 413 and not reached", rec.Code, reached != before)
	}

	if open := (&Server{cfg: Config{}}); open.denyRPC(http.NotFoundHandler()) == nil {
		t.Error("no deny list should return the wrapped handler")
	}
}
