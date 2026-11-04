package orderbook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/common"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

type executedDurableAdmissionSnapshot struct {
	Ready                 *bool
	RecoveryStatus        string
	AdmissionMode         string
	AdmissionDurable      *bool
	Queued, PostedPending *int
	RetainedRawBytes      *int64
	AdmissionStorage      *struct {
		Healthy                                    *bool
		RetainedRecords, ActiveRecords, MaxRecords *int
		RetainedRawBytes, MaxRawBytes              *int64
	}
}

func durableMarketSnapshot(t *testing.T, url string, rawBytes int, active int) executedDurableAdmissionSnapshot {
	t.Helper()
	return durableMarketRetainedSnapshot(t, url, int64(rawBytes*active), int64(rawBytes), active, 1)
}

func durableMarketRetainedSnapshot(t *testing.T, url string, activeRawBytes, retainedRawBytes int64, active, retainedRecords int) executedDurableAdmissionSnapshot {
	t.Helper()
	response, err := (&http.Client{Timeout: time.Second}).Get(url + "/status")
	if err != nil {
		t.Fatal("actual durable Relayer status was unavailable")
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	response.Body.Close()
	var status executedDurableAdmissionSnapshot
	if readErr != nil || len(raw) > 64<<10 || response.StatusCode != 200 || json.Unmarshal(raw, &status) != nil ||
		status.Ready == nil || !*status.Ready || status.RecoveryStatus != "complete" || status.AdmissionMode != "local-sync" ||
		status.AdmissionDurable == nil || !*status.AdmissionDurable || status.Queued == nil || status.PostedPending == nil ||
		*status.Queued < 0 || *status.PostedPending < 0 || *status.Queued+*status.PostedPending != active ||
		status.RetainedRawBytes == nil || *status.RetainedRawBytes != activeRawBytes || status.AdmissionStorage == nil {
		t.Fatal("actual durable Relayer readiness or active queue differs")
	}
	store := status.AdmissionStorage
	if store.Healthy == nil || !*store.Healthy || store.RetainedRecords == nil || *store.RetainedRecords != retainedRecords ||
		store.ActiveRecords == nil || *store.ActiveRecords != active || store.RetainedRawBytes == nil || *store.RetainedRawBytes != retainedRawBytes ||
		store.MaxRecords == nil || *store.MaxRecords != 4 || store.MaxRawBytes == nil || *store.MaxRawBytes != 65536 {
		t.Fatal("actual durable Relayer store ownership, retained bytes or explicit limits differ")
	}
	return status
}

// This is an actual kernel signal and a joined CLI, not a marker-only exit.
func (p *executedRelayerProcess) kernelKill(t *testing.T, parent context.Context) {
	t.Helper()
	p.once.Do(func() {
		defer p.cancel()
		select {
		case err := <-p.finished:
			t.Errorf("actual durable Relayer exited before SIGKILL: %v", err)
			return
		default:
		}
		if p.cmd == nil || p.cmd.Process == nil || p.cmd.Process.Kill() != nil {
			t.Error("actual durable Relayer SIGKILL was refused")
			return
		}
		select {
		case err := <-p.finished:
			exited, ok := err.(*exec.ExitError)
			if !ok || exited.ProcessState == nil || parent.Err() != nil {
				t.Error("actual durable Relayer did not join after its owned kernel kill")
				return
			}
			status, ok := exited.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL ||
				bytes.Contains(p.stderr.raw.Bytes(), []byte("level=ERROR")) || bytes.Contains(p.stderr.raw.Bytes(), []byte("level=WARN")) {
				t.Error("actual durable Relayer exit did not bind the expected SIGKILL")
				return
			}
			p.exit = -9
		case <-time.After(5 * time.Second):
			t.Error("actual durable Relayer SIGKILL process was not joined")
		}
	})
	if p.exit != -9 {
		t.Fatal("actual durable Relayer kernel-kill result differs")
	}
}

// Inspect the independent version-one wire layout only after joining the writer.
// No production admission-store decoder or recovery helper supplies this oracle.
func inspectQueuedWalletWire(t *testing.T, directory, config string, hash common.Hash, expected []byte) string {
	t.Helper()
	return inspectWalletAdmissionWire(t, directory, config, hash, expected, 0)
}

func inspectWalletAdmissionWire(t *testing.T, directory, config string, hash common.Hash, expected []byte, phase byte) string {
	t.Helper()
	info, err := os.Lstat(filepath.Join(directory, "owner-v1"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		t.Fatal("durable wallet owner record refused")
	}
	owner, err := os.ReadFile(filepath.Join(directory, "owner-v1"))
	if err != nil {
		t.Fatal("durable wallet owner record unreadable")
	}
	lines := bytes.Split(owner, []byte("\n"))
	if len(lines) != 3 || string(lines[0]) != "ZVM admission owner v1" || len(lines[2]) != 0 {
		t.Fatal("durable wallet owner layout refused")
	}
	domain, err := hex.DecodeString(string(lines[1]))
	if err != nil || len(domain) != sha256.Size {
		t.Fatal("durable wallet owner domain refused")
	}
	cfg, err := rollup.Load(config)
	if err != nil {
		t.Fatal("durable wallet actual rollup config refused")
	}
	encoded, err := json.Marshal(struct {
		Version  int
		Rollup   any
		Accounts []string
	}{1, cfg, []string{devnet.RelayerKey(0).Address.String()}})
	if err != nil {
		t.Fatal("durable wallet domain encoding refused")
	}
	wantDomain := sha256.Sum256(encoded)
	if !bytes.Equal(domain, wantDomain[:]) {
		t.Fatal("durable wallet store belongs to another rollup/account domain")
	}
	db, err := leveldb.OpenFile(filepath.Join(directory, "data"), &opt.Options{ReadOnly: true, Strict: opt.StrictAll, ErrorIfMissing: true})
	if err != nil {
		t.Fatal("durable wallet retained database refused")
	}
	defer db.Close()
	value, err := db.Get(append([]byte("t/"), hash[:]...), &opt.ReadOptions{Strict: opt.StrictAll})
	if err != nil || len(value) < 37+sha256.Size || len(value) > 256<<10 || string(value[:8]) != "ZVMADM01" {
		t.Fatal("durable wallet actual queued record unavailable")
	}
	count := int(binary.BigEndian.Uint32(value[8:12]))
	if count != len(expected) || len(value) != 37+count+sha256.Size || value[34] != 0 || value[35] != 0 || value[36] != phase ||
		!bytes.Equal(value[37:37+count], expected) {
		t.Fatal("durable wallet persisted wire bytes or declared phase differ")
	}
	digest := sha256.New()
	digest.Write(domain)
	digest.Write(hash[:])
	digest.Write(value[:len(value)-sha256.Size])
	if !bytes.Equal(digest.Sum(nil), value[len(value)-sha256.Size:]) {
		t.Fatal("durable wallet queued record checksum refused")
	}
	rawDigest := sha256.Sum256(expected)
	return hex.EncodeToString(rawDigest[:])
}
