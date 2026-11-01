package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type compilerProfileLog struct {
	mu       sync.Mutex
	raw      []byte
	overflow bool
}

func (b *compilerProfileLog) Write(raw []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(raw), (32<<10)-len(b.raw))
	b.raw = append(b.raw, raw[:n]...)
	b.overflow = b.overflow || n < len(raw)
	return len(raw), nil
}
func (b *compilerProfileLog) summary() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sum := sha256.Sum256(b.raw)
	return hex.EncodeToString(sum[:]), b.overflow
}

func compilerProfileRead(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		t.Fatal("kernel profile file incomplete or oversized")
	}
	return raw
}
func compilerProfileNumber(t *testing.T, path string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimSpace(string(compilerProfileRead(t, path))), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func compilerProfileOOM(t *testing.T) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(string(compilerProfileRead(t, "/sys/fs/cgroup/memory.events"))), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatal("invalid memory event")
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out[fields[0]] = n
	}
	if _, ok := out["oom"]; !ok {
		t.Fatal("OOM counter missing")
	}
	if _, ok := out["oom_kill"]; !ok {
		t.Fatal("OOM kill counter missing")
	}
	return out
}
func compilerProfileKernel(t *testing.T) {
	t.Helper()
	if os.Getuid() != 10001 || os.Getgid() != 10001 {
		t.Fatal("declared unprivileged service identity is absent")
	}
	if compilerProfileNumber(t, "/sys/fs/cgroup/memory.max") != 512<<20 || compilerProfileNumber(t, "/sys/fs/cgroup/memory.swap.max") != 0 || compilerProfileNumber(t, "/sys/fs/cgroup/pids.max") != 64 {
		t.Fatal("declared memory/swap/PID limits are absent")
	}
	cpu := strings.Fields(string(compilerProfileRead(t, "/sys/fs/cgroup/cpu.max")))
	if len(cpu) != 2 {
		t.Fatal("CPU limit missing")
	}
	quota, e1 := strconv.ParseUint(cpu[0], 10, 64)
	period, e2 := strconv.ParseUint(cpu[1], 10, 64)
	if e1 != nil || e2 != nil || period == 0 || quota/period != 2 || quota%period != 0 {
		t.Fatal("declared two-CPU limit absent")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(compilerProfileRead(t, "/proc/self/status")), "\n") {
		p := strings.SplitN(line, ":", 2)
		if len(p) == 2 {
			fields[p[0]] = strings.TrimSpace(p[1])
		}
	}
	if fields["NoNewPrivs"] != "1" || fields["CapEff"] != "0000000000000000" {
		t.Fatal("privilege/capability isolation is absent")
	}
	var root, scratch syscall.Statfs_t
	if syscall.Statfs("/", &root) != nil || syscall.Statfs("/tmp", &scratch) != nil {
		t.Fatal("filesystem profile unavailable")
	}
	if root.Flags&1 == 0 || uint64(scratch.Type) != 0x01021994 || uint64(scratch.Blocks)*uint64(scratch.Bsize) != 64<<20 {
		t.Fatal("declared read-only root and 64 MiB tmpfs are absent")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range interfaces {
		if it.Flags&net.FlagLoopback == 0 {
			t.Fatal("non-loopback network interface present")
		}
	}
	if _, err := os.Stat("/var/run/docker.sock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Docker socket is visible to the verifier")
	}
	for _, name := range []string{"ZVM_N01_HOST_SECRET_PATH", "ZVM_N01_HOST_STATE_PATH"} {
		path := os.Getenv(name)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "'\r\n") {
			t.Fatal("explicit synthetic host-canary path missing")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("host-only node canary is visible inside the verifier")
		}
	}
	if values := compilerProfileOOM(t); values["oom"] != 0 || values["oom_kill"] != 0 {
		t.Fatal("container already experienced OOM")
	}
}

type compilerProfileFixture struct {
	t                                 *testing.T
	dir, version, second, marker, url string
	body, other                       []byte
	binaryReads                       atomic.Int32
}

func newCompilerProfileFixture(t *testing.T, mode string) *compilerProfileFixture {
	t.Helper()
	f := &compilerProfileFixture{t: t, dir: t.TempDir(), version: "0.0.2+commit.container", second: "0.0.3+commit.container"}
	f.marker = filepath.Join(f.dir, "started")
	script := "#!/bin/sh\n"
	switch mode {
	case "stdout":
		script += "while :; do printf 0123456789abcdef; done\n"
	case "stderr":
		script += "while :; do printf 0123456789abcdef >&2; done\n"
	case "deadline":
		script += "printf 1 > '" + f.marker + "'\nexec /bin/sleep 30\n"
	case "isolation":
		for _, name := range []string{"ZVM_N01_HOST_SECRET_PATH", "ZVM_N01_HOST_STATE_PATH"} {
			script += "test ! -e '" + os.Getenv(name) + "' || exit 91\n"
		}
		script += "test -z \"${N01_PARENT_SENTINEL-}\" || exit 92\ngrep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status || exit 93\ngrep -Eq '^CapEff:[[:space:]]+0+$' /proc/self/status || exit 94\nprintf '{\"contracts\":{},\"n01Isolated\":true}'\n"
	default:
		script += "printf '{\"contracts\":{}}'\n"
	}
	f.body = []byte(script)
	f.other = []byte("#!/bin/sh\nprintf '{\"contracts\":{}}'\n# second immutable fixture\n")
	if err := os.WriteFile(filepath.Join(f.dir, "solc-"+f.version), f.body, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/list.json") {
			first, second := sha256.Sum256(f.body), sha256.Sum256(f.other)
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]string{
				{"path": "first-fixture", "version": "0.0.2", "longVersion": f.version, "sha256": hex.EncodeToString(first[:])},
				{"path": "second-fixture", "version": "0.0.3", "longVersion": f.second, "sha256": hex.EncodeToString(second[:])}}})
			return
		}
		f.binaryReads.Add(1)
		if strings.HasSuffix(r.URL.Path, "/second-fixture") {
			_, _ = w.Write(f.other)
			return
		}
		f.t.Error("unexpected compiler binary download")
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

type compilerProfileProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	url     string
	logs    *compilerProfileLog
	stopped bool
}

func (f *compilerProfileFixture) start(binary string, cacheFiles int) *compilerProfileProcess {
	f.t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cmd := exec.Command(binary, "solcd", "-listen", address, "-solc-dir", f.dir, "-solc-url", f.url, "-workers", "1", "-timeout", "500ms",
		"-max-output-bytes", "65536", "-max-stderr-bytes", "4096", "-max-cache-bytes", "4194304", "-max-cache-files", strconv.Itoa(cacheFiles))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=/tmp", "GOMAXPROCS=2", "GOMEMLIMIT=128MiB", "N01_PARENT_SENTINEL=synthetic-parent-only"}
	logs := new(compilerProfileLog)
	cmd.Stdout = logs
	cmd.Stderr = logs
	p := &compilerProfileProcess{command: cmd, done: make(chan struct{}), url: "http://" + address, logs: logs}
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	f.t.Cleanup(func() { p.stop(f.t) })
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			f.t.Fatalf("actual solcd exited before readiness: %v", p.err)
		default:
		}
		response, err := client.Get(p.url + "/status")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatal("actual solcd did not become ready")
	return nil
}
func (p *compilerProfileProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	select {
	case <-p.done:
		if p.err != nil {
			t.Errorf("actual solcd exited unsuccessfully: %v", p.err)
		}
		return
	default:
	}
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Error(err)
	}
	select {
	case <-p.done:
		if p.err != nil {
			t.Errorf("actual solcd failed its graceful stop: %v", p.err)
		}
	case <-time.After(7 * time.Second):
		_ = p.command.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(time.Second):
		}
		t.Error("actual solcd did not join its declared stop bound")
	}
	if _, overflow := p.logs.summary(); overflow {
		t.Error("service diagnostics exceeded their capture budget")
	}
}
func compilerProfileRequest(p *compilerProfileProcess, version string) (int, []byte, error) {
	raw, _ := json.Marshal(map[string]any{"compiler": version, "input": map[string]any{"language": "Solidity", "sources": map[string]any{}}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/compile", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
	if err != nil || len(body) > 128<<10 {
		return 0, nil, fmt.Errorf("compile response incomplete or oversized")
	}
	return response.StatusCode, body, nil
}
func compilerProfileRecord(t *testing.T, record map[string]any) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(raw))
}

func TestCompilerServiceContainerProfile(t *testing.T) {
	if os.Getenv("ZVM_N01_CONTAINER_PROFILE") != "1" {
		t.Skip("requires the explicit actual solcd kernel-isolation job")
	}
	binary := os.Getenv("ZVM_N01_SOLCD_BINARY")
	if binary != "/qualified/zvm-node" {
		t.Fatal("declared production binary is missing")
	}
	compilerProfileKernel(t)
	before := compilerProfileOOM(t)
	t.Run("isolated_actual_compiler", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "isolation")
		p := f.start(binary, 2)
		status, body, err := compilerProfileRequest(p, f.version)
		if err != nil || status != 200 || !bytes.Contains(body, []byte(`"n01Isolated":true`)) || f.binaryReads.Load() != 0 {
			t.Fatalf("actual isolated compiler did not complete: status=%d error=%v", status, err)
		}
		p.stop(t)
		sum, _ := p.logs.summary()
		compilerProfileRecord(t, map[string]any{"case": "actual_solcd_compiler_isolation", "host_node_canaries_absent": true, "parent_sentinel_not_in_compiler": true, "compiler_no_new_privileges": true, "compiler_effective_capabilities_zero": true, "original_cached_binary_reused": true, "service_joined": true, "service_log_sha256": sum})
	})
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream+"_overflow", func(t *testing.T) {
			f := newCompilerProfileFixture(t, stream)
			p := f.start(binary, 2)
			for i := 0; i < 2; i++ {
				start := time.Now()
				status, body, err := compilerProfileRequest(p, f.version)
				if err != nil || status != 400 || !bytes.Contains(body, []byte("compiler output exceeded its byte limit")) || len(body) > 256 || time.Since(start) > 2*time.Second {
					t.Fatalf("compiler overflow was not bounded: status=%d error=%v", status, err)
				}
			}
			p.stop(t)
			compilerProfileRecord(t, map[string]any{"case": "actual_solcd_" + stream + "_overflow", "attempts": 2, "HTTP_status": 400, "partial_success": false, "slot_released": true, "service_joined": true})
		})
	}
	t.Run("deadline_backpressure_and_reuse", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "deadline")
		p := f.start(binary, 2)
		type result struct {
			status int
			err    error
		}
		finished := make(chan result, 1)
		started := time.Now()
		go func() { status, _, err := compilerProfileRequest(p, f.version); finished <- result{status, err} }()
		limit := time.Now().Add(2 * time.Second)
		marked := false
		for time.Now().Before(limit) {
			if _, err := os.Stat(f.marker); err == nil {
				marked = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !marked {
			t.Fatal("controlled compiler did not start")
		}
		status, _, err := compilerProfileRequest(p, f.version)
		if err != nil || status != 503 {
			t.Fatalf("busy compiler did not refuse the next caller: %d %v", status, err)
		}
		response, err := (&http.Client{Timeout: time.Second}).Get(p.url + "/status")
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("busy compiler hid status")
		}
		select {
		case result := <-finished:
			if result.err != nil || result.status != 504 {
				t.Fatalf("deadline lost its HTTP status: %d %v", result.status, result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed-out compile caller did not join")
		}
		elapsed := time.Since(started)
		if elapsed > 2*time.Second {
			t.Fatal("compiler timeout exceeded declared bound")
		}
		status, _, err = compilerProfileRequest(p, f.version)
		if err != nil || status != 504 {
			t.Fatal("timed-out compiler retained its request slot")
		}
		p.stop(t)
		compilerProfileRecord(t, map[string]any{"case": "actual_solcd_deadline_and_backpressure", "busy_HTTP_status": 503, "deadline_HTTP_status": 504, "status_reachable": true, "slot_released": true, "elapsed_milliseconds": elapsed.Milliseconds(), "service_joined": true})
	})
	t.Run("logical_cache_exhaustion", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "success")
		p := f.start(binary, 1)
		for i := 0; i < 2; i++ {
			status, _, err := compilerProfileRequest(p, f.second)
			if err != nil || status != 503 {
				t.Fatal("full logical cache did not refuse predictably")
			}
		}
		original, err := os.ReadFile(filepath.Join(f.dir, "solc-"+f.version))
		if err != nil || !bytes.Equal(original, f.body) || f.binaryReads.Load() != 0 {
			t.Fatal("full cache changed existing input or downloaded another binary")
		}
		status, _, err := compilerProfileRequest(p, f.version)
		if err != nil || status != 200 {
			t.Fatal("full cache refusal disabled a retained compiler")
		}
		p.stop(t)
		compilerProfileRecord(t, map[string]any{"case": "actual_solcd_logical_cache_exhaustion", "HTTP_status": 503, "new_binary_downloads": 0, "existing_binary_unchanged": true, "retained_compiler_still_usable": true, "service_joined": true})
	})
	t.Run("physical_tmpfs_exhaustion", func(t *testing.T) {
		f := newCompilerProfileFixture(t, "success")
		p := f.start(binary, 2)
		fillerPath := filepath.Join(t.TempDir(), "owned-isolated-tmpfs-filler")
		filler, err := os.OpenFile(fillerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(fillerPath)
		buffer := make([]byte, 1<<20)
		written := 0
		var diskErr error
		for written < 65<<20 {
			n, err := filler.Write(buffer)
			written += n
			if err != nil {
				diskErr = err
				break
			}
		}
		_ = filler.Close()
		if !errors.Is(diskErr, syscall.ENOSPC) || written > 64<<20 {
			t.Fatal("declared isolated tmpfs did not exhaust within its bound")
		}
		start := time.Now()
		status, body, err := compilerProfileRequest(p, f.second)
		if err != nil || status != 500 || time.Since(start) > 2*time.Second || bytes.Contains(body, []byte(f.dir)) {
			t.Fatalf("physical cache failure was not bounded/redacted: %d %v", status, err)
		}
		if err := os.Remove(fillerPath); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(filepath.Join(f.dir, "solc-"+f.version))
		if err != nil || !bytes.Equal(original, f.body) {
			t.Fatal("physical refusal changed the retained compiler")
		}
		status, _, err = compilerProfileRequest(p, f.second)
		if err != nil || status != 200 {
			t.Fatal("released isolated tmpfs did not permit a fresh compiler operation")
		}
		p.stop(t)
		compilerProfileRecord(t, map[string]any{"case": "actual_solcd_physical_cache_exhaustion", "HTTP_status": 500, "ENOSPC_observed": true, "tmpfs_filler_bytes": written, "existing_binary_unchanged": true, "slot_released": true, "later_compile_succeeded": true, "service_joined": true})
	})
	after := compilerProfileOOM(t)
	if after["oom"] != before["oom"] || after["oom_kill"] != before["oom_kill"] {
		t.Fatal("compiler profile caused an OOM event")
	}
	compilerProfileRecord(t, map[string]any{"case": "compiler_kernel_profile", "memory_max_bytes": 512 << 20, "swap_max_bytes": 0, "CPUs": 2, "pids_max": 64, "tmpfs_bytes": 64 << 20,
		"uid": 10001, "gid": 10001, "read_only_root": true, "network": "none", "effective_capabilities_zero": true, "no_new_privileges": true,
		"OOM_events": 0, "actual_production_service_lifetimes": 6, "joined_services": 6, "real_node_keys_state_or_live_chain_used": false, "actual_solc_vendor_binary_qualified": false})
}
