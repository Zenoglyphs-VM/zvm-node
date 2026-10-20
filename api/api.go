// Package api serves the explorer: the index, both chains' live state, and
// same-origin proxies to op-geth, the NoM node and the ZVM's own services, so
// the frontend and a wallet can talk to everything through one port.
package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/node"
	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

type Config struct {
	Rollup  *rollup.Config
	DB      *store.DB
	Eth     *ethclient.Client
	L1      *znn.Client
	Deriver *node.Deriver
	EthURL  string
	L1URL   string
	Faucet  string // base URL of the faucet service, optional
	// L1Explorer is a Zenon explorer this chain's NoM blocks can be looked up
	// in: <base>/transaction/<hash> and <base>/momentum/<height>, which is what
	// explorer.zenon.network and zenonhub.io/explorer both serve. Optional.
	L1Explorer string
	Bridge     string // base URL of the bridge service, optional
	Relayer    string // base URL of the relayer, optional
	// RPCDeny lists method-name prefixes /rpc refuses ("debug_", "txpool_"),
	// for a node whose hostname is public. The execution client answers those
	// namespaces on the same port as eth_*, and a trace over a large block is a
	// cheap way to keep it busy. Empty refuses nothing.
	RPCDeny []string
	Dex     string // path to a Uniswap V2 deployment file, optional
	// Extensions are everything this node serves that is not the chain itself:
	// the DEX pages, the NFT market and its order book, dollar prices, the
	// media store. They live in their own module (zvm-app) and register
	// themselves here, so that a node that only derives and indexes carries
	// none of their code and none of their tables.
	Extensions []Extension
	// Ops is what the node operations page sizes and compares against.
	Ops OpsConfig
	// Verify publishes contract sources, or is nil on a node that does not.
	Verify *verify.Service
	UI     http.Handler
	Log    *slog.Logger
}

// An Extension adds routes and a slice of /api/info to a node. Register is
// called once while the handler is built; Info is called on every /api/info
// and its keys are merged into the reply; Wrap goes outside everything, which
// is how the media store answers its own hostname with stored objects rather
// than with the explorer. Info and Wrap may be left inert.
type Extension interface {
	Register(mux *http.ServeMux)
	Info() map[string]any
	Wrap(http.Handler) http.Handler
}

type Server struct {
	cfg Config
	db  *sql.DB

	opsMu      sync.RWMutex
	opsStorage *storageSample
	opsHistory []historyPoint
	opsMainnet *mainnetSample

	marks   sync.Map // common.Address → markEntry
	holders sync.Map // token address → *holderSet
	// marksSwept is when expired marks were last dropped, as unix nanoseconds.
	marksSwept atomic.Int64
}

func New(cfg Config) *Server {
	if cfg.Ops.StartedAt.IsZero() {
		cfg.Ops.StartedAt = time.Now()
	}
	return &Server{cfg: cfg, db: cfg.DB.SQL()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.handle(s.info))
	// A readiness probe: 200 while this node is keeping up, 503 once it is
	// stalled. Derivation retries forever rather than exiting (docs/HALT.md),
	// so without this a halted node passes every health check and goes on
	// answering confidently from a chain that stopped.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/blocks", s.handle(s.blocks))
	mux.HandleFunc("GET /api/blocks/{id}", s.handle(s.block))
	mux.HandleFunc("GET /api/txs", s.handle(s.txs))
	mux.HandleFunc("GET /api/txs/{hash}", s.handle(s.tx))
	mux.HandleFunc("GET /api/address/{addr}", s.handle(s.address))
	mux.HandleFunc("GET /api/address/{addr}/txs", s.handle(s.addressTxs))
	mux.HandleFunc("GET /api/address/{addr}/transfers", s.handle(s.addressTransfers))
	mux.HandleFunc("GET /api/tokens", s.handle(s.tokens))
	mux.HandleFunc("GET /api/tokens/names", s.handle(s.tokenNames))
	mux.HandleFunc("GET /api/tokens/{addr}", s.handle(s.token))
	mux.HandleFunc("GET /api/tokens/{addr}/holders", s.handle(s.tokenHolders))
	mux.HandleFunc("GET /api/tokens/{addr}/transfers", s.handle(s.tokenTransfers))
	mux.HandleFunc("GET /api/momentums", s.handle(s.momentums))
	mux.HandleFunc("GET /api/momentums/{height}", s.handle(s.momentum))
	mux.HandleFunc("GET /api/nom/{hash}", s.handle(s.nomMessage))
	mux.HandleFunc("GET /api/fee", s.handle(s.feeQuote))
	mux.HandleFunc("GET /api/bridge", s.handle(s.bridge))
	mux.HandleFunc("GET /api/stats", s.handle(s.stats))
	mux.HandleFunc("GET /api/fees/series", s.handle(s.feeSeries))
	mux.HandleFunc("GET /api/stats/txhistory", s.handle(s.txHistory))
	mux.HandleFunc("GET /api/search", s.handle(s.search))
	mux.HandleFunc("GET /api/contracts", s.handle(s.contracts))
	mux.HandleFunc("GET /api/ops", s.handle(s.opsReport))
	mux.HandleFunc("GET /api/contracts/verified", s.handle(s.verifiedMarks))
	mux.HandleFunc("GET /api/contracts/{addr}", s.handle(s.contractDetail))
	mux.HandleFunc("GET /api/contracts/{addr}/abi", s.handle(s.contractABI))
	mux.HandleFunc("GET /api/verify/compilers", s.handle(s.verifyCompilers))
	mux.HandleFunc("GET /api/verify/{id}", s.handle(s.verifyJob))
	mux.HandleFunc("POST /api/verify", s.handle(s.verifySubmit))
	// Etherscan's API shape, for forge and hardhat-verify.
	mux.HandleFunc("/api", s.etherscan)

	for _, ext := range s.cfg.Extensions {
		ext.Register(mux)
	}

	if s.cfg.Faucet != "" {
		mux.Handle("POST /api/faucet", s.proxy(s.cfg.Faucet, "/drip"))
		mux.Handle("GET /api/faucet", s.proxy(s.cfg.Faucet, "/status"))
	}
	mux.Handle("POST /rpc", s.denyRPC(s.ethRPC(s.proxy(s.cfg.EthURL, ""))))
	mux.Handle("POST /l1rpc", s.proxy(s.cfg.L1URL, ""))
	if s.cfg.UI != nil {
		mux.Handle("/", s.cfg.UI)
	}
	var h http.Handler = cors(mux)
	// Outermost, in reverse order so the first extension ends up nearest the
	// request: this is where the media hostname answers with stored objects
	// and never with the explorer, the API or an upload.
	for i := len(s.cfg.Extensions) - 1; i >= 0; i-- {
		if wrapped := s.cfg.Extensions[i].Wrap(h); wrapped != nil {
			h = wrapped
		}
	}
	return h
}

// l2EVMName is the execution ruleset this chain runs, for the page to show:
// "isthmus" is Prague, "karst" is Osaka with Jovian's minimum base fee (D3).
// nativeCeiling is the most gas the native lane can take in one block: a flat
// cap before the gas fork, and the elastic rule's ceiling after it (D10).
// Reporting the flat one on a forked chain said 150M where the lane could
// actually take 225M.
func nativeCeiling(cfg *rollup.Config, momentum uint64) uint64 {
	if cfg.Forks.GasActive(momentum) {
		return cfg.NativeCeil()
	}
	return cfg.MaxNativeGasPerBlock
}

// The values genesis baked in, for chains with no market fork of their own.
const (
	eip1559DenominatorCanyon = 250
	eip1559ElasticityGenesis = 6
)

// forkStatus says which rulesets this chain has scheduled and which are live,
// so a page — or a person reading /api/info — can tell one from the other.
func forkStatus(cfg *rollup.Config, momentum uint64) map[string]any {
	at := func(height uint64) map[string]any {
		if height == 0 {
			return map[string]any{"scheduled": false, "active": false}
		}
		return map[string]any{"scheduled": true, "active": momentum >= height, "momentum": height}
	}
	out := map[string]any{"gas": at(0), "wire": at(0), "market": at(cfg.FeeMarketFrom())}
	if cfg.Forks != nil {
		out["gas"], out["wire"] = at(cfg.Forks.Gas), at(cfg.Forks.Wire)
		if cfg.Forks.GasActive(momentum) {
			out["nativeFloor"], out["nativeCeil"] = cfg.NativeFloor(), cfg.NativeCeil()
		}
	}
	// The fee market the chain is actually running, so a reader can tell how
	// fast the base fee may move without knowing which schedule entry is live.
	den, el := uint32(eip1559DenominatorCanyon), uint32(eip1559ElasticityGenesis)
	if p := cfg.FeeParams(momentum); p != ([8]byte{}) {
		den = binary.BigEndian.Uint32(p[0:4])
		el = binary.BigEndian.Uint32(p[4:8])
	}
	out["eip1559"] = map[string]any{
		"denominator": den,
		"elasticity":  el,
		"targetGas":   cfg.GasLimit / uint64(el),
		// What the base fee can do to itself in one block, which is the number
		// a reader actually wants: a cap of twice the base fee is not enough
		// headroom when a block can raise it by more than that.
		"maxRisePct": float64(cfg.GasLimit-cfg.GasLimit/uint64(el)) / float64(cfg.GasLimit/uint64(el)) / float64(den) * 100,
		"maxFallPct": 100 / float64(den),
	}
	return out
}

func l2EVMName(cfg *rollup.Config) string {
	if cfg.EVM == "" {
		return rollup.EVMIsthmus
	}
	return cfg.EVM
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			// Chrome's Private Network Access: a page on a public origin asking
			// a node on localhost is a public → private request, and its
			// preflight carries this header. Without the matching answer the
			// browser blocks the request before it is ever sent, which is what
			// a statically hosted explorer pointed at a local node does on
			// every single call. The node is already Allow-Origin *, so this
			// grants nothing that was not already granted.
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				w.Header().Set("Access-Control-Allow-Private-Network", "true")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ethRPC wraps the JSON-RPC proxy so that one method can be answered here
// instead of by the execution client: `eth_getTransactionCount` for the
// **pending** tag.
//
// A wallet asks for a pending nonce before sending its next transaction. The
// execution client cannot answer it on this chain, because its pool is not this
// chain's pool — a relayed transaction goes to the relayer and from there to
// NoM, and the client never sees it until it is derived. So a wallet that sends
// a second transaction while its first is still queued gets the *same* nonce
// back, signs a duplicate, and the relayer refuses it as an underpriced
// replacement. The relayer is the only thing that knows, so ask it.
//
// Everything else passes straight through. A JSON-RPC batch that contains a
// pending-nonce lookup is split: those elements go to the relayer as a batch of
// their own, the rest to the client as another, and the two replies are merged
// back into the order the caller used. A batch without one is forwarded
// untouched, so the common case costs one parse.
func (s *Server) ethRPC(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Relayer == "" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		passThrough := func() {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			next.ServeHTTP(w, r)
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
			out, ok := s.splitBatch(ctx, r, next, trimmed)
			if !ok {
				passThrough()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}

		if !isPendingNonce(body) {
			passThrough()
			return
		}
		out, err := postJSON(ctx, s.cfg.Relayer, body)
		if err != nil {
			// The relayer is the better answer, not the only one: fall back to
			// the client rather than fail a wallet's nonce lookup.
			passThrough()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
}

// maxRPCBody bounds a JSON-RPC request through /rpc. Larger is refused, not
// cut short: a truncated body that happened to parse would be a different
// request from the one the filter was shown.
const maxRPCBody = 8 << 20

// rpcCall is one JSON-RPC request as denyRPC reads it: every value any key
// spelled like "method" holds, in order, duplicates included.
type rpcCall struct {
	id      json.RawMessage
	methods []string
	// invalid says why the element cannot be judged, or is empty.
	invalid string
}

// readRPCCall walks one request object's top-level keys itself rather than
// unmarshalling it, because the filter has to see what the execution client
// will act on, and the two read JSON differently. Go's decoder matches keys
// case-insensitively and keeps the last duplicate; op-reth reads the exact key
// "method". So {"method":"debug_traceCall","Method":"eth_chainId"} was
// eth_chainId to a struct and debug_traceCall to op-reth — found on
// the public devnet, where it reached txpool_ past -rpc-deny. Every key that
// could be the method under either reading is collected here.
func readRPCCall(raw json.RawMessage) rpcCall {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return rpcCall{invalid: "a JSON-RPC request is an object"}
	}
	var c rpcCall
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return rpcCall{invalid: "unreadable request"}
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return rpcCall{invalid: "unreadable request"}
		}
		switch {
		case strings.EqualFold(key, "method"):
			var m string
			if json.Unmarshal(value, &m) != nil {
				return rpcCall{id: c.id, invalid: "the method is not a string"}
			}
			c.methods = append(c.methods, m)
		case key == "id" && c.id == nil:
			c.id = value
		}
	}
	if _, err := dec.Token(); err != nil {
		return rpcCall{invalid: "unreadable request"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return rpcCall{id: c.id, invalid: "trailing data after the request"}
	}
	if len(c.methods) == 0 {
		c.invalid = "the request names no method"
	}
	return c
}

// denyRPC refuses JSON-RPC requests whose method starts with one of
// cfg.RPCDeny, before anything reaches the relayer or the execution client.
//
// A refused single request gets the standard "method not found" answer under
// its own id. A batch is refused as a whole, with one such answer per element:
// forwarding only the allowed elements would mean answering some of a batch
// here and the rest from upstream, and a caller that mixed in a denied method
// is not one whose other elements are worth the machinery.
//
// It fails closed. A body, or any element of a batch, that this filter cannot
// read is refused rather than passed on: the execution client may read it
// differently, and one malformed element used to make the whole batch
// unreadable here and forwarded unchecked — its other elements then ran,
// denied methods included.
func (s *Server) denyRPC(next http.Handler) http.Handler {
	if len(s.cfg.RPCDeny) == 0 {
		return next
	}
	denied := func(method string) bool {
		for _, p := range s.cfg.RPCDeny {
			if strings.HasPrefix(method, p) {
				return true
			}
		}
		return false
	}
	reply := func(id json.RawMessage, code int, msg string) map[string]any {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		return map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": msg},
		}
	}
	// judge says why one request may not pass, or "" when it may.
	judge := func(c rpcCall) (int, string) {
		if c.invalid != "" {
			return -32600, c.invalid
		}
		for _, m := range c.methods {
			if denied(m) {
				return -32601, "the method " + m + " is not available on this node"
			}
		}
		return 0, ""
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCBody+1))
		if err != nil {
			http.Error(w, "unreadable request body", http.StatusBadRequest)
			return
		}
		if len(body) > maxRPCBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		var out any
		trimmed := bytes.TrimSpace(body)
		switch {
		case len(trimmed) > 0 && trimmed[0] == '[':
			var elems []json.RawMessage
			if json.Unmarshal(trimmed, &elems) != nil || len(elems) == 0 {
				out = reply(nil, -32700, "the batch is not a JSON array of requests")
				break
			}
			calls := make([]rpcCall, len(elems))
			refused := false
			for i, e := range elems {
				calls[i] = readRPCCall(e)
				if code, _ := judge(calls[i]); code != 0 {
					refused = true
				}
			}
			if !refused {
				next.ServeHTTP(w, r)
				return
			}
			replies := make([]map[string]any, len(elems))
			for i, c := range calls {
				code, msg := judge(c)
				if code == 0 {
					code, msg = -32601, "the batch contains a request this node does not serve"
				}
				replies[i] = reply(c.id, code, msg)
			}
			out = replies
		default:
			c := readRPCCall(trimmed)
			code, msg := judge(c)
			if code == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if c.invalid != "" && c.id == nil {
				code = -32700
			}
			out = reply(c.id, code, msg)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// isPendingNonce reports whether one JSON-RPC request asks for an account's
// pending nonce.
func isPendingNonce(raw []byte) bool {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &req) != nil || req.Method != "eth_getTransactionCount" || len(req.Params) < 2 {
		return false
	}
	var tag string
	return json.Unmarshal(req.Params[1], &tag) == nil && tag == "pending"
}

// splitBatch answers a JSON-RPC batch whose pending-nonce lookups belong to the
// relayer and whose other elements belong to the client. It reports false when
// the batch has no such lookup, or when anything about it is unusual enough that
// forwarding it whole is the safer answer; the caller then passes it through.
//
// Responses are matched to requests by position within each sub-batch rather
// than by id: the spec lets a server reorder a batch's replies, but ids may
// repeat or be absent, and both servers here answer in request order. A
// notification (no id) gets no reply, as the spec requires, so a batch
// containing one is passed through rather than risk mis-pairing.
func (s *Server) splitBatch(ctx context.Context, r *http.Request, next http.Handler, body []byte) ([]byte, bool) {
	var elems []json.RawMessage
	if json.Unmarshal(body, &elems) != nil || len(elems) == 0 {
		return nil, false
	}
	var toRelayer, toClient []int
	for i, e := range elems {
		var probe struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(e, &probe) != nil || len(probe.ID) == 0 || string(probe.ID) == "null" {
			return nil, false
		}
		if isPendingNonce(e) {
			toRelayer = append(toRelayer, i)
		} else {
			toClient = append(toClient, i)
		}
	}
	if len(toRelayer) == 0 {
		return nil, false
	}

	pick := func(ix []int) []byte {
		sub := make([]json.RawMessage, len(ix))
		for j, i := range ix {
			sub[j] = elems[i]
		}
		out, _ := json.Marshal(sub)
		return out
	}
	replies := make([]json.RawMessage, len(elems))
	fill := func(ix []int, raw []byte) bool {
		var got []json.RawMessage
		if json.Unmarshal(raw, &got) != nil || len(got) != len(ix) {
			return false
		}
		for j, i := range ix {
			replies[i] = got[j]
		}
		return true
	}

	relayed, err := postJSON(ctx, s.cfg.Relayer, pick(toRelayer))
	if err != nil || !fill(toRelayer, relayed) {
		// The relayer is the better answer, not the only one.
		toClient = append(toClient, toRelayer...)
		sort.Ints(toClient)
	}
	if len(toClient) > 0 {
		sub := pick(toClient)
		rec := httptest.NewRecorder()
		req := r.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(sub))
		req.ContentLength = int64(len(sub))
		next.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !fill(toClient, rec.Body.Bytes()) {
			return nil, false
		}
	}
	out, err := json.Marshal(replies)
	return out, err == nil
}

// postJSON sends a JSON-RPC body to a service and returns its raw reply.
func postJSON(ctx context.Context, base string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", base, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// proxy forwards to another service, rewriting the path.
func (s *Server) proxy(target, path string) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bad proxy target "+target, http.StatusInternalServerError)
		})
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	director := rp.Director
	rp.Director = func(r *http.Request) {
		director(r)
		if path != "" {
			r.URL.Path = path
		} else {
			r.URL.Path = u.Path
		}
	}
	// The services behind this proxy set their own CORS headers, because they
	// are also reachable directly. Copied onto a response this server has
	// already set headers on, that yields two Access-Control-Allow-Origin
	// headers, and a browser treats a repeated one as a failed CORS check
	// rather than as agreement — so the request fails with an opaque network
	// error. This server owns CORS for everything it fronts; the upstream's
	// copy is dropped.
	rp.ModifyResponse = func(res *http.Response) error {
		for header := range res.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(header), "Access-Control-") {
				res.Header.Del(header)
			}
		}
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	return rp
}

type handlerFunc func(ctx context.Context, r *http.Request) (any, error)

func (s *Server) handle(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		v, err := fn(ctx, r)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, errBadRequest) {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

var (
	errNotFound   = errors.New("not found")
	errBadRequest = errors.New("bad request")
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func intParam(r *http.Request, name string, def, maxV int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	return min(v, maxV)
}

// --- health ---------------------------------------------------------------

// healthz answers 200 while this node is keeping up with NoM and 503 once it
// has stopped catching up. It is deliberately not an /api/ route: a load
// balancer or a container healthcheck should be able to ask it without knowing
// anything else about this service.
//
// "Stalled" is not the same as "behind". A node syncing a long history is
// behind and perfectly healthy; a node level with the frontier has nothing to
// derive and is also healthy. Stalled means both at once — NoM has momentums
// this node has not derived, and its head has not moved for StallAfter.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	st := s.cfg.Deriver.Status()
	w.Header().Set("Content-Type", "application/json")
	if st.Stalled {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              !st.Stalled,
		"stalled":         st.Stalled,
		"lagMomentums":    st.LagMomentums,
		"sinceProgressMs": st.SinceProgressMS,
		"derivedMomentum": st.DerivedHeight,
		"l1Frontier":      st.L1Frontier,
	})
}

// --- info ---------------------------------------------------------------

func (s *Server) info(ctx context.Context, _ *http.Request) (any, error) {
	st := s.cfg.Deriver.Status()
	cfg := s.cfg.Rollup
	out := map[string]any{
		"l1": map[string]any{
			"chainId":  cfg.L1ChainID,
			"frontier": st.L1Frontier,
			"rpc":      "/l1rpc",
			"inbox":    cfg.Inbox.String(),
			"vault":    cfg.Vault.String(),
			"explorer": s.cfg.L1Explorer,
		},
		"l2": map[string]any{
			"chainId":            cfg.L2ChainID,
			"rpc":                "/rpc",
			"head":               st.L2Number,
			"headHash":           st.L2Hash,
			"finalized":          st.FinalizedL2,
			"gasLimit":           cfg.GasLimit,
			"gasPerPlasma":       cfg.GasPerPlasma,
			"maxNativeGas":       cfg.MaxNativeGas,
			"maxNativeGasPerBlk": nativeCeiling(cfg, st.DerivedHeight),
			"maxTxGas":           cfg.MaxTxGas,
			"forks":              forkStatus(cfg, st.DerivedHeight),
			"evm":                l2EVMName(cfg),
			"genesisHash":        cfg.L2GenesisHash,
			"startMomentum":      cfg.L1StartHeight,
			"finalityDepth":      cfg.FinalityDepth,
		},
		"derivation": map[string]any{
			"momentum":        st.DerivedHeight,
			"momentumHash":    st.DerivedHash,
			"lag":             st.LagMomentums,
			"stalled":         st.Stalled,
			"sinceProgressMs": st.SinceProgressMS,
		},
		"predeploys": l2.Predeploys(),
	}
	if f := cfg.Fee; f != nil && f.FromHeight != 0 {
		// The native lane's price, so a page can say what a call will cost
		// without carrying NoM's emission schedule in JavaScript.
		out["l2"].(map[string]any)["fee"] = map[string]any{
			"fromHeight": f.FromHeight,
			"active":     f.Active(st.L1Frontier),
			"quote":      "/api/fee?gas=&token=&bytes=",
		}
	}
	if d := s.dex(); d != nil {
		out["uniswapV2"] = d
	}
	for _, ext := range s.cfg.Extensions {
		for k, v := range ext.Info() {
			out[k] = v
		}
	}
	if h, err := s.cfg.Eth.HeaderByNumber(ctx, nil); err == nil {
		out["l2"].(map[string]any)["baseFee"] = h.BaseFee.String()
		out["l2"].(map[string]any)["timestamp"] = h.Time
	}
	for name, base := range map[string]string{"relayer": s.cfg.Relayer, "bridge": s.cfg.Bridge, "faucet": s.cfg.Faucet} {
		if base == "" {
			continue
		}
		if v, err := fetchJSON(ctx, base+"/status"); err == nil {
			out[name] = v
		} else {
			out[name] = map[string]any{"error": err.Error()}
		}
	}
	return out, nil
}

func fetchJSON(ctx context.Context, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// --- blocks -------------------------------------------------------------

func (s *Server) blocks(_ context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 25, 200)
	before := r.URL.Query().Get("before")
	q := `SELECT b.number, b.hash, b.timestamp, b.momentum, b.txs, b.user_txs, b.gas_used, b.gas_limit, b.base_fee, b.size,
		COALESCE(m.hash, ''), COALESCE(m.messages, 0), COALESCE(m.zvm_bytes, 0), COALESCE(m.nom_blocks, 0), COALESCE(m.build_ms, 0),
		COALESCE(m.zvm_plasma, 0)
		FROM blocks b LEFT JOIN momentums m ON m.height = b.momentum`
	args := []any{}
	if before != "" {
		q += " WHERE b.number < ?"
		args = append(args, before)
	}
	q += " ORDER BY b.number DESC LIMIT ?"
	args = append(args, limit)
	return s.queryList(q, args, func(rows *sql.Rows) (any, error) {
		var number, ts, momentum, txs, userTxs, gasUsed, gasLimit, size, messages, zvmBytes, nomBlocks, buildMS, zvmPlasma int64
		var hash, baseFee, momentumHash string
		if err := rows.Scan(&number, &hash, &ts, &momentum, &txs, &userTxs, &gasUsed, &gasLimit, &baseFee, &size,
			&momentumHash, &messages, &zvmBytes, &nomBlocks, &buildMS, &zvmPlasma); err != nil {
			return nil, err
		}
		return map[string]any{
			"number": number, "hash": hash, "timestamp": ts, "momentum": momentum, "momentumHash": momentumHash,
			"txs": txs, "userTxs": userTxs, "gasUsed": gasUsed, "gasLimit": gasLimit, "baseFee": baseFee, "size": size,
			"messages": messages, "zvmBytes": zvmBytes, "nomBlocks": nomBlocks, "buildMs": buildMS, "zvmPlasma": zvmPlasma,
		}, nil
	})
}

func (s *Server) block(ctx context.Context, r *http.Request) (any, error) {
	// Hashes are stored lower case; one pasted from elsewhere may not be.
	id := strings.ToLower(r.PathValue("id"))
	where, arg := "number = ?", any(id)
	if strings.HasPrefix(id, "0x") {
		where = "hash = ?"
	} else if n, err := strconv.ParseUint(id, 10, 64); err == nil {
		arg = n
	} else {
		return nil, fmt.Errorf("%w: %q is not a block number or hash", errBadRequest, id)
	}
	var number, ts, momentum, txs, userTxs, gasUsed, gasLimit, size int64
	var hash, parent, baseFee string
	err := s.db.QueryRow(`SELECT number, hash, parent, timestamp, momentum, txs, user_txs, gas_used, gas_limit, base_fee, size
		FROM blocks WHERE `+where, arg).Scan(&number, &hash, &parent, &ts, &momentum, &txs, &userTxs, &gasUsed, &gasLimit, &baseFee, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	txList, err := s.queryList(`SELECT hash, idx, type, lane, nom_hash, from_addr, to_addr, value, gas_used, status, selector, contract
		FROM txs WHERE block = ? ORDER BY idx`, []any{number}, scanTxRow)
	if err != nil {
		return nil, err
	}
	m, _ := s.momentumRow(momentum)
	return map[string]any{
		"number": number, "hash": hash, "parent": parent, "timestamp": ts, "momentum": momentum,
		"txs": txs, "userTxs": userTxs, "gasUsed": gasUsed, "gasLimit": gasLimit, "baseFee": baseFee, "size": size,
		"transactions": txList, "momentumDetail": m,
	}, nil
}

func scanTxRow(rows *sql.Rows) (any, error) {
	var hash, lane, from string
	var nomHash, to, selector, contract sql.NullString
	var idx, txType, gasUsed, status int64
	var value string
	if err := rows.Scan(&hash, &idx, &txType, &lane, &nomHash, &from, &to, &value, &gasUsed, &status, &selector, &contract); err != nil {
		return nil, err
	}
	return map[string]any{
		"hash": hash, "index": idx, "type": txType, "lane": lane, "nomHash": nullStr(nomHash), "from": from,
		"to": nullStr(to), "value": value, "gasUsed": gasUsed, "status": status,
		"selector": nullStr(selector), "contract": nullStr(contract),
	}, nil
}

// scanAddrTxRow is scanTxRow for the address listing, whose query carries the
// block number as a thirteenth column so the three arms can be merged in one
// order.
func scanAddrTxRow(rows *sql.Rows) (any, error) {
	var hash, lane, from string
	var nomHash, to, selector, contract sql.NullString
	var idx, txType, gasUsed, status, block int64
	var value string
	if err := rows.Scan(&hash, &idx, &txType, &lane, &nomHash, &from, &to, &value, &gasUsed, &status,
		&selector, &contract, &block); err != nil {
		return nil, err
	}
	return map[string]any{
		"hash": hash, "index": idx, "type": txType, "lane": lane, "nomHash": nullStr(nomHash), "from": from,
		"to": nullStr(to), "value": value, "gasUsed": gasUsed, "status": status,
		"selector": nullStr(selector), "contract": nullStr(contract), "block": block,
	}, nil
}

func nullStr(s sql.NullString) any {
	if !s.Valid {
		return nil
	}
	return s.String
}

// --- transactions -------------------------------------------------------

func (s *Server) txs(_ context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 25, 200)
	q := `SELECT hash, idx, type, lane, nom_hash, from_addr, to_addr, value, gas_used, status, selector, contract
		FROM txs WHERE 1=1`
	args := []any{}
	if lane := r.URL.Query().Get("lane"); lane != "" {
		q += " AND lane = ?"
		args = append(args, lane)
	}
	if r.URL.Query().Get("users") == "1" {
		q += " AND lane != 'system'"
	}
	if before := r.URL.Query().Get("before"); before != "" {
		q += " AND block < ?"
		args = append(args, before)
	}
	q += " ORDER BY block DESC, idx DESC LIMIT ?"
	args = append(args, limit)
	return s.queryList(q, args, scanTxRow)
}

func (s *Server) tx(ctx context.Context, r *http.Request) (any, error) {
	hash := strings.ToLower(r.PathValue("hash"))
	var out map[string]any
	var block, idx, ts, txType, gas, gasUsed, nonce, status, inputSize int64
	var lane, from, value, gasPrice, fee string
	var nomHash, to, mint, selector, contract sql.NullString
	err := s.db.QueryRow(`SELECT block, idx, timestamp, type, lane, nom_hash, from_addr, to_addr, value, mint, gas, gas_used,
		gas_price, fee, nonce, status, input_size, selector, contract FROM txs WHERE hash = ?`, hash).
		Scan(&block, &idx, &ts, &txType, &lane, &nomHash, &from, &to, &value, &mint, &gas, &gasUsed, &gasPrice, &fee,
			&nonce, &status, &inputSize, &selector, &contract)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	out = map[string]any{
		"hash": hash, "block": block, "index": idx, "timestamp": ts, "type": txType, "lane": lane,
		"nomHash": nullStr(nomHash), "from": from, "to": nullStr(to), "value": value, "mint": nullStr(mint),
		"gas": gas, "gasUsed": gasUsed, "gasPrice": gasPrice, "fee": fee, "nonce": nonce, "status": status,
		"inputSize": inputSize, "selector": nullStr(selector), "contract": nullStr(contract),
	}
	if logs, err := s.queryList(`SELECT idx, address, topic0, topic1, topic2, topic3, data FROM logs WHERE tx = ? ORDER BY idx`,
		[]any{hash}, func(rows *sql.Rows) (any, error) {
			var idx int64
			var address string
			var t0, t1, t2, t3 sql.NullString
			var data []byte
			if err := rows.Scan(&idx, &address, &t0, &t1, &t2, &t3, &data); err != nil {
				return nil, err
			}
			topics := []any{}
			for _, t := range []sql.NullString{t0, t1, t2, t3} {
				if t.Valid {
					topics = append(topics, t.String)
				}
			}
			return map[string]any{"index": idx, "address": address, "topics": topics, "data": "0x" + common.Bytes2Hex(data)}, nil
		}); err == nil {
		out["logs"] = logs
	}
	if tt, err := s.queryList(`SELECT t.idx, t.token, t.from_addr, t.to_addr, t.value, t.token_id,
		COALESCE(k.symbol,''), COALESCE(k.decimals,0), COALESCE(k.kind,'')
		FROM token_transfers t LEFT JOIN tokens k ON k.address = t.token WHERE t.tx = ? ORDER BY t.idx`,
		[]any{hash}, scanTransferRow); err == nil {
		out["tokenTransfers"] = tt
	}
	if nomHash.Valid {
		if msg, err := s.messageRow(nomHash.String); err == nil {
			out["nomMessage"] = msg
		}
	}
	if tx, _, err := s.cfg.Eth.TransactionByHash(ctx, common.HexToHash(hash)); err == nil {
		out["input"] = "0x" + common.Bytes2Hex(tx.Data())
	}
	return out, nil
}

func scanTransferRow(rows *sql.Rows) (any, error) {
	var idx, decimals int64
	var token, from, to, symbol, kind string
	var value, tokenID sql.NullString
	if err := rows.Scan(&idx, &token, &from, &to, &value, &tokenID, &symbol, &decimals, &kind); err != nil {
		return nil, err
	}
	return map[string]any{
		"index": idx, "token": token, "from": from, "to": to, "value": nullStr(value), "tokenId": nullStr(tokenID),
		"symbol": symbol, "decimals": decimals, "kind": kind,
	}, nil
}

// --- addresses ----------------------------------------------------------

func parseAddress(s string) (common.Address, error) { return ParseAddress(s) }

func (s *Server) address(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	nom := znn.Address(addr)
	out := map[string]any{"address": addr.Hex(), "nomAddress": nom.String(), "isNomUser": nom.IsUser()}
	if bal, err := s.cfg.Eth.BalanceAt(ctx, addr, nil); err == nil {
		out["balance"] = bal.String()
	}
	if nonce, err := s.cfg.Eth.NonceAt(ctx, addr, nil); err == nil {
		out["nonce"] = nonce
	}
	if code, err := s.cfg.Eth.CodeAt(ctx, addr, nil); err == nil && len(code) > 0 {
		out["codeSize"] = len(code)
		var creator, tx string
		var block int64
		if err := s.db.QueryRow("SELECT creator, tx, block FROM contracts WHERE address = ?", addr.Hex()).
			Scan(&creator, &tx, &block); err == nil {
			out["contract"] = map[string]any{"creator": creator, "tx": tx, "block": block}
		}
	}
	if _, ok := out["codeSize"]; ok {
		var kind string
		if err := s.db.QueryRowContext(ctx, "SELECT kind FROM tokens WHERE address = ?", addr.Hex()).Scan(&kind); err != nil {
			kind = s.nftStandard(ctx, addr)
		}
		if kind != "" {
			out["standard"] = kind
		}
	}
	for name, a := range l2.Predeploys() {
		if a == addr {
			out["predeploy"] = name
		}
	}
	var txCount int64
	s.db.QueryRow("SELECT txs FROM addr_stats WHERE addr = ?", addr.Hex()).Scan(&txCount)
	out["txCount"] = txCount
	if tokens, err := s.addressTokens(ctx, addr); err == nil {
		out["tokens"] = tokens
	}
	if nom.IsUser() {
		l1 := map[string]any{}
		if info, err := s.cfg.L1.AccountInfo(ctx, nom); err == nil {
			l1["znn"] = info.Balance(znn.ZNN).String()
			l1["qsr"] = info.Balance(znn.QSR).String()
			l1["height"] = info.AccountHeight
		}
		if p, err := s.cfg.L1.PlasmaInfo(ctx, nom); err == nil {
			l1["plasma"] = p.CurrentPlasma
			l1["maxPlasma"] = p.MaxPlasma
			l1["fusedQsr"] = p.QsrAmount.Big().String()
		}
		if list, err := s.cfg.L1.UnreceivedBlocks(ctx, nom, 0, 10); err == nil {
			l1["unreceived"] = list.Count
		}
		out["l1"] = l1
	}
	return out, nil
}

// nftStandard asks a contract over ERC-165 whether it is an ERC-721 or an
// ERC-1155, and answers "" when it is neither or cannot say.
func (s *Server) nftStandard(ctx context.Context, addr common.Address) string {
	for _, c := range []struct{ kind, id string }{{"erc721", "80ac58cd"}, {"erc1155", "d9b67a26"}} {
		data := common.FromHex("0x01ffc9a7" + c.id + strings.Repeat("0", 56))
		out, err := s.cfg.Eth.CallContract(ctx, ethCall(addr, data), nil)
		if err == nil && len(out) == 32 && out[31] == 1 {
			return c.kind
		}
	}
	return ""
}

// callText calls a no-argument view returning a string, such as name().
func (s *Server) callText(ctx context.Context, addr common.Address, selector string) string {
	out, err := s.cfg.Eth.CallContract(ctx, ethCall(addr, common.FromHex(selector)), nil)
	if err != nil || len(out) < 64 {
		return ""
	}
	n := new(big.Int).SetBytes(out[32:64])
	if !n.IsUint64() || 64+n.Uint64() > uint64(len(out)) {
		return ""
	}
	return string(out[64 : 64+n.Uint64()])
}

// addressTokens lists every token this address has ever moved, with what it
// holds of each now. ERC-20 and ERC-721 balances are read from the contract;
// ERC-1155 has no per-owner total, so it is replayed from this address's own
// transfers, as is an ERC-721 whose balanceOf fails. Held tokens come first,
// fungible before collectibles, so a busy account's holdings are not buried
// under everything it once touched.
func (s *Server) addressTokens(ctx context.Context, addr common.Address) ([]map[string]any, error) {
	const maxTokens = 1000
	rows, err := s.db.QueryContext(ctx, `SELECT u.token, COALESCE(k.symbol,''), COALESCE(k.name,''), COALESCE(k.decimals,0), COALESCE(k.kind,'')
		FROM (SELECT token FROM token_transfers WHERE from_addr = ? UNION SELECT token FROM token_transfers WHERE to_addr = ?) u
		LEFT JOIN tokens k ON k.address = u.token LIMIT ?`, addr.Hex(), addr.Hex(), maxTokens)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		var token, symbol, name, kind string
		var decimals int64
		if err := rows.Scan(&token, &symbol, &name, &decimals, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, map[string]any{"token": token, "symbol": symbol, "name": name, "decimals": decimals, "kind": kind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, entry := range out {
		wg.Add(1)
		sem <- struct{}{}
		go func(entry map[string]any) {
			defer func() { <-sem; wg.Done() }()
			token := entry["token"].(string)
			var bal *big.Int
			ids := 0
			switch entry["kind"] {
			case "erc20":
				bal, _ = s.erc20Balance(ctx, common.HexToAddress(token), addr)
			case "erc721":
				if b, err := s.erc20Balance(ctx, common.HexToAddress(token), addr); err == nil {
					bal, ids = b, int(b.Int64())
				} else if h, err := s.ownHoldings(ctx, "erc721", token, addr); err == nil {
					bal, ids = h.quantity, h.ids
				}
			case "erc1155":
				if h, err := s.ownHoldings(ctx, "erc1155", token, addr); err == nil {
					bal, ids = h.quantity, h.ids
				}
			}
			if bal != nil {
				entry["balance"] = bal.String()
				entry["held"] = bal.Sign() > 0
			}
			if ids > 0 {
				entry["ids"] = ids
			}
		}(entry)
	}
	wg.Wait()

	rank := func(e map[string]any) int {
		held, _ := e["held"].(bool)
		r := 0
		if !held {
			r += 10
		}
		if e["kind"] != "erc20" {
			r++
		}
		return r
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		return strings.ToLower(out[i]["symbol"].(string)) < strings.ToLower(out[j]["symbol"].(string))
	})
	return out, nil
}

type ownHolding struct {
	quantity *big.Int
	ids      int
}

// ownHoldings replays one address's transfers of one NFT contract. Only the
// transfers that name the address matter: for an ERC-721 id the latest one
// says whether it still owns it, and ERC-1155 amounts simply add and subtract.
func (s *Server) ownHoldings(ctx context.Context, kind, token string, addr common.Address) (*ownHolding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT from_addr, to_addr, COALESCE(value,''), COALESCE(token_id,'') FROM token_transfers
		WHERE token = ? AND (from_addr = ? OR to_addr = ?) ORDER BY block, idx`, token, addr.Hex(), addr.Hex())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	me := addr.Hex()
	owned := map[string]bool{}
	amounts := map[string]*big.Int{}
	for rows.Next() {
		var from, to, value, id string
		if err := rows.Scan(&from, &to, &value, &id); err != nil {
			return nil, err
		}
		if kind == "erc721" {
			owned[id] = to == me
			continue
		}
		v, ok := new(big.Int).SetString(value, 10)
		if !ok {
			continue
		}
		if amounts[id] == nil {
			amounts[id] = new(big.Int)
		}
		if from == me {
			amounts[id].Sub(amounts[id], v)
		}
		if to == me {
			amounts[id].Add(amounts[id], v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	h := &ownHolding{quantity: new(big.Int)}
	for _, yes := range owned {
		if yes {
			h.ids++
		}
	}
	h.quantity.SetInt64(int64(h.ids))
	for _, v := range amounts {
		if v.Sign() > 0 {
			h.quantity.Add(h.quantity, v)
			h.ids++
		}
	}
	return h, nil
}

func (s *Server) erc20Balance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(holder.Bytes(), 32)...)
	out, err := s.cfg.Eth.CallContract(ctx, ethCall(token, data), nil)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

func (s *Server) addressTxs(_ context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	limit := intParam(r, "limit", 25, 200)
	// One indexed lookup per side instead of a row per address per
	// transaction (D21): `txs` already holds who sent it, who received it and
	// what it created, and three indexes make each side a range scan. Each arm
	// is bounded by the page size before they are merged, so a busy address
	// costs the same as a quiet one.
	before := int64(math.MaxInt64)
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: before must be a block number", errBadRequest)
		}
		before = n
	}
	const cols = `t.hash, t.idx, t.type, t.lane, t.nom_hash, t.from_addr, t.to_addr, t.value, t.gas_used, t.status, t.selector, t.contract, t.block`
	arm := func(where string) string {
		return `SELECT * FROM (SELECT ` + cols + ` FROM txs t WHERE ` + where +
			` AND t.block < ? ORDER BY t.block DESC, t.idx DESC LIMIT ?)`
	}
	q := arm("t.from_addr = ?") + " UNION " + arm("t.to_addr = ?") + " UNION " + arm("t.contract = ?") +
		" ORDER BY 13 DESC, 2 DESC LIMIT ?"
	args := []any{
		addr.Hex(), before, limit,
		addr.Hex(), before, limit,
		addr.Hex(), before, limit,
		limit,
	}
	return s.queryList(q, args, scanAddrTxRow)
}

func (s *Server) addressTransfers(_ context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	limit := intParam(r, "limit", 25, 200)
	return s.queryList(`SELECT t.idx, t.token, t.from_addr, t.to_addr, t.value, t.token_id,
		COALESCE(k.symbol,''), COALESCE(k.decimals,0), COALESCE(k.kind,'')
		FROM token_transfers t LEFT JOIN tokens k ON k.address = t.token
		WHERE t.from_addr = ? OR t.to_addr = ? ORDER BY t.block DESC, t.idx DESC LIMIT ?`,
		[]any{addr.Hex(), addr.Hex(), limit}, scanTransferRow)
}

// --- tokens -------------------------------------------------------------

// tokens lists the indexed tokens with how many addresses hold each now,
// busiest first or, with sort=holders, most held first. A holder count is a
// replay of the token's transfers (holdersOf), too dear to run for every token
// when only the top of the list is wanted, and it does not have to be: every
// holder received at least one transfer, so a token has no more holders than
// transfers. Walking tokens busiest first, the walk stops as soon as the
// limit-th best count is at least the next token's transfers.
func (s *Server) tokens(ctx context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 50, 200)
	byHolders := r.URL.Query().Get("sort") == "holders"
	q, args := `SELECT address, kind, COALESCE(name,''), COALESCE(symbol,''), COALESCE(decimals,0), first_block, transfers
		FROM tokens ORDER BY transfers DESC, address`, []any{}
	if !byHolders {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	type tokenRow struct {
		Address    string `json:"address"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Symbol     string `json:"symbol"`
		Decimals   int64  `json:"decimals"`
		FirstBlock int64  `json:"firstBlock"`
		Transfers  int64  `json:"transfers"`
		Holders    int64  `json:"holders"`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var all []tokenRow
	for rows.Next() {
		var t tokenRow
		if err := rows.Scan(&t.Address, &t.Kind, &t.Name, &t.Symbol, &t.Decimals, &t.FirstBlock, &t.Transfers); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Ties go to the busier token, which is the order the walk visits them in,
	// so an unvisited token can never outrank a visited one.
	rank := func(a, b tokenRow) bool {
		if a.Holders != b.Holders {
			return a.Holders > b.Holders
		}
		return a.Transfers > b.Transfers
	}
	out := make([]tokenRow, 0, min(len(all), limit))
	for _, t := range all {
		if byHolders && len(out) >= limit && out[limit-1].Holders >= t.Transfers {
			break
		}
		h, err := s.holdersOf(ctx, t.Address, t.Kind)
		if err != nil {
			return nil, err
		}
		t.Holders = int64(len(h.rows))
		out = append(out, t)
		if byHolders {
			sort.SliceStable(out, func(i, j int) bool { return rank(out[i], out[j]) })
			if len(out) > limit {
				out = out[:limit]
			}
		}
	}
	return out, nil
}

func (s *Server) token(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	var kind, name, symbol string
	var decimals, firstBlock, transfers int64
	err = s.db.QueryRow(`SELECT kind, COALESCE(name,''), COALESCE(symbol,''), COALESCE(decimals,0), first_block, transfers
		FROM tokens WHERE address = ?`, addr.Hex()).Scan(&kind, &name, &symbol, &decimals, &firstBlock, &transfers)
	if errors.Is(err, sql.ErrNoRows) {
		// The indexer learns a token from its first transfer, so a collection
		// nobody has minted from yet is not in the table. ERC-165 still says
		// what it is.
		kind = s.nftStandard(ctx, addr)
		if kind == "" {
			return nil, errNotFound
		}
		name, symbol = s.callText(ctx, addr, "0x06fdde03"), s.callText(ctx, addr, "0x95d89b41")
		err = nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"address": addr.Hex(), "nomAddress": znn.Address(addr).String(), "kind": kind,
		"name": name, "symbol": symbol, "decimals": decimals, "firstBlock": firstBlock, "transfers": transfers}
	if kind == "erc20" {
		if supply, err := s.cfg.Eth.CallContract(ctx, ethCall(addr, common.FromHex("0x18160ddd")), nil); err == nil && len(supply) == 32 {
			out["totalSupply"] = new(big.Int).SetBytes(supply).String()
		}
	}
	recent, err := s.queryList(`SELECT t.idx, t.token, t.from_addr, t.to_addr, t.value, t.token_id,
		COALESCE(k.symbol,''), COALESCE(k.decimals,0), COALESCE(k.kind,'')
		FROM token_transfers t LEFT JOIN tokens k ON k.address = t.token
		WHERE t.token = ? ORDER BY t.block DESC, t.idx DESC LIMIT 25`, []any{addr.Hex()}, scanTransferRow)
	if err != nil {
		return nil, err
	}
	out["recentTransfers"] = recent
	return out, nil
}

// gasForkHeight is the momentum the fee split starts at, or a height past any
// real chain when the fork is not scheduled — so "before the fork" covers
// everything, which is the truth on a chain that never splits.
func gasForkHeight(cfg *rollup.Config) uint64 {
	if cfg == nil || cfg.Forks == nil || cfg.Forks.Gas == 0 {
		return math.MaxUint64
	}
	return cfg.Forks.Gas
}

// feeSeries is every fee this chain has taken, running totals, by token and by
// where it goes. Four of the five are ZNN and one is QSR, so the caller gets
// whole token units rather than base units: cumulative wei passes 2^53 and
// would lose precision as a JSON number long before it stopped mattering.
//
// The five are not the same kind of thing, which is the point of separating
// them:
//
//   - burned by native calls, in ZNN and in QSR — this leaves NoM's supply,
//     because the inbox can never spend it (§6);
//   - the base fee, which accrues to the L2 fee vault and is swept by nobody;
//   - the relayer's half of the priority fee, held by the splitter until a
//     relayer withdraws it;
//   - the burned half, which goes to 0x…dEaD on the ZVM and is owed a mirror
//     on NoM (§7).
func (s *Server) feeSeries(_ context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 150, 2000)

	// Deposits pay nothing, so they are excluded from the gas the base fee is
	// charged on; `fee` already stores zero for them.
	const perBlock = `SELECT b.number, b.timestamp, b.momentum, b.base_fee,
			COALESCE(SUM(CASE WHEN t.type != 126 THEN t.gas_used ELSE 0 END),0),
			COALESCE(SUM(CAST(t.fee AS REAL)),0)
		FROM blocks b LEFT JOIN txs t ON t.block = b.number
		WHERE b.number > ? GROUP BY b.number ORDER BY b.number`

	var head int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(number),0) FROM blocks`).Scan(&head); err != nil {
		return nil, err
	}
	from := max(head-int64(limit), 0)

	// Everything before the window, so the curve starts at the right height
	// rather than at zero.
	// Tips are split only from the gas fork (D9); before it the whole tip went
	// to the sequencer fee vault undivided, so attributing it to relayers would
	// overstate what they are owed — by exactly half the vault's balance.
	split := func(momentum int64) bool { return s.cfg.Rollup.Forks.GasActive(uint64(momentum)) }

	var base, tip, seq, znnBurn, qsrBurn float64
	var gas, fee sql.NullFloat64
	var preBase, preSplitTip, preSeqTip sql.NullFloat64
	s.db.QueryRow(`SELECT
		COALESCE(SUM(CAST(b.base_fee AS REAL) * x.gas),0),
		COALESCE(SUM(CASE WHEN b.momentum >= ? THEN x.fee - CAST(b.base_fee AS REAL) * x.gas END),0),
		COALESCE(SUM(CASE WHEN b.momentum <  ? THEN x.fee - CAST(b.base_fee AS REAL) * x.gas END),0)
		FROM blocks b
		JOIN (SELECT block, SUM(CASE WHEN type != 126 THEN gas_used ELSE 0 END) AS gas, SUM(CAST(fee AS REAL)) AS fee
		      FROM txs GROUP BY block) x ON x.block = b.number
		WHERE b.number <= ?`, gasForkHeight(s.cfg.Rollup), gasForkHeight(s.cfg.Rollup), from).Scan(&preBase, &preSplitTip, &preSeqTip)
	base, tip, seq = preBase.Float64/1e18, preSplitTip.Float64/1e18, preSeqTip.Float64/1e18
	s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN burn_token = ? THEN CAST(burned AS REAL) END),0),
		COALESCE(SUM(CASE WHEN burn_token = ? THEN CAST(burned AS REAL) END),0)
		FROM messages WHERE momentum <= (SELECT COALESCE(MAX(momentum),0) FROM blocks WHERE number <= ?)`,
		znn.ZNN.String(), znn.QSR.String(), from).Scan(&znnBurn, &qsrBurn)
	znnBurn, qsrBurn = znnBurn/1e8, qsrBurn/1e8

	// Native burns in the window, by the momentum each block derives from.
	burns := map[int64][2]float64{}
	rows, err := s.db.Query(`SELECT b.number,
			COALESCE(SUM(CASE WHEN m.burn_token = ? THEN CAST(m.burned AS REAL) END),0),
			COALESCE(SUM(CASE WHEN m.burn_token = ? THEN CAST(m.burned AS REAL) END),0)
		FROM blocks b JOIN messages m ON m.momentum = b.momentum
		WHERE b.number > ? GROUP BY b.number`, znn.ZNN.String(), znn.QSR.String(), from)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n int64
		var z, q float64
		if err := rows.Scan(&n, &z, &q); err != nil {
			rows.Close()
			return nil, err
		}
		burns[n] = [2]float64{z / 1e8, q / 1e8}
	}
	rows.Close()

	out, err := s.queryList(perBlock, []any{from}, func(rows *sql.Rows) (any, error) {
		var number, ts, momentum int64
		var baseFeeStr string
		if err := rows.Scan(&number, &ts, &momentum, &baseFeeStr, &gas, &fee); err != nil {
			return nil, err
		}
		bfWei, _ := new(big.Float).SetString(baseFeeStr)
		if bfWei == nil {
			bfWei = new(big.Float)
		}
		blockBase, _ := new(big.Float).Mul(bfWei, big.NewFloat(gas.Float64)).Float64()
		base += blockBase / 1e18
		if t := (fee.Float64 - blockBase) / 1e18; split(momentum) {
			tip += t
		} else {
			seq += t
		}
		if b, ok := burns[number]; ok {
			znnBurn, qsrBurn = znnBurn+b[0], qsrBurn+b[1]
		}
		return map[string]any{
			"number": number, "timestamp": ts,
			"znnBurnedNative": znnBurn, "qsrBurnedNative": qsrBurn,
			"baseFee": base, "relayer": tip / 2, "burnedL2": tip / 2, "sequencer": seq,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"series": out, "totals": map[string]any{
		"znnBurnedNative": znnBurn, "qsrBurnedNative": qsrBurn,
		"baseFee": base, "relayer": tip / 2, "burnedL2": tip / 2, "sequencer": seq,
	}}, nil
}

// txHistory answers the total-transactions chart: cumulative counts over a
// window of wall-clock time, rather than the last N blocks — the two answer
// different questions on a chain whose block time can drift, and drawing 24h
// of raw per-block rows would ship a point for every ~15s momentum. Buckets
// keep the response small no matter how busy the window was.
func (s *Server) txHistory(ctx context.Context, r *http.Request) (any, error) {
	hours := intParam(r, "hours", 24, 24*30)
	buckets := intParam(r, "buckets", 288, 1000)
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	bucketSize := int64(hours) * 3600 / int64(buckets)
	if bucketSize < 1 {
		bucketSize = 1
	}

	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(user_txs),0) FROM blocks").Scan(&total); err != nil {
		return nil, err
	}
	var windowTxs int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(user_txs),0) FROM blocks WHERE timestamp >= ?", since).Scan(&windowTxs); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT (timestamp - ?) / ? AS bucket, SUM(user_txs)
		FROM blocks WHERE timestamp >= ? GROUP BY bucket ORDER BY bucket`, since, bucketSize, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	series := []map[string]any{}
	running := total - windowTxs
	for rows.Next() {
		var bucket, txs int64
		if err := rows.Scan(&bucket, &txs); err != nil {
			return nil, err
		}
		running += txs
		// Each point sits at the start of the interval it summarizes, not
		// wherever its blocks happened to land within it.
		series = append(series, map[string]any{"timestamp": since + bucket*bucketSize, "txs": txs, "total": running})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"series": series, "total": total, "windowTxs": windowTxs, "since": since}, nil
}

// --- NoM side -----------------------------------------------------------

func (s *Server) momentums(_ context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 25, 200)
	q := `SELECT height, hash, timestamp, l2_number, l2_hash, txs, messages, dropped, nom_blocks, zvm_bytes, zvm_plasma, gas_used, build_ms, attempts FROM momentums`
	args := []any{}
	if before := r.URL.Query().Get("before"); before != "" {
		q += " WHERE height < ?"
		args = append(args, before)
	}
	q += " ORDER BY height DESC LIMIT ?"
	args = append(args, limit)
	return s.queryList(q, args, scanMomentumRow)
}

func scanMomentumRow(rows *sql.Rows) (any, error) {
	var height, ts, l2Number, txs, messages, dropped, nomBlocks, zvmBytes, zvmPlasma, gasUsed, buildMS, attempts int64
	var hash, l2Hash string
	if err := rows.Scan(&height, &hash, &ts, &l2Number, &l2Hash, &txs, &messages, &dropped, &nomBlocks, &zvmBytes, &zvmPlasma, &gasUsed, &buildMS, &attempts); err != nil {
		return nil, err
	}
	return map[string]any{
		"height": height, "hash": hash, "timestamp": ts, "l2Number": l2Number, "l2Hash": l2Hash,
		"txs": txs, "messages": messages, "dropped": dropped, "nomBlocks": nomBlocks,
		"zvmBytes": zvmBytes, "zvmPlasma": zvmPlasma, "gasUsed": gasUsed, "buildMs": buildMS, "attempts": attempts,
	}, nil
}

func (s *Server) momentumRow(height int64) (any, error) {
	rows, err := s.db.Query(`SELECT height, hash, timestamp, l2_number, l2_hash, txs, messages, dropped, nom_blocks, zvm_bytes, zvm_plasma, gas_used, build_ms, attempts
		FROM momentums WHERE height = ?`, height)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, errNotFound
	}
	return scanMomentumRow(rows)
}

func (s *Server) momentum(_ context.Context, r *http.Request) (any, error) {
	height, err := strconv.ParseInt(r.PathValue("height"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: momentum height", errBadRequest)
	}
	m, err := s.momentumRow(height)
	if err != nil {
		return nil, err
	}
	msgs, err := s.queryList(`SELECT nom_hash, momentum, idx, author, kind, status, COALESCE(reason,''), bytes, plasma,
		COALESCE(token,''), COALESCE(amount,''), COALESCE(txs,'[]'), COALESCE(dropped,'[]'),
		COALESCE(burned,''), COALESCE(burn_token,''), COALESCE(fee_gas,0), COALESCE(fee_price,0), COALESCE(boost,''), COALESCE(boost_gas,0), COALESCE(deferred_from,0)
		FROM messages WHERE momentum = ? ORDER BY idx`, []any{height}, scanMessageRow)
	if err != nil {
		return nil, err
	}
	return map[string]any{"momentum": m, "messages": msgs}, nil
}

func scanMessageRow(rows *sql.Rows) (any, error) {
	var nomHash, author, kind, status, reason, token, amount, txsJSON, droppedJSON, burned, burnToken, boost string
	var momentum, idx, bytes, plasma, feeGas, feePrice, boostGas, deferredFrom int64
	if err := rows.Scan(&nomHash, &momentum, &idx, &author, &kind, &status, &reason, &bytes, &plasma, &token, &amount,
		&txsJSON, &droppedJSON, &burned, &burnToken, &feeGas, &feePrice, &boost, &boostGas, &deferredFrom); err != nil {
		return nil, err
	}
	var txs, dropped any
	json.Unmarshal([]byte(txsJSON), &txs)
	json.Unmarshal([]byte(droppedJSON), &dropped)
	out := map[string]any{
		"nomHash": nomHash, "momentum": momentum, "index": idx, "author": author, "kind": kind, "status": status,
		"reason": reason, "bytes": bytes, "plasma": plasma, "token": token, "amount": amount,
		"txs": txs, "dropped": dropped,
	}
	if burned != "" {
		out["burned"], out["burnToken"], out["feeGas"], out["feePrice"] = burned, burnToken, feeGas, feePrice
	}
	// The part of the burn that bought admission priority rather than gas, and
	// what it is worth in gas: the number the native lane is ordered by.
	if boost != "" && boost != "0" {
		out["boost"], out["boostGas"] = boost, boostGas
	}
	// A deposit or payout a full block carried forward (docs/HALT.md, path 6):
	// the momentum it arrived in, where this row's momentum is where it applied.
	if deferredFrom != 0 {
		out["deferredFrom"] = deferredFrom
	}
	return out, nil
}

func (s *Server) nomMessage(_ context.Context, r *http.Request) (any, error) {
	msg, err := s.messageRow(strings.TrimPrefix(r.PathValue("hash"), "0x"))
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func (s *Server) messageRow(hash string) (any, error) {
	rows, err := s.db.Query(`SELECT nom_hash, momentum, idx, author, kind, status, COALESCE(reason,''), bytes, plasma,
		COALESCE(token,''), COALESCE(amount,''), COALESCE(txs,'[]'), COALESCE(dropped,'[]'),
		COALESCE(burned,''), COALESCE(burn_token,''), COALESCE(fee_gas,0), COALESCE(fee_price,0), COALESCE(boost,''), COALESCE(boost_gas,0), COALESCE(deferred_from,0) FROM messages WHERE nom_hash = ?`, hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, errNotFound
	}
	return scanMessageRow(rows)
}

// --- bridge -------------------------------------------------------------

func (s *Server) bridge(ctx context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 25, 200)
	flows, err := s.queryList(`SELECT id, kind, wid, block, timestamp, l2_tx, COALESCE(account,''), COALESCE(token,''),
		COALESCE(amount,''), COALESCE(nom_block,''), COALESCE(note,'') FROM bridge_flows ORDER BY id DESC LIMIT ?`,
		[]any{limit}, func(rows *sql.Rows) (any, error) {
			var id, block, ts int64
			var wid sql.NullInt64
			var kind, l2tx, account, token, amount, nomBlock, note string
			if err := rows.Scan(&id, &kind, &wid, &block, &ts, &l2tx, &account, &token, &amount, &nomBlock, &note); err != nil {
				return nil, err
			}
			out := map[string]any{"id": id, "kind": kind, "block": block, "timestamp": ts, "l2Tx": l2tx,
				"account": account, "token": token, "amount": amount, "nomBlock": nomBlock, "note": note}
			if wid.Valid {
				out["withdrawalId"] = wid.Int64
			}
			return out, nil
		})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"flows": flows}
	// Deposits come from the derivation store, not from a contract event.
	deposits, err := s.queryList(`SELECT nom_hash, momentum, idx, author, kind, status, COALESCE(reason,''), bytes, plasma,
		COALESCE(token,''), COALESCE(amount,''), COALESCE(txs,'[]'), COALESCE(dropped,'[]'),
		COALESCE(burned,''), COALESCE(burn_token,''), COALESCE(fee_gas,0), COALESCE(fee_price,0), COALESCE(boost,''), COALESCE(boost_gas,0), COALESCE(deferred_from,0)
		FROM messages WHERE kind = 'deposit' ORDER BY momentum DESC LIMIT ?`, []any{limit}, scanMessageRow)
	if err != nil {
		return nil, err
	}
	out["deposits"] = deposits
	if s.cfg.Bridge != "" {
		if v, err := fetchJSON(ctx, s.cfg.Bridge+"/status"); err == nil {
			out["committee"] = v
		}
	}
	if bal, err := s.cfg.Eth.BalanceAt(ctx, rollup.BridgeAddr, nil); err == nil {
		out["lockedOnZVM"] = bal.String()
	}
	if info, err := s.cfg.L1.AccountInfo(ctx, s.cfg.Rollup.Vault); err == nil {
		out["vault"] = map[string]any{"address": s.cfg.Rollup.Vault.String(),
			"znn": info.Balance(znn.ZNN).String(), "qsr": info.Balance(znn.QSR).String()}
	}
	return out, nil
}

// --- stats and search ---------------------------------------------------

// nullIfZero keeps "not known" apart from a value: no fusion price is ever 0.
func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// backfillFusionPrices fills in the fusion price of momentums recorded before
// it was kept, for the window the stats series is about to read. It reads
// NoM in ranges and writes what it finds back, so each height is fetched once.
// A failure leaves the rows NULL and the chart with a gap, never a wrong value.
func (s *Server) backfillFusionPrices(ctx context.Context, limit int) {
	if s.cfg.L1 == nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.height FROM momentums m
		JOIN (SELECT momentum FROM blocks ORDER BY number DESC LIMIT ?) b ON b.momentum = m.height
		WHERE m.fusion_price IS NULL ORDER BY m.height`, limit)
	if err != nil {
		return
	}
	var missing []uint64
	for rows.Next() {
		var h uint64
		if rows.Scan(&h) == nil {
			missing = append(missing, h)
		}
	}
	rows.Close()
	for i := 0; i < len(missing); {
		from := missing[i]
		j := i
		for j+1 < len(missing) && missing[j+1]-from < 100 {
			j++
		}
		list, err := s.cfg.L1.MomentumsByHeight(ctx, from, missing[j]-from+1)
		if err != nil {
			return
		}
		for _, m := range list {
			s.db.ExecContext(ctx, "UPDATE momentums SET fusion_price = ? WHERE height = ? AND fusion_price IS NULL", m.FusionPrice(), m.Height)
		}
		i = j + 1
	}
}

func (s *Server) stats(ctx context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 120, 1000)
	s.backfillFusionPrices(ctx, limit)
	// zvmPlasma is UsedPlasma — what the momentum's account blocks were actually
	// charged — so under dynamic plasma it carries the fusion price and not just
	// the cost of the bytes. Paired with baseFee on one time axis it answers
	// whether what the ZVM pays NoM reaches what its users pay.
	// What each momentum burned in fees, by token. A native call burns ZNN or
	// QSR to the inbox for its gas (D9); a batch burns nothing, so most blocks
	// are zero. Both are in the token's own base units, which is 8 decimals.
	burns := `SELECT momentum,
			SUM(CASE WHEN burn_token = '` + znn.ZNN.String() + `' THEN CAST(burned AS INTEGER) ELSE 0 END) AS znn,
			SUM(CASE WHEN burn_token = '` + znn.QSR.String() + `' THEN CAST(burned AS INTEGER) ELSE 0 END) AS qsr
		FROM messages WHERE burned IS NOT NULL AND burned != '' GROUP BY momentum`
	series, err := s.queryList(`SELECT b.number, b.timestamp, b.user_txs, b.gas_used, b.base_fee, b.size,
		COALESCE(m.zvm_bytes,0), COALESCE(m.nom_blocks,0), COALESCE(m.messages,0), COALESCE(m.build_ms,0),
		COALESCE(m.zvm_plasma,0), COALESCE(f.znn,0), COALESCE(f.qsr,0), COALESCE(m.fusion_price,0)
		FROM blocks b LEFT JOIN momentums m ON m.height = b.momentum
		LEFT JOIN (`+burns+`) f ON f.momentum = b.momentum
		ORDER BY b.number DESC LIMIT ?`,
		[]any{limit}, func(rows *sql.Rows) (any, error) {
			var number, ts, userTxs, gasUsed, size, zvmBytes, nomBlocks, messages, buildMS, zvmPlasma, znnBurned, qsrBurned, fusionPrice int64
			var baseFee string
			if err := rows.Scan(&number, &ts, &userTxs, &gasUsed, &baseFee, &size, &zvmBytes, &nomBlocks, &messages, &buildMS, &zvmPlasma, &znnBurned, &qsrBurned, &fusionPrice); err != nil {
				return nil, err
			}
			perBlock := int64(0)
			if nomBlocks > 0 {
				perBlock = zvmPlasma / nomBlocks
			}
			return map[string]any{"number": number, "timestamp": ts, "txs": userTxs, "gasUsed": gasUsed,
				"baseFee": baseFee, "size": size, "zvmBytes": zvmBytes, "nomBlocks": nomBlocks,
				"messages": messages, "buildMs": buildMS, "zvmPlasma": zvmPlasma, "plasmaPerBlock": perBlock,
				"znnBurned": znnBurned, "qsrBurned": qsrBurned, "fusionPrice": nullIfZero(fusionPrice)}, nil
		})
	if err != nil {
		return nil, err
	}
	totals := map[string]any{}
	var txs, blocks, gas int64
	s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(user_txs),0), COALESCE(SUM(gas_used),0) FROM blocks").Scan(&blocks, &txs, &gas)
	totals["blocks"], totals["txs"], totals["gas"] = blocks, txs, gas
	var contracts, tokens int64
	s.db.QueryRow("SELECT COUNT(*) FROM contracts").Scan(&contracts)
	s.db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&tokens)
	totals["contracts"], totals["tokens"] = contracts, tokens
	// The highest transactions-per-second any single block has carried across
	// the chain's whole history, kept as it happens rather than recomputed
	// from whatever window `limit` asks for (see chain_stats in the indexer).
	var maxTPS float64
	var maxTPSBlock, maxTPSTxs, maxTPSAt int64
	if err := s.db.QueryRow("SELECT max_tps, max_tps_block, max_tps_txs, max_tps_at FROM chain_stats WHERE id = 1").
		Scan(&maxTPS, &maxTPSBlock, &maxTPSTxs, &maxTPSAt); err == nil {
		totals["maxTps"] = map[string]any{"tps": maxTPS, "block": maxTPSBlock, "txs": maxTPSTxs, "timestamp": maxTPSAt}
	}
	// Everything this chain has ever burned for native-call gas, by token, in
	// base units. It is the only fee that leaves circulation on NoM itself:
	// the relayed lane's split happens on the ZVM and is mirrored separately.
	var znnBurned, qsrBurned int64
	s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN burn_token = ? THEN CAST(burned AS INTEGER) END),0),
		COALESCE(SUM(CASE WHEN burn_token = ? THEN CAST(burned AS INTEGER) END),0)
		FROM messages WHERE burned IS NOT NULL AND burned != ''`,
		znn.ZNN.String(), znn.QSR.String()).Scan(&znnBurned, &qsrBurned)
	totals["znnBurned"], totals["qsrBurned"] = znnBurned, qsrBurned
	var lanes []any
	if rows, err := s.db.Query("SELECT lane, COUNT(*) FROM txs GROUP BY lane"); err == nil {
		for rows.Next() {
			var lane string
			var n int64
			rows.Scan(&lane, &n)
			lanes = append(lanes, map[string]any{"lane": lane, "count": n})
		}
		rows.Close()
	}
	totals["lanes"] = lanes
	return map[string]any{"series": series, "totals": totals}, nil
}

func (s *Server) contracts(_ context.Context, r *http.Request) (any, error) {
	limit := intParam(r, "limit", 50, 200)
	return s.queryList(`SELECT address, creator, tx, block, code_size FROM contracts ORDER BY block DESC LIMIT ?`,
		[]any{limit}, func(rows *sql.Rows) (any, error) {
			var address, creator, tx string
			var block, codeSize int64
			if err := rows.Scan(&address, &creator, &tx, &block, &codeSize); err != nil {
				return nil, err
			}
			return map[string]any{"address": address, "creator": creator, "tx": tx, "block": block, "codeSize": codeSize}, nil
		})
}

func (s *Server) search(ctx context.Context, r *http.Request) (any, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return nil, fmt.Errorf("%w: empty query", errBadRequest)
	}
	hit := func(kind, id string) any { return map[string]any{"kind": kind, "id": id} }
	if n, err := strconv.ParseUint(q, 10, 64); err == nil {
		var exists int
		if s.db.QueryRow("SELECT 1 FROM blocks WHERE number = ?", n).Scan(&exists) == nil {
			return hit("block", q), nil
		}
		if s.db.QueryRow("SELECT 1 FROM momentums WHERE height = ?", n).Scan(&exists) == nil {
			return hit("momentum", q), nil
		}
	}
	if strings.HasPrefix(strings.ToLower(q), "z1") || common.IsHexAddress(q) {
		if _, err := parseAddress(q); err == nil {
			return hit("address", q), nil
		}
	}
	// Hashes are stored lower case; one pasted from elsewhere may not be.
	bare := strings.TrimPrefix(strings.ToLower(q), "0x")
	if len(bare) == 64 {
		var exists int
		if s.db.QueryRow("SELECT 1 FROM txs WHERE hash = ?", "0x"+bare).Scan(&exists) == nil {
			return hit("tx", "0x"+bare), nil
		}
		if s.db.QueryRow("SELECT 1 FROM blocks WHERE hash = ?", "0x"+bare).Scan(&exists) == nil {
			return hit("block", "0x"+bare), nil
		}
		if s.db.QueryRow("SELECT 1 FROM messages WHERE nom_hash = ?", strings.ToLower(bare)).Scan(&exists) == nil {
			return hit("nom", strings.ToLower(bare)), nil
		}
		var height int64
		if s.db.QueryRow("SELECT height FROM momentums WHERE hash = ?", strings.ToLower(bare)).Scan(&height) == nil {
			return hit("momentum", strconv.FormatInt(height, 10)), nil
		}
	}
	return nil, errNotFound
}

// --- helpers ------------------------------------------------------------

func ethCall(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

func (s *Server) queryList(q string, args []any, scan func(*sql.Rows) (any, error)) ([]any, error) {
	return QueryList(s.db, q, args, scan)
}

// feeQuote prices a native call: what it must burn to buy the gas it asks for.
//
// The quote is computed here rather than in the page, for the reason prices
// are: a second implementation of NoM's emission schedule in JavaScript is a
// second thing to keep in step, and a page that quotes low publishes a call
// that derives with less gas than it asked for and no way to get it back.
func (s *Server) feeQuote(ctx context.Context, r *http.Request) (any, error) {
	cfg := s.cfg.Rollup
	gas, err := strconv.ParseUint(r.URL.Query().Get("gas"), 10, 64)
	if err != nil || gas == 0 {
		return nil, fmt.Errorf("%w: gas is required", errBadRequest)
	}
	if gas > cfg.MaxNativeGas {
		return nil, fmt.Errorf("%w: a native call is capped at %d gas", errBadRequest, cfg.MaxNativeGas)
	}
	token := znn.ZNN
	switch strings.ToUpper(r.URL.Query().Get("token")) {
	case "", "ZNN":
	case "QSR":
		token = znn.QSR
	default:
		return nil, fmt.Errorf("%w: token must be ZNN or QSR", errBadRequest)
	}
	// A block's size sets both its plasma and, under dynamic plasma, the price
	// it must meet, so the quote depends on how big the message is.
	size := intParam(r, "bytes", 128, znn.MaxDataLength)
	frontier, err := s.cfg.L1.FrontierMomentum(ctx)
	if err != nil {
		return nil, err
	}
	block := derive.QuoteBlock(size)
	active := cfg.Fee.Active(frontier.Height + 1)
	res := map[string]any{
		"gas":        gas,
		"token":      token.String(),
		"symbol":     map[znn.TokenStandard]string{znn.ZNN: "ZNN", znn.QSR: "QSR"}[token],
		"decimals":   8,
		"bytes":      size,
		"active":     active,
		"fromHeight": feeFromHeight(cfg),
		"plasmaGas":  block.UsedPlasma * cfg.GasPerPlasma,
		"amount":     "0",
	}
	if active {
		res["amount"] = derive.FeeQuote(cfg, token, gas, frontier, block).String()
		res["epoch"] = cfg.Fee.EpochAt(int64(frontier.Timestamp))
		res["marginBps"] = derive.FeeMarginBP
	}
	return res, nil
}

func feeFromHeight(cfg *rollup.Config) uint64 {
	if cfg.Fee == nil {
		return 0
	}
	return cfg.Fee.FromHeight
}
