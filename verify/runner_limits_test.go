package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompilerCaptureCannotBypassItsLimitThroughIOCopy(t *testing.T) {
	canceled := false
	capture := &compilerCapture{limit: 8, cancel: func() { canceled = true }}
	// LimitedReader has no WriteTo fast path, so an embedded Buffer's
	// ReadFrom would bypass the capture's Write method here.
	_, err := io.Copy(capture, io.LimitReader(strings.NewReader(strings.Repeat("x", 32)), 32))
	if !errors.Is(err, ErrCompilerOutputLimit) || !canceled || capture.buffer.Len() != 8 {
		t.Fatalf("io.Copy bypassed the cap: bytes=%d canceled=%v err=%v", capture.buffer.Len(), canceled, err)
	}
}

func fixtureLocalRunner(t *testing.T, script string) *LocalRunner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the compiler-process fixture uses the POSIX shell")
	}
	dir := t.TempDir()
	const version = "0.0.2+commit.fixture"
	body := []byte("#!/bin/sh\n" + script + "\n")
	sum := sha256.Sum256(body)
	build := Build{LongVersion: version, Version: "0.0.2", Path: "solc-fixture", SHA256: hex.EncodeToString(sum[:])}
	if err := os.WriteFile(filepath.Join(dir, "solc-"+version), body, 0o700); err != nil {
		t.Fatal(err)
	}
	return &LocalRunner{Compilers: &Compilers{Dir: dir, list: &BuildList{Builds: []Build{build}}, listAt: time.Now()}, Timeout: time.Second, MaxOutputBytes: 64, MaxStderrBytes: 64}
}

func TestCompilerProcessOutputFloodsAreTerminatedWithoutPartialSuccess(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			script := "while :; do printf 0123456789abcdef; done"
			if stream == "stderr" {
				script = "while :; do printf 0123456789abcdef >&2; done"
			}
			runner := fixtureLocalRunner(t, script)
			start := time.Now()
			out, err := runner.Compile(context.Background(), "0.0.2", []byte(`{}`))
			if !errors.Is(err, ErrCompilerOutputLimit) || len(out) != 0 || time.Since(start) > 3*time.Second {
				t.Fatalf("output flood was not promptly refused: output=%d err=%v duration=%s", len(out), err, time.Since(start))
			}
			if len(runner.slots) != 0 {
				t.Fatal("failed compiler leaked its capacity reservation")
			}
		})
	}
}

func TestCompilerDeadlineAndExactOutputBoundary(t *testing.T) {
	runner := fixtureLocalRunner(t, "exec /bin/sleep 30")
	runner.Timeout = 100 * time.Millisecond
	start := time.Now()
	if out, err := runner.Compile(context.Background(), "0.0.2", []byte(`{}`)); len(out) != 0 || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("deadline was not preserved: output=%d err=%v duration=%s", len(out), err, time.Since(start))
	}
	runner = fixtureLocalRunner(t, `printf '{"contracts":{}}'`)
	runner.MaxOutputBytes = int64(len(`{"contracts":{}}`))
	if out, err := runner.Compile(context.Background(), "0.0.2", []byte(`{}`)); err != nil || string(out) != `{"contracts":{}}` {
		t.Fatalf("exact boundary was truncated or refused: output=%q err=%v", out, err)
	}
}

func TestCompilerConcurrencyIsReservedBeforeReleaseLookup(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	runner := &LocalRunner{Compilers: &Compilers{Dir: t.TempDir(), BaseURL: srv.URL}, MaxConcurrent: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := runner.Compile(ctx, "0.0.2", []byte(`{}`)); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture lookup did not start")
	}
	if _, err := runner.Compile(context.Background(), "0.0.2", []byte(`{}`)); !errors.Is(err, ErrCompilerBusy) {
		t.Fatalf("excess compile queued or bypassed capacity: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled lookup lost its cause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled lookup did not release capacity")
	}
}

func TestRemoteCompilerRefusesOverflowInsteadOfReturningATruncatedResult(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusBadRequest} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte("123456789"))
		}))
		runner := &RemoteRunner{URL: srv.URL, MaxOutputBytes: 8}
		out, err := runner.Compile(context.Background(), "fixture", []byte(`{}`))
		srv.Close()
		if !errors.Is(err, ErrCompilerOutputLimit) || len(out) != 0 {
			t.Fatalf("HTTP %d returned partial compiler output: %q err=%v", code, out, err)
		}
	}
}

func TestCompilerRejectsOversizedInputBeforeResolvingOrStarting(t *testing.T) {
	runner := &LocalRunner{MaxInputBytes: 1}
	if _, err := runner.Compile(context.Background(), "unused", []byte(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized input reached the absent compiler: %v", err)
	}
	if _, err := (&LocalRunner{MaxConcurrent: -1}).Compile(context.Background(), "unused", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative concurrency bypassed the limit: %v", err)
	}
	remote := &RemoteRunner{URL: "http://unused.invalid"}
	if _, err := remote.Compile(context.Background(), "unused", []byte(strings.Repeat("x", DefaultCompileInputBytes+1))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized remote input reached the network: %v", err)
	}
}
