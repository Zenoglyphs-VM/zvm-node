package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type compilerProtocolTransport func(*http.Request) (*http.Response, error)

func (f compilerProtocolTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type compilerProtocolBody struct {
	reads, closed atomic.Int32
	reader        io.Reader
	err           error
}

func (b *compilerProtocolBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.err != nil {
		return 0, b.err
	}
	return b.reader.Read(p)
}
func (b *compilerProtocolBody) Close() error { b.closed.Add(1); return nil }

func TestRemoteCompilerFailureClassesOmitResponseDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{"busy", 503, "v1.busy", ErrCompilerBusy}, {"cache_full", 503, "v1.cache-full", ErrCompilerCacheFull},
		{"invalid", 400, "v1.invalid", ErrInvalid}, {"output", 400, "v1.output-limit", ErrCompilerOutputLimit},
		{"deadline", 504, "v1.deadline", context.DeadlineExceeded}, {"unavailable", 500, "v1.unavailable", ErrCompilerUnavailable},
		{"legacy_busy", 503, "", ErrCompilerBusy}, {"legacy_invalid", 400, "", ErrInvalid},
		{"legacy_deadline", 504, "", context.DeadlineExceeded}, {"legacy_private_failure", 500, "", ErrCompilerUnavailable},
		{"unknown_version", 503, "v2.busy", ErrCompilerUnavailable}, {"mismatched_status", 500, "v1.busy", ErrCompilerUnavailable},
		{"unauthorized", 401, "", ErrCompilerUnavailable}, {"redirect_status", 302, "", ErrCompilerUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &compilerProtocolBody{err: errors.New("synthetic-private-response-body")}
			runner := &RemoteRunner{URL: "http://compiler-fixture.invalid", Token: "fixture-token", Client: &http.Client{Transport: compilerProtocolTransport(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				if test.code != "" {
					h.Set(CompilerErrorHeader, test.code)
				}
				return &http.Response{StatusCode: test.status, Header: h, Body: body, ContentLength: -1, Request: r}, nil
			})}}
			out, err := runner.Compile(context.Background(), "0.8.30", []byte(`{}`))
			if out != nil || !errors.Is(err, test.want) || strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), runner.URL) || strings.Contains(err.Error(), runner.Token) {
				t.Fatal("remote diagnostic escaped or fixed failure class changed", err)
			}
			if body.reads.Load() != 0 || body.closed.Load() != 1 {
				t.Fatal("failure response was consumed or not closed")
			}
		})
	}
}

func TestRemoteCompilerTransportAndReadErrorsAreFixed(t *testing.T) {
	for _, kind := range []string{"transport", "body", "timeout", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			runner := &RemoteRunner{URL: "http://fixture-token@compiler-fixture.invalid", Client: &http.Client{Transport: compilerProtocolTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if kind == "transport" {
					return nil, errors.New("synthetic-private-upstream-diagnostic")
				}
				if kind == "timeout" {
					return nil, fmt.Errorf("synthetic-private-upstream-diagnostic: %w", context.DeadlineExceeded)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &compilerProtocolBody{err: errors.New("synthetic-private-read-diagnostic")}, ContentLength: -1, Request: r}, nil
			})}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "canceled" {
				cancel()
			}
			out, err := runner.Compile(ctx, "0.8.30", []byte(`{}`))
			if out != nil || err == nil || strings.Contains(err.Error(), "synthetic-private") || strings.Contains(err.Error(), "fixture-token") || strings.Contains(err.Error(), "compiler-fixture.invalid") {
				t.Fatal("transport/read diagnostic escaped", err)
			}
			if kind == "canceled" {
				if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
					t.Fatal("canceled caller reached transport")
				}
			} else if !errors.Is(err, ErrCompilerUnavailable) {
				t.Fatal("transport lost fixed unavailable class", err)
			}
			if kind == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("client timeout lost its type")
			}
		})
	}
}

func TestRemoteCompilerRefusesEndpointRedirectAndPreservesClient(t *testing.T) {
	var redirected, callback atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.Write([]byte(`{}`)) }))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	check := func(*http.Request, []*http.Request) error { callback.Add(1); return nil }
	client := &http.Client{Timeout: time.Second, CheckRedirect: check}
	runner := &RemoteRunner{URL: source.URL, Token: "fixture-token", Client: client}
	if _, err := runner.Compile(context.Background(), "0.8.30", []byte(`{}`)); !errors.Is(err, ErrCompilerUnavailable) {
		t.Fatal("redirect did not refuse", err)
	}
	if redirected.Load() != 0 || callback.Load() != 0 || client.Timeout != time.Second || reflect.ValueOf(client.CheckRedirect).Pointer() != reflect.ValueOf(check).Pointer() {
		t.Fatal("redirect changed provider or caller-owned client options")
	}
}

func TestRemoteCompilerSuccessAndProtocolHeadersRemainBounded(t *testing.T) {
	for _, test := range []struct {
		name   string
		length int64
		body   string
		codes  []string
		want   error
		reads  bool
	}{
		{"exact", 8, "12345678", nil, nil, true}, {"declared_overflow", 9, "123456789", nil, ErrCompilerOutputLimit, false},
		{"stream_overflow", -1, "123456789", nil, ErrCompilerOutputLimit, true},
		{"failure_code_on_success", 8, "12345678", []string{"v1.invalid"}, ErrCompilerUnavailable, false},
		{"duplicate_code", 8, "12345678", []string{"v1.busy", "v1.invalid"}, ErrCompilerUnavailable, false},
		{"empty_code", 8, "12345678", []string{""}, ErrCompilerUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &compilerProtocolBody{reader: strings.NewReader(test.body)}
			runner := &RemoteRunner{URL: "http://compiler-fixture.invalid", MaxOutputBytes: 8, Client: &http.Client{Transport: compilerProtocolTransport(func(r *http.Request) (*http.Response, error) {
				header := make(http.Header)
				for _, code := range test.codes {
					header.Add(CompilerErrorHeader, code)
				}
				return &http.Response{StatusCode: 200, Header: header, ContentLength: test.length, Body: body, Request: r}, nil
			})}}
			out, err := runner.Compile(context.Background(), "0.8.30", []byte(`{}`))
			if test.want == nil {
				if err != nil || !bytes.Equal(out, []byte(test.body)) {
					t.Fatal("exact bounded success changed", err)
				}
			} else if !errors.Is(err, test.want) || out != nil {
				t.Fatal("bounded response became partial success", err)
			}
			if (body.reads.Load() > 0) != test.reads || body.closed.Load() != 1 {
				t.Fatal("response work exceeded its declared read/close contract")
			}
		})
	}
}

func TestCompilerVersionLimitPrecedesRegistryAndCompilerWork(t *testing.T) {
	var registry, transport atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { registry.Add(1); w.Write([]byte(`{"builds":[]}`)) }))
	defer server.Close()
	compilers := &Compilers{BaseURL: server.URL}
	local := &LocalRunner{}
	remote := &RemoteRunner{URL: "http://compiler-fixture.invalid", Client: &http.Client{Transport: compilerProtocolTransport(func(*http.Request) (*http.Response, error) {
		transport.Add(1)
		return nil, errors.New("unexpected transport")
	})}}
	for _, version := range []string{"", "v", strings.Repeat("x", MaxCompilerVersionBytes+1)} {
		if _, err := compilers.Resolve(context.Background(), version); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid name reached registry", err)
		}
		if _, err := local.Compile(context.Background(), version, []byte(`{}`)); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid name reached local compiler", err)
		}
		if _, err := remote.Compile(context.Background(), version, []byte(`{}`)); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid name reached remote compiler", err)
		}
	}
	if registry.Load() != 0 || transport.Load() != 0 {
		t.Fatal("version refusal consumed remote work")
	}
	if err := CheckCompilerVersion("v0.8.30+commit.73712a01"); err != nil {
		t.Fatal("ordinary version refused", err)
	}
}

func TestRemoteCompilerFailuresDoNotPersistPrivateJobMessagesOrRetry(t *testing.T) {
	env := newVerifyEnv(t)
	var requests atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !healthy.Load() {
			w.Header().Set(CompilerErrorHeader, "v1.cache-full")
			http.Error(w, "synthetic-private-remote-job-diagnostic", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(compiledAs(runtimeHex("0000", "11"))))
	}))
	defer server.Close()
	env.s.runner = &RemoteRunner{URL: server.URL}
	job := env.run(t, source("remote capacity failure"))
	if job.Status != "failed" || !strings.Contains(job.Message, ErrCompilerCacheFull.Error()) || strings.Contains(job.Message, "synthetic-private") || strings.Contains(job.Message, server.URL) {
		t.Fatal("durable verification job retained a remote diagnostic or wrong failure class", job.Status, job.Message)
	}
	if requests.Load() != 1 || env.count(t, "SELECT COUNT(*) FROM verify_jobs") != 1 || env.count(t, "SELECT COUNT(*) FROM verify_inputs") != 0 {
		t.Fatal("remote failure retried, duplicated a job or retained failed input")
	}
	healthy.Store(true)
	next := env.run(t, source("independent later request"))
	if next.Status != "verified" || next.Match != Exact || requests.Load() != 2 || env.count(t, "SELECT COUNT(*) FROM verify_jobs") != 2 || env.count(t, "SELECT COUNT(*) FROM verified_contracts") != 1 {
		t.Fatal("independent later verification did not preserve the original match/job lifecycle", next.Status, next.Message)
	}
}
