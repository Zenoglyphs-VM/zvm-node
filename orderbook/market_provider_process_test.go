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
	"github.com/ethereum/go-ethereum/common"
)

type executedRelayerProcess struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	finished chan error
	stderr   *executedBrowserOutput
	once     sync.Once
	exit     int
}

func (p *executedRelayerProcess) stop(t *testing.T, parent context.Context) int {
	t.Helper()
	p.once.Do(func() {
		select {
		case err := <-p.finished:
			t.Errorf("actual relayer exited before its owned stop: %v", err)
			return
		default:
		}
		p.cancel()
		select {
		case err := <-p.finished:
			if exited, ok := err.(*exec.ExitError); ok && exited.ExitCode() == 1 && parent.Err() == nil &&
				bytes.HasSuffix(bytes.TrimSpace(p.stderr.raw.Bytes()), []byte("zvm-relayer relayer: context canceled")) &&
				!bytes.Contains(p.stderr.raw.Bytes(), []byte("level=ERROR")) && !bytes.Contains(p.stderr.raw.Bytes(), []byte("level=WARN")) {
				p.exit = 1
			} else {
				t.Errorf("actual relayer has an unexpected stop: %v; bounded stderr=%s", err, p.stderr.raw.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("owned actual relayer process was not joined")
		}
	})
	return p.exit
}

// Build once, verify the exact source in the binary, and reuse it across the
// original browser profiles and an actual reopen between two wallet actions.
func startProductionMarketRelayer(t *testing.T, parent context.Context, f *orderbook.ExecutedMarketFixture, source string) (string, func() int, func()) {
	t.Helper()
	url, join, reopen, _, _ := startProductionMarketRelayerWithAdmission(t, parent, f, source, false)
	return url, join, reopen
}

func startProductionMarketRelayerWithAdmission(t *testing.T, parent context.Context, f *orderbook.ExecutedMarketFixture, source string, durable bool) (string, func() int, func(), func(common.Hash, []byte), func(*orderbook.ExecutedMarketProviderFixture, int)) {
	t.Helper()
	binary := os.Getenv("ZVM_BROWSER_RELAYER_BINARY")
	info, err := os.Stat(binary)
	if !filepath.IsAbs(binary) || err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Size() > 64<<20 {
		t.Fatal("bounded executable relayer program required")
	}
	build, err := buildinfo.ReadFile(binary)
	if err != nil || build.Path != "github.com/Zenoglyphs-VM/zvm-relayer/cmd/zvm-relayer" {
		t.Fatal("actual relayer executable build information is required")
	}
	settings := map[string]string{}
	for _, value := range build.Settings {
		if _, exists := settings[value.Key]; exists {
			t.Fatal("duplicate binary build setting")
		}
		settings[value.Key] = value.Value
	}
	if settings["vcs.revision"] != orderbook.ExecutedMarketRelayerSource || settings["vcs.modified"] != "false" {
		t.Fatal("relayer binary does not bind the exact clean candidate source")
	}
	file, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (64<<20)+1))
	file.Close()
	if err != nil || n != info.Size() {
		t.Fatal("relayer binary hash read was incomplete")
	}
	encoded, err := json.Marshal(f.Rollup())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "relayer-rollup.json")
	if err := os.WriteFile(config, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	directory := ""
	if durable {
		directory = filepath.Join(t.TempDir(), "admissions")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	url := "http://" + address
	var current *executedRelayerProcess
	lifetimes := 0
	launch := func(wait string) *executedRelayerProcess {
		t.Helper()
		actual, err := os.Stat(binary)
		if err != nil || !os.SameFile(info, actual) || actual.Size() != info.Size() {
			t.Fatal("pinned relayer executable identity changed")
		}
		check, err := os.Open(binary)
		if err != nil {
			t.Fatal(err)
		}
		currentHash := sha256.New()
		readBytes, hashErr := io.Copy(currentHash, io.LimitReader(check, (64<<20)+1))
		check.Close()
		if hashErr != nil || readBytes != info.Size() || !bytes.Equal(currentHash.Sum(nil), hash.Sum(nil)) {
			t.Fatal("pinned relayer executable bytes changed before process start")
		}
		ctx, cancel := context.WithCancel(parent)
		args := []string{"relayer", "-config", config, "-l1", source, "-eth", f.ExecutionURL(), "-listen", address, "-accounts", "1", "-max-wait", wait}
		if durable {
			args = append(args, "-admission-store", directory, "-admission-store-max-records", "4", "-admission-store-max-raw-bytes", "65536")
		}
		cmd := exec.CommandContext(ctx, binary, args...)
		stdout, stderr := &executedBrowserOutput{limit: 64 << 10}, &executedBrowserOutput{limit: 128 << 10}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 3 * time.Second
		process := &executedRelayerProcess{cmd: cmd, cancel: cancel, finished: make(chan error, 1), stderr: stderr, exit: -1}
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		lifetimes++
		current = process // Cleanup can join even a failed readiness attempt.
		go func() { process.finished <- cmd.Wait() }()
		client := &http.Client{Timeout: 200 * time.Millisecond}
		deadline := time.Now().Add(10 * time.Second)
		for {
			response, err := client.Get(url + "/health")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return process
				}
			}
			if time.Now().After(deadline) {
				process.stop(t, parent)
				t.Fatal("actual pinned relayer endpoint did not finish complete recovery")
			}
			select {
			case <-parent.Done():
				process.stop(t, parent)
				t.Fatal("relayer startup exceeded owned context")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	join := func() int {
		if current == nil {
			return -1
		}
		return current.stop(t, parent)
	}
	t.Cleanup(func() { join() })
	initialWait := "10ms"
	if durable {
		initialWait = "1h"
	}
	current = launch(initialWait)
	reopens := 0
	queuedRecovered := false
	var queuedWire []byte
	publicationRecoveries := 0
	lifecycle := &marketDurableWalletLifecycle{parent: parent, endpoint: url, directory: directory, config: config, current: &current, launch: launch, lifetimes: &lifetimes}
	reopen := func() {
		t.Helper()
		if reopens != 0 || (durable && (!queuedRecovered || publicationRecoveries != 1)) || join() != 1 {
			t.Fatal("declared single relayer reopen did not join its first process")
		}
		current = launch("10ms")
		reopens++
		if durable {
			durableMarketSnapshot(t, url, len(queuedWire), 0)
		} else {
			response, err := (&http.Client{Timeout: time.Second}).Get(url + "/status")
			if err != nil {
				t.Fatal(err)
			}
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
			response.Body.Close()
			var status struct {
				Ready                 *bool
				RecoveryStatus        string
				AdmissionMode         string
				Queued, PostedPending *int
				RetainedRawBytes      *int64
			}
			if readErr != nil || len(raw) > 64<<10 || response.StatusCode != 200 || json.Unmarshal(raw, &status) != nil ||
				status.Ready == nil || !*status.Ready || status.RecoveryStatus != "complete" || status.AdmissionMode != "volatile" ||
				status.Queued == nil || *status.Queued != 0 || status.PostedPending == nil || *status.PostedPending != 0 || status.RetainedRawBytes == nil || *status.RetainedRawBytes != 0 {
				t.Fatal("executed first nonce was retained or admission enabled before complete recovery")
			}
		}
		recordValue := map[string]any{"source": orderbook.ExecutedMarketRelayerSource, "sameEndpointAndBinary": true,
			"oldProcessJoined": true, "oldExit": 1, "newProcessReadyAfterCompleteRecovery": true,
			"queued": 0, "postedPending": 0, "retainedRawBytes": 0, "actualProcessLifetimesSoFar": 2,
			"defaultAdmissionMode": "volatile", "realWalletOrPublicNetworkOperations": 0}
		marker := "ACTUAL-MARKET-RELAYER-REOPEN "
		if durable {
			marker = "ACTUAL-MARKET-DURABLE-EXECUTED-RECOVERY "
			recordValue["actualProcessLifetimesSoFar"] = lifetimes
			recordValue["admissionMode"] = "local-sync"
			recordValue["originalQueuedACKKernelKillJoined"] = true
			recordValue["retainedAuditRecords"] = 1
			recordValue["retainedAuditRawBytes"] = len(queuedWire)
			recordValue["auditBytesPruned"] = false
		}
		record, err := json.Marshal(recordValue)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(marker + string(record))
	}
	recoverQueued := func(hash common.Hash, raw []byte) {
		t.Helper()
		if !durable || queuedRecovered || reopens != 0 || len(raw) == 0 || len(raw) >= 1024 {
			t.Fatal("undeclared durable wallet recovery")
		}
		before := durableMarketSnapshot(t, url, len(raw), 1)
		if *before.Queued != 1 || *before.PostedPending != 0 {
			t.Fatal("actual wallet ACK was not held in the queued phase")
		}
		current.kernelKill(t, parent)
		wireSHA := inspectQueuedWalletWire(t, directory, config, hash, raw)
		current = launch("10ms")
		after := durableMarketSnapshot(t, url, len(raw), 1)
		queuedRecovered = true
		queuedWire = append([]byte(nil), raw...)
		lifecycle.firstHash = hash
		lifecycle.firstRaw = append([]byte(nil), raw...)
		record, err := json.Marshal(map[string]any{"source": orderbook.ExecutedMarketRelayerSource,
			"admissionMode": "local-sync", "actualOriginalWalletHashACK": true, "queuedBeforeKill": 1, "postedBeforeKill": 0,
			"publicationBeforeKill": 0, "actualKernelSignal": 9, "killedProcessJoined": true, "actualQueuedStoreWireVerified": true,
			"storedSignedRawSHA256": wireSHA, "storedSignedRawBytes": len(raw), "sameStoreAndEndpoint": true,
			"sameExecutableIdentityAndSHA256": true, "newProcessReadyAfterCompleteRecovery": true,
			"queuedAfterReopen": *after.Queued, "postedAfterReopen": *after.PostedPending,
			"activeAfterReopen": 1, "retainedStoreRecords": 1, "retainedSignedBytes": len(raw), "storeHealthy": true,
			"storeRecordLimit": 4, "storeSignedByteLimit": 65536, "walletSubmissionsBeforeAndAfterReopen": 1,
			"realWalletOrPublicNetworkOperations": 0})
		if err != nil {
			t.Fatal(err)
		}
		t.Log("ACTUAL-MARKET-DURABLE-QUEUED-ACK-RECOVERY " + string(record))
	}
	recoverPublication := func(transport *orderbook.ExecutedMarketProviderFixture, index int) {
		t.Helper()
		if !durable || !queuedRecovered || index < 0 || index > 1 || publicationRecoveries != index || reopens != index {
			t.Fatal("undeclared durable wallet publication recovery order")
		}
		lifecycle.recover(t, transport, index)
		publicationRecoveries++
	}

	record, err := json.Marshal(map[string]any{"source": orderbook.ExecutedMarketRelayerSource, "core": "29e6d9b30be94de3772c850c7a1e9811585a8833",
		"sha256": hex.EncodeToString(hash.Sum(nil)), "go": build.GoVersion, "actualUnmodifiedRelayerCLI": true, "loopbackOnly": true, "publicDevnetAccount": true,
		"repository": orderbook.ExecutedMarketRelayerRepository, "embeddedExactVCSRevisionAndCleanTreeVerified": true})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ACTUAL-MARKET-RELAYER-BUILD " + string(record))
	return url, join, reopen, recoverQueued, recoverPublication
}
