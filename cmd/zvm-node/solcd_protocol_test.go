package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestSolcdRemoteClientPreservesTypedFailuresWithoutDetails(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		status  int
		code    string
		want    error
	}{
		{"busy", verify.ErrCompilerBusy, 503, "v1.busy", verify.ErrCompilerBusy},
		{"cache", verify.ErrCompilerCacheFull, 503, "v1.cache-full", verify.ErrCompilerCacheFull},
		{"invalid", verify.ErrInvalid, 400, "v1.invalid", verify.ErrInvalid},
		{"output", verify.ErrCompilerOutputLimit, 400, "v1.output-limit", verify.ErrCompilerOutputLimit},
		{"deadline", context.DeadlineExceeded, 504, "v1.deadline", context.DeadlineExceeded},
		{"private", errors.New("synthetic-private-execution-detail"), 500, "v1.unavailable", verify.ErrCompilerUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
				return nil, fmt.Errorf("synthetic-private-wrapper: %w", test.failure)
			})
			handler := solcdHandler(runner, "fixture-token", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &verify.RemoteRunner{URL: server.URL, Token: "fixture-token"}
			out, err := client.Compile(context.Background(), "fixture", []byte(`{}`))
			if out != nil || !errors.Is(err, test.want) || strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("node/service typed failure boundary changed", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(`{"compiler":"fixture","input":{}}`))
			request.Header.Set("Authorization", "Bearer fixture-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Header().Get(verify.CompilerErrorHeader) != test.code || strings.Contains(response.Body.String(), "synthetic-private") {
				t.Fatal("service exported an unexpected protocol code/body")
			}
		})
	}
}

func TestSolcdInvalidVersionNeverReachesRunnerOrLog(t *testing.T) {
	var called atomic.Int32
	var logs bytes.Buffer
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		called.Add(1)
		return nil, errors.New("unexpected compile")
	})
	handler := solcdHandler(runner, "", 1, time.Second, slog.New(slog.NewTextHandler(&logs, nil)))
	version := strings.Repeat("x", verify.MaxCompilerVersionBytes+1)
	request := httptest.NewRequest(http.MethodPost, "/compile", strings.NewReader(`{"compiler":"`+version+`","input":{}}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || response.Header().Get(verify.CompilerErrorHeader) != "v1.invalid" || called.Load() != 0 || strings.Contains(logs.String(), version) {
		t.Fatal("invalid version reached compile or diagnostics")
	}
}

func TestSolcdBusyClientAndIndependentCompletionKeepSlots(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	runner := compileFixture(func(context.Context, string, []byte) ([]byte, error) {
		close(started)
		<-release
		return []byte(`{"contracts":{}}`), nil
	})
	server := httptest.NewServer(solcdHandler(runner, "", 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer func() { once.Do(func() { close(release) }); server.Close() }()
	client := &verify.RemoteRunner{URL: server.URL}
	go func() { _, err := client.Compile(context.Background(), "fixture", []byte(`{}`)); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first remote compile did not start")
	}
	if _, err := client.Compile(context.Background(), "fixture", []byte(`{}`)); !errors.Is(err, verify.ErrCompilerBusy) {
		t.Fatal("busy service lost its typed client refusal", err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("busy follower canceled independent work", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted client did not join")
	}
}
