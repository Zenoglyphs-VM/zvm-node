package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

type compileFixture func(context.Context, string, []byte) ([]byte, error)

func (f compileFixture) Compile(ctx context.Context, compiler string, input []byte) ([]byte, error) {
	return f(ctx, compiler, input)
}

func TestSolcdExecutionFailuresDoNotExposePrivateFilesystemDiagnostics(t *testing.T) {
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		return nil, errors.New("open /private/fixture-secret: permission denied")
	})
	mux := solcdHandler(runner, "", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(`{"compiler":"fixture","input":{}}`)))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "fixture-secret") || strings.Contains(rec.Body.String(), "/private/") {
		t.Fatalf("raw filesystem error escaped the service: HTTP %d body=%q", rec.Code, rec.Body.String())
	}
}

type countedBody struct{ reads atomic.Int32 }

func (b *countedBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }

func TestSolcdRefusesExcessRequestsBeforeReadingTheirBodies(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		close(started)
		<-release
		return []byte(`{}`), nil
	})
	mux := solcdHandler(runner, "", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := httptest.NewRecorder()
	go func() {
		defer close(done)
		mux.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(`{"compiler":"fixture","input":{}}`)))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first compile did not start")
	}
	body := &countedBody{}
	excess := httptest.NewRecorder()
	mux.ServeHTTP(excess, httptest.NewRequest(http.MethodPost, "/compile", body))
	if excess.Code != http.StatusServiceUnavailable || excess.Header().Get("Retry-After") != "1" || body.reads.Load() != 0 {
		t.Fatalf("excess body consumed capacity: HTTP %d reads=%d", excess.Code, body.reads.Load())
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("first compile did not finish")
	}
	if first.Code != http.StatusOK {
		t.Fatalf("admitted compile failed: HTTP %d", first.Code)
	}
}

func TestSolcdAuthenticationAndStrictRequestShape(t *testing.T) {
	var called atomic.Int32
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		called.Add(1)
		return []byte(`{}`), nil
	})
	mux := solcdHandler(runner, "fixture-token", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := &countedBody{}
	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/compile", body))
	if unauthorized.Code != http.StatusUnauthorized || body.reads.Load() != 0 {
		t.Fatal("unauthenticated request read a body")
	}
	for _, input := range []string{`{"compiler":"fixture","input":{},"unknown":true}`, `{"compiler":"fixture","input":{}}{}`} {
		req := httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(input))
		req.Header.Set("Authorization", "Bearer fixture-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || called.Load() != 0 {
			t.Fatalf("malformed request reached the compiler: HTTP %d calls=%d", rec.Code, called.Load())
		}
	}
}

func TestSolcdCacheRefusalReleasesTheRequestSlot(t *testing.T) {
	var called atomic.Int32
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		called.Add(1)
		return nil, verify.ErrCompilerCacheFull
	})
	mux := solcdHandler(runner, "", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(`{"compiler":"fixture","input":{}}`)))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("cache refusal did not expose retryable capacity: HTTP %d", rec.Code)
		}
	}
	if called.Load() != 2 {
		t.Fatal("cache refusal leaked its request slot")
	}
	status := httptest.NewRecorder()
	mux.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status", nil))
	if strings.Contains(status.Body.String(), "solcDir") || !strings.Contains(status.Body.String(), `"maxConcurrent":1`) {
		t.Fatalf("status exposes paths or omits its capacity: %s", status.Body.String())
	}
}
