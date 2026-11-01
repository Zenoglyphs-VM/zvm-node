package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A Runner compiles standard JSON input and returns solc's raw output.
//
// It is an interface because of where solc sits: it is a large C++ program run
// on input anyone can submit, and it runs next to the node's Engine API secret,
// its database and its media store. Filtering the input (no `urls`, no absolute
// imports, an empty environment, an empty working directory, a timeout) stops
// solc being *asked* for those files; it does nothing if solc itself is made to
// misbehave. Blockscout answers this by running verification as its own
// service, and so does this: `zvm solcd` runs solc and nothing else, in a
// container with no volumes of the node's, a read-only root and an unprivileged
// user (docker-compose.yml, the `verifier` service).
//
// LocalRunner stays the default because a devnet node with no verifier
// configured should still verify; the compose stack points at the service.
type Runner interface {
	Compile(ctx context.Context, compiler string, input []byte) ([]byte, error)
}

// LocalRunner runs solc in this process's own container.
type LocalRunner struct {
	Compilers *Compilers
	Timeout   time.Duration
	// Limits are fixed before first use. Zero selects the defaults below.
	MaxConcurrent  int
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxStderrBytes int64
	once           sync.Once
	slots          chan struct{}
}

const (
	DefaultCompileInputBytes  = 16 << 20
	DefaultCompileOutputBytes = 64 << 20
	DefaultCompileStderrBytes = 1 << 20
	DefaultCompileConcurrent  = 2
)

var (
	ErrCompilerBusy        = errors.New("compiler capacity is busy; try again shortly")
	ErrCompilerOutputLimit = errors.New("compiler output exceeded its byte limit")
)

// compilerCapture retains a bounded prefix, then cancels the child immediately.
// Returning an I/O error alone would leave an output-flooding child running
// until its deadline, or waiting forever for a pipe reader that stopped.
type compilerCapture struct {
	// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy
	// bypass Write and silently remove the byte bound on process pipes.
	buffer   bytes.Buffer
	limit    int64
	cancel   context.CancelFunc
	exceeded bool
}

func (b *compilerCapture) Write(p []byte) (int, error) {
	n := min(int64(len(p)), b.limit-int64(b.buffer.Len()))
	if n > 0 {
		b.buffer.Write(p[:n])
	}
	if n < int64(len(p)) {
		b.exceeded = true
		b.cancel()
		return int(n), ErrCompilerOutputLimit
	}
	return int(n), nil
}

func compileLimit(value, fallback int64) (int64, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 || value > 256<<20 {
		return 0, fmt.Errorf("%w: compiler byte limit must be positive and at most 256 MiB", ErrInvalid)
	}
	return value, nil
}

func (r *LocalRunner) Compile(ctx context.Context, compiler string, input []byte) ([]byte, error) {
	if r.MaxConcurrent < 0 || r.MaxConcurrent > 64 {
		return nil, fmt.Errorf("%w: compiler concurrency must be between 1 and 64", ErrInvalid)
	}
	r.once.Do(func() {
		n := r.MaxConcurrent
		if n == 0 {
			n = DefaultCompileConcurrent
		}
		r.slots = make(chan struct{}, n)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return nil, ErrCompilerBusy
	}
	inputLimit, err := compileLimit(r.MaxInputBytes, DefaultCompileInputBytes)
	if err != nil {
		return nil, err
	}
	outputLimit, err := compileLimit(r.MaxOutputBytes, DefaultCompileOutputBytes)
	if err != nil {
		return nil, err
	}
	stderrLimit, err := compileLimit(r.MaxStderrBytes, DefaultCompileStderrBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(input)) > inputLimit {
		return nil, fmt.Errorf("%w: compiler input is larger than %d bytes", ErrInvalid, inputLimit)
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	build, err := r.Compilers.Resolve(cctx, compiler)
	if err != nil {
		return nil, err
	}
	bin, err := r.Compilers.Path(cctx, build)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "zvm-solc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(cctx, bin, "--standard-json")
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(input)
	stdout := compilerCapture{limit: outputLimit, cancel: cancel}
	stderr := compilerCapture{limit: stderrLimit, cancel: cancel}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Bound inherited pipe descriptors too if the compiler starts a child.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("solc %s: %w", build.LongVersion, ErrCompilerOutputLimit)
	}
	if err != nil {
		if cctx.Err() != nil {
			return nil, fmt.Errorf("solc %s stopped before completion: %w", build.LongVersion, cctx.Err())
		}
		// The file is there, so "not found" is its loader: several releases
		// (0.8.12–0.8.23 among them) are linked against glibc.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("solc %s needs glibc, which this node's system lacks (on alpine, install gcompat)", build.LongVersion)
		}
		return nil, fmt.Errorf("solc %s failed to run: %v", build.LongVersion, err)
	}
	return stdout.buffer.Bytes(), nil
}

// RemoteRunner sends the work to a `zvm solcd` service.
type RemoteRunner struct {
	URL            string
	Token          string
	Client         *http.Client
	MaxOutputBytes int64
}

type compileRequest struct {
	Compiler string          `json:"compiler"`
	Input    json.RawMessage `json:"input"`
}

func (r *RemoteRunner) Compile(ctx context.Context, compiler string, input []byte) ([]byte, error) {
	if len(input) > DefaultCompileInputBytes {
		return nil, fmt.Errorf("%w: compiler input is larger than %d bytes", ErrInvalid, DefaultCompileInputBytes)
	}
	limit, err := compileLimit(r.MaxOutputBytes, DefaultCompileOutputBytes)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(compileRequest{Compiler: compiler, Input: input})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/compile", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the verifier service: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, ErrCompilerOutputLimit
	}
	if resp.StatusCode != http.StatusOK {
		// Its errors are solc's, already stripped of file excerpts.
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return out, nil
}
