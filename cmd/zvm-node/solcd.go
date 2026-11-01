package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

func init() {
	register("solcd", "run solc for the node, and nothing else: the sandboxed half of contract verification", runSolcd)
}

// solcd exists so that the node does not run a compiler.
//
// Verification compiles whatever a stranger submits. The input is filtered —
// no `urls`, no absolute imports, an empty environment and working directory, a
// timeout — but that only stops solc being *asked* to read the node's files. It
// does nothing if solc itself is made to misbehave, and next to the node sit
// the Engine API secret (which drives op-geth), the index, the media store and
// the upload session key. Blockscout separates verification into its own
// service for the same reason; this is that service.
//
// What makes it a sandbox is the container it runs in, not this file:
// docker-compose.yml's `verifier` gives it a read-only root filesystem, a
// tmpfs for scratch, an unprivileged user, no volumes of the node's, and CPU,
// memory and process limits. All this program adds is a door with one shape.
func runSolcd(args []string) error {
	fs := flag.NewFlagSet("solcd", flag.ExitOnError)
	listen := fs.String("listen", ":8600", "listen address")
	solcDir := fs.String("solc-dir", "/solc", "where fetched compilers are kept")
	solcURL := fs.String("solc-url", "", "mirror of binaries.soliditylang.org (default: the public host)")
	tokenFile := fs.String("token-file", "", "shared secret the node presents, if any")
	timeout := fs.Duration("timeout", 2*time.Minute, "how long one compile may take")
	workers := fs.Int("workers", verify.DefaultCompileConcurrent, "maximum concurrent requests; excess requests receive HTTP 503")
	outputBytes := fs.Int64("max-output-bytes", verify.DefaultCompileOutputBytes, "maximum compiler stdout bytes per request")
	stderrBytes := fs.Int64("max-stderr-bytes", verify.DefaultCompileStderrBytes, "maximum compiler stderr bytes per request")
	cacheBytes := fs.Int64("max-cache-bytes", verify.DefaultCompilerCacheBytes, "maximum retained compiler cache bytes; no automatic eviction")
	cacheFiles := fs.Int("max-cache-files", verify.DefaultCompilerCacheFiles, "maximum retained compiler cache files")
	fs.Parse(args)
	if *workers < 1 || *workers > 64 || *timeout <= 0 || *outputBytes < 1 || *outputBytes > 256<<20 || *stderrBytes < 1 || *stderrBytes > 256<<20 || *cacheBytes < 1 || *cacheFiles < 1 {
		return errors.New("compiler limits must be positive; concurrency is at most 64 and each output limit at most 256 MiB")
	}

	var token string
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(raw))
	}
	runner := &verify.LocalRunner{
		Compilers: &verify.Compilers{Dir: *solcDir, BaseURL: *solcURL, MaxCacheBytes: *cacheBytes, MaxCacheFiles: *cacheFiles},
		Timeout:   *timeout, MaxConcurrent: *workers, MaxOutputBytes: *outputBytes, MaxStderrBytes: *stderrBytes,
	}
	log := slog.Default().With("svc", "solcd")
	mux := solcdHandler(runner, token, *workers, *timeout, log)

	ctx, cancel := signalContext()
	defer cancel()
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 2 * time.Minute, WriteTimeout: 2*time.Minute + *timeout + 35*time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		srv.Shutdown(c)
	}()
	log.Info("solcd listening", "addr", *listen, "workers", *workers)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func solcdHandler(runner verify.Runner, token string, workers int, timeout time.Duration, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	slots := make(chan struct{}, workers)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "maxConcurrent": workers})
	})
	mux.HandleFunc("POST /compile", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Reserve capacity before reading a potentially large body. Limiting
		// only the compiler would leave concurrent JSON decoding unbounded.
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, verify.ErrCompilerBusy.Error(), http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Compiler string          `json:"compiler"`
			Input    json.RawMessage `json:"input"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout+30*time.Second)
		defer cancel()
		out, err := runner.Compile(ctx, req.Compiler, req.Input)
		if err != nil {
			// solc's own message, which the node already keeps free of file
			// excerpts; nothing about this process's filesystem goes back.
			log.Warn("compile failed", "compiler", req.Compiler, "err", err)
			status, message := http.StatusInternalServerError, "compiler execution failed"
			if errors.Is(err, verify.ErrCompilerBusy) || errors.Is(err, verify.ErrCompilerCacheFull) {
				status = http.StatusServiceUnavailable
				message = "compiler capacity is unavailable; try again shortly"
				w.Header().Set("Retry-After", "1")
			} else if errors.Is(err, verify.ErrInvalid) {
				status, message = http.StatusBadRequest, "invalid compiler request"
			} else if errors.Is(err, verify.ErrCompilerOutputLimit) {
				status, message = http.StatusBadRequest, verify.ErrCompilerOutputLimit.Error()
			} else if errors.Is(err, context.DeadlineExceeded) {
				status, message = http.StatusGatewayTimeout, "compiler deadline exceeded"
			}
			http.Error(w, message, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	})

	return mux
}
