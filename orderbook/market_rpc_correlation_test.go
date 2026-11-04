package orderbook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const marketRPCCorrelationHeader = "X-ZVM-Fixture-Request"
const marketRPCCorrelationQueryHeader = "X-ZVM-Fixture-Requests"
const marketRPCCorrelationMaximumRecords = 256
const marketRPCCorrelationMaximumActive = 32
const marketRPCCorrelationMaximumQuery = 8

type marketRPCCorrelationRecord struct {
	connection                            *marketRPCHTTP1Cycle
	connectionReason                      string
	ctx                                   context.Context
	returned, normal, canceled, ambiguous bool
}
type marketRPCCorrelationMatch struct {
	Index            int    `json:"index"`
	Status           string `json:"status"`
	Returned         bool   `json:"returned"`
	ReturnedNormally bool   `json:"returnedNormally"`
	ContextCanceled  bool   `json:"contextCanceled"`
}
type marketRPCCorrelationSnapshot struct {
	Version                            int                         `json:"version"`
	Scope                              string                      `json:"scope"`
	MaximumRecords                     int                         `json:"maximumRecords"`
	MaximumActive                      int                         `json:"maximumActive"`
	MaximumQuery                       int                         `json:"maximumQuery"`
	Overflow                           bool                        `json:"overflow"`
	Matches                            []marketRPCCorrelationMatch `json:"matches"`
	HTTPStatusBodyOrDeliveryObserved   bool                        `json:"HTTPStatusBodyOrDeliveryObserved"`
	ProtocolSessionIdentityEstablished bool                        `json:"protocolSessionIdentityEstablished"`
	QualifiesNavigation                bool                        `json:"qualifiesNavigation"`
}

// This separate fixture ledger keeps only an owned nonce and handler lifetime.
// It preserves both HTTP arguments and the original handler's panic/result.
// It adds one response header for a tracked request; it observes no body/status.
type marketRPCCorrelations struct {
	mu                 sync.Mutex
	records            map[string]*marketRPCCorrelationRecord
	active             int
	overflow           bool
	connections        map[net.Conn]*marketRPCConnection
	connectionOverflow bool
}

func newMarketRPCCorrelations() *marketRPCCorrelations {
	return &marketRPCCorrelations{records: make(map[string]*marketRPCCorrelationRecord), connections: make(map[net.Conn]*marketRPCConnection)}
}
func marketRPCNonce(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
func marketRPCCapability(value string) bool {
	return len(value) == 64 && marketRPCNonce(value[:32]) && marketRPCNonce(value[32:])
}
func (p *marketRPCCorrelations) begin(nonce string, ctx context.Context) *marketRPCCorrelationRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.records[nonce]; old != nil {
		old.ambiguous = true
		return nil
	}
	if len(p.records) == marketRPCCorrelationMaximumRecords || p.active == marketRPCCorrelationMaximumActive {
		p.overflow = true
		return nil
	}
	row := &marketRPCCorrelationRecord{ctx: ctx}
	p.records[nonce] = row
	p.active++
	return row
}
func (p *marketRPCCorrelations) finish(row *marketRPCCorrelationRecord, normal bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row.returned, row.normal, row.canceled = true, normal, row.ctx.Err() != nil
	if row.connection != nil {
		row.connection.returned = true
	}
	row.ctx = nil
	p.active--
}
func (p *marketRPCCorrelations) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(marketRPCCorrelationHeader)
		if r.Method != http.MethodPost || r.URL.Path != "/rpc" || len(values) != 1 || !marketRPCNonce(values[0]) {
			next.ServeHTTP(w, r)
			return
		}
		row := p.begin(values[0], r.Context())
		if row == nil {
			next.ServeHTTP(w, r)
			return
		}
		p.bindConnection(row, r)
		w.Header().Set(marketRPCCorrelationHeader, values[0])
		normal := false
		defer func() { p.finish(row, normal) }()
		next.ServeHTTP(w, r)
		normal = true
	})
}
func (p *marketRPCCorrelations) snapshot(nonces []string) marketRPCCorrelationSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := marketRPCCorrelationSnapshot{Version: 1, Scope: "owned-loopback-rpc-request-correlation",
		MaximumRecords: marketRPCCorrelationMaximumRecords, MaximumActive: marketRPCCorrelationMaximumActive,
		MaximumQuery: marketRPCCorrelationMaximumQuery, Overflow: p.overflow,
		Matches: make([]marketRPCCorrelationMatch, 0, len(nonces))}
	for index, nonce := range nonces {
		match := marketRPCCorrelationMatch{Index: index, Status: "UNKNOWN"}
		if row := p.records[nonce]; row != nil {
			match.Status = "MATCHED"
			if row.ambiguous {
				match.Status = "AMBIGUOUS"
			}
			match.Returned, match.ReturnedNormally, match.ContextCanceled = row.returned, row.normal, row.canceled
			if row.ctx != nil {
				match.ContextCanceled = row.ctx.Err() != nil
			}
		}
		value.Matches = append(value.Matches, match)
	}
	return value
}
func (p *marketRPCCorrelations) lookupHandler(capability string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !marketRPCCapability(capability) {
			http.Error(w, "fixture unavailable", 503)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "read-only fixture route required", 405)
			return
		}
		if r.Header.Get("X-ZVM-Fixture") != capability {
			http.Error(w, "fixture capability required", 403)
			return
		}
		headers := r.Header.Values(marketRPCCorrelationQueryHeader)
		if len(headers) != 1 || len(headers[0]) > marketRPCCorrelationMaximumQuery*33-1 {
			http.Error(w, "bounded fixture selection required", 400)
			return
		}
		nonces := strings.Split(headers[0], ",")
		seen := map[string]bool{}
		if len(nonces) > marketRPCCorrelationMaximumQuery {
			http.Error(w, "bounded fixture selection required", 400)
			return
		}
		for _, nonce := range nonces {
			if !marketRPCNonce(nonce) || seen[nonce] {
				http.Error(w, "bounded fixture selection required", 400)
				return
			}
			seen[nonce] = true
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(p.snapshot(nonces)); err != nil {
			return
		}
	})
}

// These additional controls execute inside the existing normal/race-selected
// TestMarketRPCProgress parent. Every original named subtest remains unchanged.
func requireMarketRPCCorrelation(t *testing.T) {
	t.Helper()
	nonce, other := strings.Repeat("1", 32), strings.Repeat("2", 32)
	cap := strings.Repeat("a", 64)
	request := func(method, path, nonce string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader("private-body"))
		if nonce != "" {
			r.Header.Set(marketRPCCorrelationHeader, nonce)
		}
		r.Header.Set("Authorization", "private-header")
		return r
	}
	query := func(p *marketRPCCorrelations, selection, capability string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/__fixture/rpc-correlations", nil)
		r.Header.Set("X-ZVM-Fixture", capability)
		r.Header.Set(marketRPCCorrelationQueryHeader, selection)
		w := httptest.NewRecorder()
		p.lookupHandler(cap).ServeHTTP(w, r)
		return w
	}
	check := func(ok bool, why string) {
		t.Helper()
		if !ok {
			t.Fatal("owned RPC correlation control: " + why)
		}
	}
	controls := 0
	{
		p := newMarketRPCCorrelations()
		r := request("POST", "/rpc?private-query", nonce)
		w := httptest.NewRecorder()
		next := http.HandlerFunc(func(got http.ResponseWriter, req *http.Request) {
			check(got == w && req == r, "HTTP argument identity changed")
			got.WriteHeader(201)
			got.Write([]byte("unchanged-response"))
		})
		p.observe(next).ServeHTTP(w, r)
		check(w.Code == 201 && w.Body.String() == "unchanged-response" && w.Header().Get(marketRPCCorrelationHeader) == nonce, "pass-through or owned echo differs")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		calls := 0
		next := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		for _, v := range [][2]string{{"GET", "/rpc"}, {"POST", "/other"}} {
			w := httptest.NewRecorder()
			next.ServeHTTP(w, request(v[0], v[1], nonce))
			check(w.Header().Get(marketRPCCorrelationHeader) == "", "unobserved echo")
		}
		check(calls == 2 && len(p.records) == 0, "unobserved route tracked")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		calls := 0
		next := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		for _, bad := range []string{"", strings.Repeat("A", 32), strings.Repeat("1", 31), strings.Repeat("1", 33), strings.Repeat("g", 32)} {
			next.ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", bad))
		}
		check(calls == 5 && len(p.records) == 0, "malformed nonce changed forwarding")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		r := request("POST", "/rpc", nonce)
		r.Header.Add(marketRPCCorrelationHeader, other)
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), r)
		check(len(p.records) == 0, "multiple header values admitted")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		v := p.snapshot([]string{nonce}).Matches[0]
		check(v.Status == "MATCHED" && v.Returned && v.ReturnedNormally && p.active == 0, "normal completion differs")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			v := p.snapshot([]string{nonce}).Matches[0]
			check(v.Status == "MATCHED" && !v.Returned && !v.ContextCanceled, "active lookup differs")
		})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			cancel()
			check(p.snapshot([]string{nonce}).Matches[0].ContextCanceled, "active cancellation lost")
		})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce).WithContext(ctx))
		v := p.snapshot([]string{nonce}).Matches[0]
		check(v.ContextCanceled && p.records[nonce].ctx == nil, "completed context retained")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		func() {
			defer func() { check(recover() == "owned-panic", "panic swallowed or replaced") }()
			p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("owned-panic") })).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		}()
		v := p.snapshot([]string{nonce}).Matches[0]
		check(v.Returned && !v.ReturnedNormally && p.active == 0, "panic completion differs")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		calls := 0
		h := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
		h.ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request("POST", "/rpc", nonce))
		check(calls == 2 && p.snapshot([]string{nonce}).Matches[0].Status == "AMBIGUOUS" && w.Header().Get(marketRPCCorrelationHeader) == "", "duplicate identity admitted")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", nonce))
		check(p.active == 0 && p.snapshot([]string{nonce}).Matches[0].Status == "AMBIGUOUS", "active duplicate admitted")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		h := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		for i := 1; i <= marketRPCCorrelationMaximumRecords+1; i++ {
			h.ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", fmt.Sprintf("%032x", i)))
		}
		check(len(p.records) == marketRPCCorrelationMaximumRecords && p.overflow && p.active == 0, "record bound or honest overflow differs")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		var enter func(int)
		enter = func(depth int) {
			p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				if depth < marketRPCCorrelationMaximumActive+1 {
					enter(depth + 1)
				} else {
					check(p.active == marketRPCCorrelationMaximumActive, "active bound exceeded")
				}
			})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", fmt.Sprintf("%032x", depth)))
		}
		enter(1)
		check(p.active == 0 && p.overflow && len(p.records) == marketRPCCorrelationMaximumActive, "active limit hidden")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		check(query(p, nonce, "wrong").Code == 403, "capability refusal lost")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		for _, bad := range []string{"", nonce + "," + nonce, strings.Repeat("g", 32), strings.Repeat(nonce+",", 8) + other} {
			check(query(p, bad, cap).Code == 400, "invalid selection admitted")
		}
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		w := query(p, other+","+nonce, cap)
		var v marketRPCCorrelationSnapshot
		check(w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &v) == nil && len(v.Matches) == 2 && v.Matches[0].Index == 0 && v.Matches[1].Status == "UNKNOWN", "query ordering or unknown identity differs")
		check(!v.QualifiesNavigation && !v.ProtocolSessionIdentityEstablished && !v.HTTPStatusBodyOrDeliveryObserved, "lookup widened authority")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc?private-query", nonce))
		raw := query(p, nonce, cap).Body.String()
		check(!strings.Contains(raw, nonce) && !strings.Contains(raw, cap) && !strings.Contains(raw, "private-"), "private nonce or request data reported")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		old := newMarketRPCProgress()
		r := request("POST", "/rpc", nonce)
		w := httptest.NewRecorder()
		p.observe(old.observe(http.HandlerFunc(func(got http.ResponseWriter, req *http.Request) {
			check(got == w && req == r, "composed observer replaced arguments")
		}))).ServeHTTP(w, r)
		check(old.snapshot().NormalReturns == 1 && !old.snapshot().ResponseWriterOrRequestReplaced, "original observer changed")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		var group sync.WaitGroup
		for i := 1; i <= 16; i++ {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				n := fmt.Sprintf("%032x", i)
				p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), request("POST", "/rpc", n))
				p.snapshot([]string{n})
			}(i)
		}
		group.Wait()
		check(p.active == 0 && len(p.records) == 16 && !p.overflow, "joined concurrency differs")
		controls++
	}
	check(controls == 18, "declared control count differs")
	t.Logf("OWNED-MARKET-RPC-CORRELATION-CONTROLS cases=%d pass=true memoryOnly=true originalHTTPArgumentsPreserved=true nonceOrRequestValuesReported=false completionGateChanged=false", controls)
}
