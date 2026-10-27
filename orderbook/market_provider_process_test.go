package orderbook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The unmodified pinned program is built once by the explicit browser job.
// All endpoints are this profile's loopback fixtures and the program uses only
// Core's deliberately public devnet relayer account. No external daemon or
// public RPC can be selected by the page or the signer.
func startProductionMarketRelayer(t *testing.T, parent context.Context, f *orderbook.ExecutedMarketFixture, source string) (string, func() int) {
	t.Helper()
	binary := os.Getenv("ZVM_BROWSER_RELAYER_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute pinned relayer program is required")
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Size() > 64<<20 {
		t.Fatal("bounded executable relayer program required")
	}
	build, err := buildinfo.ReadFile(binary)
	if err != nil || build.Path != "github.com/Zenoglyphs-VM/zvm-relayer/cmd/zvm-relayer" {
		t.Fatal("actual relayer executable build information is required")
	}
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, io.LimitReader(file, (64<<20)+1))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(f.Rollup())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "relayer-rollup.json")
	if err = os.WriteFile(config, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, binary, "relayer", "-config", config, "-l1", source, "-eth", f.ExecutionURL(), "-listen", address, "-accounts", "1", "-max-wait", "10ms")
	stdout, stderr := &executedBrowserOutput{limit: 64 << 10}, &executedBrowserOutput{limit: 128 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	finished := make(chan error, 1)
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { finished <- cmd.Wait() }()
	var once sync.Once
	exitCode := -1
	join := func() int {
		once.Do(func() {
			select {
			case err := <-finished:
				t.Errorf("actual relayer exited before its owned stop: %v", err)
				return
			default:
			}
			cancel()
			select {
			case err := <-finished:
				if exited, ok := err.(*exec.ExitError); ok && exited.ExitCode() == 1 && parent.Err() == nil && ctx.Err() == context.Canceled &&
					bytes.HasSuffix(bytes.TrimSpace(stderr.raw.Bytes()), []byte("zvm-relayer relayer: context canceled")) &&
					!bytes.Contains(stderr.raw.Bytes(), []byte("level=ERROR")) && !bytes.Contains(stderr.raw.Bytes(), []byte("level=WARN")) {
					exitCode = 1 // main reports Run's joined context cancellation as exit 1.
				} else {
					t.Errorf("actual relayer has an unexpected stop: %v; bounded stderr=%s", err, stderr.raw.String())
				}
			case <-time.After(5 * time.Second):
				t.Error("owned actual relayer process was not joined")
			}
		})
		return exitCode
	}
	t.Cleanup(func() { join() })
	url := "http://" + address
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for {
		response, err := client.Get(url + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			join()
			t.Fatal("actual pinned relayer HTTP endpoint did not start")
		}
		select {
		case <-parent.Done():
			join()
			t.Fatal("relayer startup exceeded its owned context")
		case <-time.After(20 * time.Millisecond):
		}
	}
	record, err := json.Marshal(map[string]any{"source": "5a72a223dd60d1ae4e974d420cadd85e6f9ee929", "core": "29e6d9b30be94de3772c850c7a1e9811585a8833",
		"sha256": hex.EncodeToString(hash.Sum(nil)), "go": build.GoVersion, "actualUnmodifiedRelayerCLI": true, "loopbackOnly": true, "publicDevnetAccount": true})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ACTUAL-MARKET-RELAYER-BUILD " + string(record))
	return url, join
}
