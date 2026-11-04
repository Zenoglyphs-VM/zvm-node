package orderbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const marketRPCMaximumConnections = 32

type marketRPCConnectionKey struct{}
type marketRPCHTTP1Cycle struct{ bound, returned, idle, closed, hijacked, ambiguous bool }
type marketRPCConnection struct {
	owner *marketRPCCorrelations
	state http.ConnState
	cycle *marketRPCHTTP1Cycle
}
type marketRPCConnectionMatch struct {
	Index                          int    `json:"index"`
	Status                         string `json:"status"`
	Reason                         string `json:"reason"`
	HTTP1CycleBound                bool   `json:"HTTP1CycleBound"`
	HandlerReturned                bool   `json:"handlerReturned"`
	HandlerReturnedNormally        bool   `json:"handlerReturnedNormally"`
	ServerIdleAfterHandler         bool   `json:"serverIdleAfterHandler"`
	ConnectionClosedAfterHandler   bool   `json:"connectionClosedAfterHandler"`
	ConnectionHijackedAfterHandler bool   `json:"connectionHijackedAfterHandler"`
}
type marketRPCConnectionSnapshot struct {
	Version                            int                        `json:"version"`
	Scope                              string                     `json:"scope"`
	MaximumRecords                     int                        `json:"maximumRecords"`
	MaximumConnections                 int                        `json:"maximumConnections"`
	MaximumQuery                       int                        `json:"maximumQuery"`
	ConnectionTrackingOverflow         bool                       `json:"connectionTrackingOverflow"`
	Matches                            []marketRPCConnectionMatch `json:"matches"`
	HTTPArgumentsReplaced              bool                       `json:"HTTPArgumentsReplaced"`
	PayloadOrAddressesReported         bool                       `json:"payloadOrAddressesReported"`
	ResponseDeliveryEstablished        bool                       `json:"responseDeliveryEstablished"`
	ProtocolSessionIdentityEstablished bool                       `json:"protocolSessionIdentityEstablished"`
	QualifiesNavigation                bool                       `json:"qualifiesNavigation"`
}

// Native server hooks observe connection state, not bytes, status, delivery or
// browser completion. Only HTTP/1.1 cycles can bind a per-request observation.
// No ResponseWriter, Request, net.Conn, body or original callback is replaced.
func (p *marketRPCCorrelations) configureConnections(s *http.Server) error {
	if s == nil || s.ConnContext != nil || s.ConnState != nil {
		return errors.New("fresh owned HTTP server hooks required")
	}
	s.ConnContext = p.connectionContext
	s.ConnState = p.connectionState
	return nil
}
func (p *marketRPCCorrelations) connectionContext(ctx context.Context, c net.Conn) context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.connections[c]; exists || len(p.connections) == marketRPCMaximumConnections {
		p.connectionOverflow = true
		return ctx
	}
	entry := &marketRPCConnection{owner: p, state: http.StateNew}
	p.connections[c] = entry
	return context.WithValue(ctx, marketRPCConnectionKey{}, entry)
}
func (p *marketRPCCorrelations) connectionState(c net.Conn, state http.ConnState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.connections[c]
	if entry == nil {
		return
	}
	entry.state = state
	if state == http.StateActive {
		if old := entry.cycle; old != nil && old.bound && !old.returned {
			old.ambiguous = true
		}
		entry.cycle = &marketRPCHTTP1Cycle{}
	} else if cycle := entry.cycle; cycle != nil && cycle.bound {
		if !cycle.returned {
			cycle.ambiguous = true
		} else {
			switch state {
			case http.StateIdle:
				cycle.idle = true
			case http.StateClosed:
				cycle.closed = true
			case http.StateHijacked:
				cycle.hijacked = true
			}
		}
	}
	if state == http.StateClosed || state == http.StateHijacked {
		delete(p.connections, c)
	}
}
func (p *marketRPCCorrelations) bindConnection(row *marketRPCCorrelationRecord, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.ProtoMajor != 1 || r.ProtoMinor != 1 {
		row.connectionReason = "PROTOCOL"
		return
	}
	entry, _ := r.Context().Value(marketRPCConnectionKey{}).(*marketRPCConnection)
	if entry == nil {
		row.connectionReason = "NO_CONTEXT"
		return
	}
	if entry.owner != p {
		row.connectionReason = "FOREIGN_CONTEXT"
		return
	}
	if entry.state != http.StateActive || entry.cycle == nil {
		row.connectionReason = "NO_ACTIVE_CYCLE"
		return
	}
	row.connection = entry.cycle
	if row.connection.bound {
		row.connection.ambiguous = true
		row.connectionReason = "REUSED_CYCLE"
		return
	}
	row.connection.bound = true
}
func (p *marketRPCCorrelations) connectionSnapshot(nonces []string) marketRPCConnectionSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := marketRPCConnectionSnapshot{Version: 1, Scope: "owned-loopback-http1-post-handler-connection-lifecycle",
		MaximumRecords: marketRPCCorrelationMaximumRecords, MaximumConnections: marketRPCMaximumConnections,
		MaximumQuery: marketRPCCorrelationMaximumQuery, ConnectionTrackingOverflow: p.connectionOverflow,
		Matches: make([]marketRPCConnectionMatch, 0, len(nonces))}
	for index, nonce := range nonces {
		match := marketRPCConnectionMatch{Index: index, Status: "UNKNOWN", Reason: "NO_REQUEST"}
		if row := p.records[nonce]; row != nil {
			match.HandlerReturned, match.HandlerReturnedNormally = row.returned, row.normal
			switch {
			case row.ambiguous:
				match.Status, match.Reason = "AMBIGUOUS", "NONCE"
			case row.connection == nil:
				match.Status, match.Reason = "UNAVAILABLE", row.connectionReason
				if match.Reason == "" {
					match.Reason = "NO_CONTEXT"
				}
			case row.connection.ambiguous:
				match.Status, match.Reason = "AMBIGUOUS", "CYCLE"
			default:
				cycle := row.connection
				match.Status, match.Reason = "MATCHED", ""
				match.HTTP1CycleBound = cycle.bound
				match.ServerIdleAfterHandler, match.ConnectionClosedAfterHandler, match.ConnectionHijackedAfterHandler = cycle.idle, cycle.closed, cycle.hijacked
			}
		}
		value.Matches = append(value.Matches, match)
	}
	return value
}
func (p *marketRPCCorrelations) connectionLookupHandler(capability string) http.Handler {
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
		_ = json.NewEncoder(w).Encode(p.connectionSnapshot(nonces))
	})
}

// These finite callback controls run inside the unchanged normal/race-selected
// parent. An embedded nil net.Conn makes accidental address/IO access panic.
type marketRPCFakeConnection struct{ net.Conn }

func requireMarketRPCConnections(t *testing.T) {
	t.Helper()
	controls := 0
	check := func(ok bool, why string) {
		t.Helper()
		if !ok {
			t.Fatal("owned HTTP1 connection control: " + why)
		}
	}
	nonce, other, capability := strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("a", 64)
	setup := func() (*marketRPCCorrelations, *marketRPCFakeConnection, context.Context) {
		p := newMarketRPCCorrelations()
		c := &marketRPCFakeConnection{}
		ctx := p.connectionContext(context.Background(), c)
		p.connectionState(c, http.StateNew)
		p.connectionState(c, http.StateActive)
		return p, c, ctx
	}
	serve := func(p *marketRPCCorrelations, ctx context.Context, n string, next http.HandlerFunc) {
		r := httptest.NewRequest("POST", "/rpc?private-query", strings.NewReader("private-body")).WithContext(ctx)
		r.Header.Set(marketRPCCorrelationHeader, n)
		r.Header.Set("Authorization", "private-header")
		w := httptest.NewRecorder()
		p.observe(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
			check(got == w && request == r, "HTTP arguments changed")
			next.ServeHTTP(got, request)
		})).ServeHTTP(w, r)
	}
	noOp := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	match := func(p *marketRPCCorrelations, n string) marketRPCConnectionMatch {
		return p.connectionSnapshot([]string{n}).Matches[0]
	}
	{
		p, c, ctx := setup()
		key := struct{ value int }{1}
		ctx = context.WithValue(ctx, key, "preserved")
		serve(p, ctx, nonce, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			check(r.Context().Value(key) == "preserved", "base context lost")
			_, ok := w.(http.Flusher)
			check(ok, "optional writer interface lost")
			w.WriteHeader(201)
		}))
		p.connectionState(c, http.StateIdle)
		check(match(p, nonce).ServerIdleAfterHandler, "normal idle missing")
		controls++
	}
	{
		p, _, ctx := setup()
		serve(p, ctx, nonce, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			m := match(p, nonce)
			check(m.HTTP1CycleBound && !m.HandlerReturned && !m.ServerIdleAfterHandler, "active cycle fabricated completion")
		}))
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		m := match(p, nonce)
		check(m.HandlerReturnedNormally && !m.ServerIdleAfterHandler, "handler return treated as idle")
		p.connectionState(c, http.StateIdle)
		check(match(p, nonce).ServerIdleAfterHandler, "post-return idle missing")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateClosed)
		m := match(p, nonce)
		check(m.ConnectionClosedAfterHandler && !m.ServerIdleAfterHandler && len(p.connections) == 0, "close became idle or retained connection")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateIdle)
		p.connectionState(c, http.StateClosed)
		m := match(p, nonce)
		check(m.ServerIdleAfterHandler && m.ConnectionClosedAfterHandler, "idle then closed provenance lost")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateHijacked)
		m := match(p, nonce)
		check(m.ConnectionHijackedAfterHandler && !m.ServerIdleAfterHandler && len(p.connections) == 0, "hijacked lifecycle differs")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateIdle)
		p.connectionState(c, http.StateActive)
		serve(p, ctx, other, noOp)
		check(match(p, nonce).ServerIdleAfterHandler && !match(p, other).ServerIdleAfterHandler, "cycles aliased")
		p.connectionState(c, http.StateClosed)
		check(!match(p, nonce).ConnectionClosedAfterHandler && match(p, other).ConnectionClosedAfterHandler, "later close attached to old cycle")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		serve(p, context.Background(), nonce, noOp)
		m := match(p, nonce)
		check(m.Status == "UNAVAILABLE" && m.Reason == "NO_CONTEXT" && p.snapshot([]string{nonce}).Matches[0].Status == "MATCHED", "missing context changed original observation")
		controls++
	}
	{
		p, _, ctx := setup()
		r := httptest.NewRequest("POST", "/rpc", nil).WithContext(ctx)
		r.ProtoMajor, r.ProtoMinor = 2, 0
		r.Header.Set(marketRPCCorrelationHeader, nonce)
		p.observe(noOp).ServeHTTP(httptest.NewRecorder(), r)
		check(match(p, nonce).Reason == "PROTOCOL", "HTTP2 became a request cycle")
		controls++
	}
	{
		_, _, ctx := setup()
		p := newMarketRPCCorrelations()
		serve(p, ctx, nonce, noOp)
		check(match(p, nonce).Reason == "FOREIGN_CONTEXT", "foreign connection bound")
		controls++
	}
	{
		p, _, ctx := setup()
		serve(p, ctx, nonce, noOp)
		serve(p, ctx, other, noOp)
		check(match(p, nonce).Status == "AMBIGUOUS" && match(p, other).Status == "AMBIGUOUS", "reused cycle accepted")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateIdle)
		p.connectionState(c, http.StateActive)
		serve(p, ctx, nonce, noOp)
		check(match(p, nonce).Status == "AMBIGUOUS" && !match(p, nonce).HTTP1CycleBound, "duplicate nonce uniquely bound")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		c := &marketRPCFakeConnection{}
		ctx := p.connectionContext(context.Background(), c)
		serve(p, ctx, nonce, noOp)
		check(match(p, nonce).Reason == "NO_ACTIVE_CYCLE", "new connection became active")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { p.connectionState(c, http.StateIdle) }))
		check(match(p, nonce).Status == "AMBIGUOUS" && !match(p, nonce).ServerIdleAfterHandler, "premature idle became completion")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		m := match(p, nonce)
		check(m.Status == "UNKNOWN" && !m.HandlerReturned && !m.HTTP1CycleBound, "unknown request had lifecycle")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		var first *marketRPCFakeConnection
		for i := 0; i < marketRPCMaximumConnections; i++ {
			c := &marketRPCFakeConnection{}
			if i == 0 {
				first = c
			}
			p.connectionContext(context.Background(), c)
		}
		c := &marketRPCFakeConnection{}
		ctx := p.connectionContext(context.Background(), c)
		p.connectionState(c, http.StateActive)
		serve(p, ctx, nonce, noOp)
		check(len(p.connections) == marketRPCMaximumConnections && p.connectionOverflow && match(p, nonce).Reason == "NO_CONTEXT", "connection cap bypassed")
		p.connectionState(first, http.StateClosed)
		p.connectionContext(context.Background(), &marketRPCFakeConnection{})
		check(len(p.connections) == marketRPCMaximumConnections && p.connectionOverflow, "freed slot lost overflow evidence")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		called := false
		s := &http.Server{ConnState: func(net.Conn, http.ConnState) { called = true }}
		check(p.configureConnections(s) != nil, "preconfigured callback overwritten")
		s.ConnState(nil, http.StateNew)
		check(called, "original callback lost")
		check(p.configureConnections(nil) != nil, "nil server configured")
		controls++
	}
	{
		p, c, ctx := setup()
		func() {
			defer func() { check(recover() == "declared connection panic", "handler panic replaced") }()
			serve(p, ctx, nonce, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("declared connection panic") }))
		}()
		p.connectionState(c, http.StateClosed)
		m := match(p, nonce)
		check(m.HandlerReturned && !m.HandlerReturnedNormally && m.ConnectionClosedAfterHandler, "panic lifecycle became normal")
		controls++
	}
	query := func(p *marketRPCCorrelations, method, selection, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/__fixture/rpc-connections", nil)
		r.Header.Set("X-ZVM-Fixture", token)
		r.Header.Set(marketRPCCorrelationQueryHeader, selection)
		w := httptest.NewRecorder()
		p.connectionLookupHandler(capability).ServeHTTP(w, r)
		return w
	}
	{
		p := newMarketRPCCorrelations()
		check(query(p, "POST", nonce, capability).Code == 405 && query(p, "GET", nonce, "bad").Code == 403, "read-only capability controls differ")
		controls++
	}
	{
		p, c, ctx := setup()
		serve(p, ctx, nonce, noOp)
		p.connectionState(c, http.StateIdle)
		w := query(p, "GET", nonce, capability)
		check(w.Code == 200, "snapshot unavailable")
		raw := w.Body.String()
		check(!strings.Contains(raw, nonce) && !strings.Contains(raw, "private-") && !strings.Contains(raw, capability), "private material reported")
		v := p.connectionSnapshot([]string{nonce})
		check(!v.HTTPArgumentsReplaced && !v.PayloadOrAddressesReported && !v.ResponseDeliveryEstablished && !v.ProtocolSessionIdentityEstablished && !v.QualifiesNavigation, "connection observation widened authority")
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		for _, selection := range []string{"", nonce + "," + nonce, strings.Repeat("A", 32), strings.Repeat("x", 264)} {
			check(query(p, "GET", selection, capability).Code == 400, "invalid selection accepted")
		}
		controls++
	}
	{
		p := newMarketRPCCorrelations()
		r := httptest.NewRequest("GET", "/__fixture/rpc-connections", nil)
		r.Header.Set("X-ZVM-Fixture", capability)
		r.Header.Add(marketRPCCorrelationQueryHeader, nonce)
		r.Header.Add(marketRPCCorrelationQueryHeader, other)
		w := httptest.NewRecorder()
		p.connectionLookupHandler(capability).ServeHTTP(w, r)
		check(w.Code == 400, "multiple query headers accepted")
		controls++
	}
	{
		p, _, ctx := setup()
		r := httptest.NewRequest("GET", "/rpc", nil).WithContext(ctx)
		r.Header.Set(marketRPCCorrelationHeader, nonce)
		p.observe(noOp).ServeHTTP(httptest.NewRecorder(), r)
		check(len(p.records) == 0, "unobserved route bound")
		controls++
	}
	{
		p, _, ctx := setup()
		r := httptest.NewRequest("POST", "/rpc", nil).WithContext(ctx)
		r.ProtoMajor, r.ProtoMinor = 1, 0
		r.Header.Set(marketRPCCorrelationHeader, nonce)
		p.observe(noOp).ServeHTTP(httptest.NewRecorder(), r)
		check(match(p, nonce).Reason == "PROTOCOL", "HTTP1.0 inferred keepalive lifecycle")
		s := &http.Server{}
		check(p.configureConnections(s) == nil && s.ConnContext != nil && s.ConnState != nil, "fresh hooks unavailable")
		controls++
	}
	check(controls == 24, "control inventory differs")
	t.Log("OWNED-MARKET-RPC-CONNECTION-CONTROLS cases=24 pass=true HTTPArgumentsPreserved=true connectionOnly=true bodyDeliveryEstablished=false")
}
