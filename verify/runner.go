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
}

func (r *LocalRunner) Compile(ctx context.Context, compiler string, input []byte) ([]byte, error) {
	build, err := r.Compilers.Resolve(ctx, compiler)
	if err != nil {
		return nil, err
	}
	bin, err := r.Compilers.Path(ctx, build)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "zvm-solc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "--standard-json")
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() != nil {
			return nil, fmt.Errorf("solc %s did not finish within %s", build.LongVersion, timeout)
		}
		// The file is there, so "not found" is its loader: several releases
		// (0.8.12–0.8.23 among them) are linked against glibc.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("solc %s needs glibc, which this node's system lacks (on alpine, install gcompat)", build.LongVersion)
		}
		return nil, fmt.Errorf("solc %s failed to run: %v", build.LongVersion, err)
	}
	return stdout.Bytes(), nil
}

// RemoteRunner sends the work to a `zvm solcd` service.
type RemoteRunner struct {
	URL    string
	Token  string
	Client *http.Client
}

type compileRequest struct {
	Compiler string          `json:"compiler"`
	Input    json.RawMessage `json:"input"`
}

func (r *RemoteRunner) Compile(ctx context.Context, compiler string, input []byte) ([]byte, error) {
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
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Its errors are solc's, already stripped of file excerpts.
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return out, nil
}
