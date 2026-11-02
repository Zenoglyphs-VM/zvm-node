package orderbook_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const marketRPCMaximumActive = 32
const marketRPCMaximumRecent = 8

type marketRPCStart struct {
	sequence uint64
	started  time.Time
	ctx      context.Context
	tracked  bool
}

type marketRPCActive struct {
	Sequence        uint64 `json:"sequence"`
	AgeMilliseconds int64  `json:"ageMilliseconds"`
	ContextCanceled bool   `json:"contextCanceled"`
}

type marketRPCRecent struct {
	Sequence             uint64 `json:"sequence"`
	DurationMilliseconds int64  `json:"durationMilliseconds"`
	ReturnedNormally     bool   `json:"returnedNormally"`
	ContextCanceled      bool   `json:"contextCanceled"`
}

type marketRPCSnapshot struct {
	Version                            int               `json:"version"`
	Scope                              string            `json:"scope"`
	MaximumTrackedActive               int               `json:"maximumTrackedActive"`
	MaximumRecent                      int               `json:"maximumRecent"`
	Started                            uint64            `json:"started"`
	Finished                           uint64            `json:"finished"`
	NormalReturns                      uint64            `json:"normalReturns"`
	AbnormalExits                      uint64            `json:"abnormalExits"`
	Active                             uint64            `json:"active"`
	OverflowStarted                    uint64            `json:"overflowStarted"`
	UntrackedActive                    uint64            `json:"untrackedActive"`
	CompleteTracking                   bool              `json:"completeTracking"`
	ActiveRecords                      []marketRPCActive `json:"activeRecords"`
	Recent                             []marketRPCRecent `json:"recent"`
	HTTPStatusBodyOrDeliveryObserved   bool              `json:"HTTPStatusBodyOrDeliveryObserved"`
	ProtocolRequestIdentityEstablished bool              `json:"protocolRequestIdentityEstablished"`
	ResponseWriterOrRequestReplaced    bool              `json:"responseWriterOrRequestReplaced"`
}

// Counts describe only this owned fixture's POST /rpc handler lifetime. They
// do not identify a CDP request or prove status, body delivery or navigation.
// The writer and request reach the next handler unchanged. No body/header,
// URL/query, remote identity or extra RPC call is retained or introduced.
type marketRPCProgress struct {
	mu                                  sync.Mutex
	started, finished, normal, abnormal uint64
	overflow, untracked                 uint64
	active                              map[uint64]marketRPCStart
	recent                              [marketRPCMaximumRecent]marketRPCRecent
	recentNext, recentCount             int
}

func newMarketRPCProgress() *marketRPCProgress {
	return &marketRPCProgress{active: make(map[uint64]marketRPCStart)}
}

func (p *marketRPCProgress) begin(ctx context.Context) marketRPCStart {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started++
	row := marketRPCStart{sequence: p.started, started: time.Now(), ctx: ctx,
		tracked: len(p.active) < marketRPCMaximumActive}
	if row.tracked {
		p.active[row.sequence] = row
	} else {
		p.overflow++
		p.untracked++
	}
	return row
}

func (p *marketRPCProgress) finish(row marketRPCStart, normal bool) {
	entry := marketRPCRecent{Sequence: row.sequence, DurationMilliseconds: time.Since(row.started).Milliseconds(),
		ReturnedNormally: normal, ContextCanceled: row.ctx.Err() != nil}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finished++
	if normal {
		p.normal++
	} else {
		p.abnormal++
	}
	if row.tracked {
		delete(p.active, row.sequence)
	} else {
		p.untracked--
	}
	p.recent[p.recentNext] = entry
	p.recentNext = (p.recentNext + 1) % marketRPCMaximumRecent
	if p.recentCount < marketRPCMaximumRecent {
		p.recentCount++
	}
}

func (p *marketRPCProgress) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
			next.ServeHTTP(w, r)
			return
		}
		row := p.begin(r.Context())
		normal := false
		defer func() { p.finish(row, normal) }()
		next.ServeHTTP(w, r)
		normal = true
	})
}

func (p *marketRPCProgress) snapshot() marketRPCSnapshot {
	p.mu.Lock()
	value := marketRPCSnapshot{Version: 1, Scope: "owned-loopback-POST-rpc-handler-lifecycle",
		MaximumTrackedActive: marketRPCMaximumActive, MaximumRecent: marketRPCMaximumRecent,
		Started: p.started, Finished: p.finished, NormalReturns: p.normal, AbnormalExits: p.abnormal,
		Active: p.started - p.finished, OverflowStarted: p.overflow, UntrackedActive: p.untracked,
		CompleteTracking: p.overflow == 0, ActiveRecords: make([]marketRPCActive, 0, len(p.active)),
		Recent: make([]marketRPCRecent, 0, p.recentCount)}
	active := make([]marketRPCStart, 0, len(p.active))
	for _, row := range p.active {
		active = append(active, row)
	}
	for i := 0; i < p.recentCount; i++ {
		value.Recent = append(value.Recent, p.recent[(p.recentNext-p.recentCount+i+marketRPCMaximumRecent)%marketRPCMaximumRecent])
	}
	p.mu.Unlock()
	for _, row := range active {
		value.ActiveRecords = append(value.ActiveRecords, marketRPCActive{Sequence: row.sequence,
			AgeMilliseconds: time.Since(row.started).Milliseconds(), ContextCanceled: row.ctx.Err() != nil})
	}
	sort.Slice(value.ActiveRecords, func(i, j int) bool { return value.ActiveRecords[i].Sequence < value.ActiveRecords[j].Sequence })
	return value
}

func (p *marketRPCProgress) log(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"phase": "AFTER_OWNED_BROWSER_JOIN", "status": "AVAILABLE", "snapshot": p.snapshot(),
		"qualifiesNavigation": false})
	if err != nil {
		t.Log(`OWNED-MARKET-RPC-PROGRESS {"phase":"AFTER_OWNED_BROWSER_JOIN","status":"UNAVAILABLE","reason":"ENCODING","qualifiesNavigation":false}`)
		return
	}
	t.Log("OWNED-MARKET-RPC-PROGRESS " + string(raw))
}

func TestMarketRPCProgress(t *testing.T) {
	t.Run("unobserved-routes", func(t *testing.T) {
		p := newMarketRPCProgress()
		for _, route := range []struct{ method, path string }{{"GET", "/rpc"}, {"POST", "/other"}} {
			r := httptest.NewRequest(route.method, route.path, nil)
			w := httptest.NewRecorder()
			p.observe(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
				if got != w || request != r {
					t.Fatal("observer replaced an unobserved argument")
				}
				got.WriteHeader(202)
			})).ServeHTTP(w, r)
			if w.Code != 202 {
				t.Fatal("unobserved handler did not execute")
			}
		}
		if p.snapshot().Started != 0 {
			t.Fatal("observer tracked an undeclared route")
		}
	})
	t.Run("unchanged-arguments-and-private-input", func(t *testing.T) {
		p := newMarketRPCProgress()
		r := httptest.NewRequest("POST", "/rpc?token=do-not-log", strings.NewReader("body-do-not-log"))
		r.Header.Set("Authorization", "header-do-not-log")
		w := httptest.NewRecorder()
		p.observe(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
			if got != w || request != r {
				t.Fatal("observer replaced the original HTTP argument")
			}
			raw, err := io.ReadAll(request.Body)
			if err != nil || string(raw) != "body-do-not-log" {
				t.Fatal("observer changed the original body")
			}
			if _, ok := got.(http.Flusher); !ok {
				t.Fatal("observer hid an optional writer interface")
			}
			got.WriteHeader(201)
			got.Write(raw)
		})).ServeHTTP(w, r)
		value := p.snapshot()
		if w.Code != 201 || w.Body.String() != "body-do-not-log" || value.Started != 1 || value.Finished != 1 || value.NormalReturns != 1 || value.Active != 0 {
			t.Fatal("pass-through or completion accounting differs")
		}
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), "do-not-log") {
			t.Fatal("observer retained private request material")
		}
		if value.HTTPStatusBodyOrDeliveryObserved || value.ProtocolRequestIdentityEstablished || value.ResponseWriterOrRequestReplaced {
			t.Fatal("diagnostic widened its authority")
		}
	})
	t.Run("already-canceled", func(t *testing.T) {
		p := newMarketRPCProgress()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/rpc", nil).WithContext(ctx))
		value := p.snapshot()
		if value.Finished != 1 || len(value.Recent) != 1 || !value.Recent[0].ContextCanceled {
			t.Fatal("canceled context observation missing")
		}
	})
	t.Run("active-context-cancellation", func(t *testing.T) {
		p := newMarketRPCProgress()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer func() { once.Do(func() { close(release) }); <-joined }()
		go func() {
			defer close(joined)
			p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/rpc", nil).WithContext(ctx))
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("owned context handler did not start")
		}
		before := p.snapshot()
		if before.Active != 1 || len(before.ActiveRecords) != 1 || before.ActiveRecords[0].ContextCanceled {
			t.Fatal("initial active context differs")
		}
		cancel()
		if !p.snapshot().ActiveRecords[0].ContextCanceled {
			t.Fatal("active cancellation was not observed")
		}
		once.Do(func() { close(release) })
		<-joined
		if value := p.snapshot(); value.Finished != 1 || !value.Recent[0].ContextCanceled {
			t.Fatal("terminal cancellation was not preserved")
		}
	})
	t.Run("panic-propagates", func(t *testing.T) {
		p := newMarketRPCProgress()
		func() {
			defer func() {
				if recover() != "declared handler panic" {
					t.Fatal("observer swallowed or replaced the panic")
				}
			}()
			p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("declared handler panic") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/rpc", nil))
		}()
		value := p.snapshot()
		if value.Finished != 1 || value.AbnormalExits != 1 || value.NormalReturns != 0 || value.Active != 0 || value.Recent[0].ReturnedNormally {
			t.Fatal("abnormal handler exit was counted as a normal return")
		}
	})
	t.Run("bounded-recent-history", func(t *testing.T) {
		p := newMarketRPCProgress()
		handler := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		for i := 0; i < marketRPCMaximumRecent+3; i++ {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/rpc", nil))
		}
		value := p.snapshot()
		if value.Started != marketRPCMaximumRecent+3 || len(value.Recent) != marketRPCMaximumRecent || value.Recent[0].Sequence != 4 || value.Recent[len(value.Recent)-1].Sequence != value.Started {
			t.Fatal("recent ring retained the wrong bounded window")
		}
	})
	t.Run("bounded-active-concurrency", func(t *testing.T) {
		p := newMarketRPCProgress()
		const count = marketRPCMaximumActive + 2
		started, release := make(chan struct{}, count), make(chan struct{})
		var group sync.WaitGroup
		var once sync.Once
		defer func() { once.Do(func() { close(release) }); group.Wait() }()
		handler := p.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { started <- struct{}{}; <-release }))
		for i := 0; i < count; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/rpc", nil))
			}()
		}
		for i := 0; i < count; i++ {
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("owned concurrent handler did not start")
			}
		}
		value := p.snapshot()
		if value.Active != count || len(value.ActiveRecords) != marketRPCMaximumActive || value.UntrackedActive != 2 || value.OverflowStarted != 2 || value.CompleteTracking {
			t.Fatal("active tracking exceeded its bound or hid overflow")
		}
		once.Do(func() { close(release) })
		group.Wait()
		value = p.snapshot()
		if value.Active != 0 || value.UntrackedActive != 0 || value.Finished != count || value.NormalReturns != count || len(value.Recent) != marketRPCMaximumRecent || value.CompleteTracking {
			t.Fatal("joined concurrency lost its honest overflow history")
		}
	})
}
