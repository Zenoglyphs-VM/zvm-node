package nft

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const metadataProcessRows, metadataProcessLogicalBytes = 65536, 64 << 20

// A child only enters this branch when the parent supplies the complete owned
// fixture protocol. It never completes its test: the parent must observe the
// declared boundary, send SIGKILL and reap that exact process.
func metadataIndexProcessChild(t *testing.T) bool {
	phase := os.Getenv("ZVM_NFT_INDEX_PROCESS_PHASE")
	if phase == "" {
		return false
	}
	path, token := os.Getenv("ZVM_NFT_INDEX_PROCESS_FILE"), os.Getenv("ZVM_NFT_INDEX_PROCESS_TOKEN")
	if (phase != "before-commit" && phase != "after-commit") || len(token) != 32 || strings.Trim(token, "0123456789abcdef") != "" || !filepath.IsAbs(path) || filepath.Base(path) != "index-kill.db" {
		t.Fatal("invalid isolated index-process protocol")
	}
	canonical, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	parent, parentErr := os.Lstat(filepath.Dir(path))
	if err != nil || canonical != path || statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > metadataProcessLogicalBytes || parentErr != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		t.Fatal("index-process path is not the declared private regular fixture")
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != uint32(os.Geteuid()) || identity.Nlink != 1 {
		t.Fatal("index-process fixture ownership or link count changed")
	}
	db := metadataProcessOpen(t, path)
	metadataProcessConfigure(t, db)
	if metadataProcessInteger(t, db, "SELECT count(*) FROM nft_tokens") != metadataProcessRows || metadataProcessIndexPresent(t, db) {
		t.Fatal("index-process fixture is not a legacy unindexed token table")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec((&Index{}).Schema()); err != nil {
		t.Fatal(err)
	}
	var created int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'").Scan(&created); err != nil || created != 1 {
		t.Fatal("production schema did not create the index inside its transaction")
	}
	if phase == "after-commit" {
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if !metadataProcessIndexPresent(t, db) {
			t.Fatal("completed schema transaction did not retain its index")
		}
	}
	fmt.Printf("METADATA-INDEX-PROCESS-READY phase=%s token=%s\n", phase, token)
	for {
		time.Sleep(time.Hour)
	}
}

func metadataProcessOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal("could not open the owned index-process fixture")
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func metadataProcessInteger(t *testing.T, db *sql.DB, statement string) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(statement).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func metadataProcessIndexPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	return metadataProcessInteger(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'") == 1
}

func metadataProcessConfigure(t *testing.T, db *sql.DB) {
	t.Helper()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("index-process fixture requires WAL mode")
	}
	for _, statement := range []string{"PRAGMA synchronous=FULL", "PRAGMA temp_store=MEMORY", "PRAGMA cache_size=64", "PRAGMA cache_spill=ON"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	page := metadataProcessInteger(t, db, "PRAGMA page_size")
	if page <= 0 || metadataProcessLogicalBytes%page != 0 || metadataProcessInteger(t, db, fmt.Sprintf("PRAGMA max_page_count=%d", metadataProcessLogicalBytes/page)) != metadataProcessLogicalBytes/page || metadataProcessInteger(t, db, "PRAGMA synchronous") != 2 || metadataProcessInteger(t, db, "PRAGMA temp_store") != 2 || metadataProcessInteger(t, db, "PRAGMA cache_size") != 64 || metadataProcessInteger(t, db, "PRAGMA cache_spill") == 0 || metadataProcessInteger(t, db, "PRAGMA wal_autocheckpoint=0") != 0 {
		t.Fatal("index-process fixture did not retain its declared SQLite settings")
	}
}

// Output is bounded independently of the subprocess lifetime. Only the stdout
// writer owns ready; the parent reads the stored bytes after Wait has joined.
type metadataProcessOutput struct {
	data     []byte
	total    int
	overflow bool
	ready    chan string
	notified bool
}

func (w *metadataProcessOutput) Write(p []byte) (int, error) {
	w.total += len(p)
	keep := min(len(p), 8192-len(w.data))
	w.data = append(w.data, p[:keep]...)
	w.overflow = w.overflow || keep != len(p)
	if w.ready != nil && !w.notified {
		if newline := bytes.IndexByte(w.data, '\n'); newline >= 0 {
			w.notified = true
			w.ready <- string(w.data[:newline])
		} else if w.overflow {
			w.notified = true
			w.ready <- "OUTPUT_OVERFLOW"
		}
	}
	return len(p), nil
}

func metadataProcessFailureClass(output []byte) string {
	for _, entry := range []struct{ text, code string }{
		{"invalid isolated index-process protocol", "INVALID_PROTOCOL"},
		{"index-process path is not the declared private regular fixture", "PRIVATE_FIXTURE_REFUSED"},
		{"index-process fixture ownership or link count changed", "OWNERSHIP_REFUSED"},
		{"index-process fixture is not a legacy unindexed token table", "LEGACY_FIXTURE_REFUSED"},
		{"production schema did not create the index inside its transaction", "INDEX_CREATION_REFUSED"},
		{"completed schema transaction did not retain its index", "COMMIT_REFUSED"},
		{"DATA RACE", "RACE_DIAGNOSTIC"},
	} {
		if bytes.Contains(output, []byte(entry.text)) {
			return entry.code
		}
	}
	return "UNCLASSIFIED"
}

func metadataProcessKillAtBoundary(t *testing.T, path, phase string) {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	token := fmt.Sprintf("%x", nonce)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPreparedURIRangeIndex$", "-test.timeout=30s")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "ZVM_NFT_INDEX_PROCESS_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "ZVM_NFT_INDEX_PROCESS_PHASE="+phase, "ZVM_NFT_INDEX_PROCESS_FILE="+path, "ZVM_NFT_INDEX_PROCESS_TOKEN="+token)
	stdout := &metadataProcessOutput{ready: make(chan string, 1)}
	stderr := &metadataProcessOutput{}
	command.Stdout, command.Stderr, command.WaitDelay = stdout, stderr, time.Second
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	joined := false
	defer func() {
		if !joined {
			command.Process.Kill()
			<-done
		}
	}()
	expected := "METADATA-INDEX-PROCESS-READY phase=" + phase + " token=" + token
	select {
	case line := <-stdout.ready:
		if line != expected || ctx.Err() != nil {
			command.Process.Kill()
			<-done
			joined = true
			class := metadataProcessFailureClass(append(append([]byte{}, stdout.data...), stderr.data...))
			t.Fatalf("owned index process refused its boundary: class=%s stdoutBytes=%d stderrBytes=%d stdoutSHA256=%x stderrSHA256=%x", class, stdout.total, stderr.total, sha256.Sum256(stdout.data), sha256.Sum256(stderr.data))
		}
	case err := <-done:
		joined = true
		t.Fatalf("index process ended before its declared boundary: %v", err)
	case <-ctx.Done():
		t.Fatal("index process did not reach its boundary within the fixture deadline")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal("could not send SIGKILL to the exact owned process")
	}
	err := <-done
	joined = true
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ProcessState == nil {
		t.Fatalf("index-process interruption was not an observed process exit: %v", err)
	}
	status, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || ctx.Err() != nil {
		t.Fatal("index process did not end from the requested SIGKILL before its deadline")
	}
	if stdout.overflow || stderr.overflow || stderr.total != 0 || stdout.total != len(expected)+1 || string(stdout.data) != expected+"\n" {
		t.Fatal("owned process emitted unexpected output, a race report or incomplete boundary evidence")
	}
}

func requireMetadataIndexProcessInterruption(t *testing.T) {
	t.Helper()
	// Seed one immutable logical source, then make independent SQLite copies.
	// This includes the real observed-history, payload and legacy table writers.
	x, memory, host := uriHistoryFixture(t, true)
	a, b := common.Hash{31: 1}, common.Hash{31: 2}
	uriBlock(t, memory, 1, a)
	if _, err := memory.Exec(`INSERT INTO nft_collections(address,name,symbol,contract_uri,meta,first_block) VALUES('collection','old','OLD','legacy-collection','{}',1);
	 INSERT INTO nft_tokens(collection,token_id,owner,uri,meta,name,image,minted_block,updated_block) VALUES('collection','1','owner','legacy-token','{}','old-name','old-image',1,1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := memory.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreMetadata(tx, 1); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	host.values[a], host.values[b] = uriDocument("process-A"), uriDocument("process-B")
	if err := uriLog(x, memory, 1, a, true); err != nil {
		t.Fatal(err)
	}
	uriBlock(t, memory, 2, b)
	if err := uriLog(x, memory, 2, b, false); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"nft_metadata_history", "nft_metadata_payloads", "nft_metadata_legacy"} {
		if metadataProcessInteger(t, memory, "SELECT count(*) FROM "+table) == 0 {
			t.Fatal("process fixture requires populated history, payload and legacy tables")
		}
	}
	if host.latest != 0 {
		t.Fatal("process fixture used an unqualified latest-state URI")
	}
	basePath := filepath.Join(t.TempDir(), "source.db")
	if _, err := memory.Exec("VACUUM INTO ?", basePath); err != nil {
		t.Fatal(err)
	}
	db := metadataProcessOpen(t, basePath)
	metadataProcessConfigure(t, db)
	if _, err := db.Exec("DROP INDEX nft_tokens_metadata_range; CREATE TABLE process_fixture_other(key TEXT PRIMARY KEY,payload BLOB); INSERT INTO process_fixture_other VALUES('owned',X'0001FF')"); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO nft_tokens(collection,token_id,owner,uri,meta,name,image,minted_block,updated_block) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for number := 0; number < metadataProcessRows-2; number++ {
		if _, err := statement.Exec(fmt.Sprintf("collection-%031d", number%4), metadataIndexCostID(number), "fixture-owner", "urn:fixture:token", `{"fixture":true}`, "fixture", "urn:fixture:image", number+11, number+21); err != nil {
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before := uriHistoryDump(t, db)
	digest := sha256.Sum256([]byte(before))
	if metadataProcessInteger(t, db, "SELECT count(*) FROM nft_tokens") != metadataProcessRows || metadataProcessIndexPresent(t, db) {
		t.Fatal("process seed changed its declared row count or index state")
	}
	var version string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"before-commit", "after-commit"} {
		t.Run(phase, func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal("could not resolve the newly owned process fixture")
			}
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal("could not make the newly owned process fixture private")
			}
			if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
				t.Fatal("newly owned process fixture did not retain its private mode")
			}
			path := filepath.Join(directory, "index-kill.db")
			if _, err := db.Exec("VACUUM INTO ?", path); err != nil {
				t.Fatal(err)
			}
			if source, err := os.Stat(basePath); err != nil {
				t.Fatal(err)
			} else if copied, err := os.Stat(path); err != nil || os.SameFile(source, copied) {
				t.Fatal("process fixture aliases its source")
			}
			metadataProcessKillAtBoundary(t, path, phase)
			// Inspect the WAL before reopening can checkpoint or remove it.
			wal, err := os.Lstat(path + "-wal")
			if err != nil || !wal.Mode().IsRegular() || wal.Size() < 32 || wal.Size() > metadataProcessLogicalBytes {
				t.Fatal("killed process did not leave the bounded observed WAL")
			}
			walBytes := wal.Size()
			recovered := metadataProcessOpen(t, path)
			metadataProcessConfigure(t, recovered)
			var integrity string
			if err := recovered.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatal("killed index-process database did not pass integrity checking")
			}
			if uriHistoryDump(t, recovered) != before || metadataProcessInteger(t, recovered, "SELECT count(*) FROM nft_tokens") != metadataProcessRows || metadataProcessIndexPresent(t, recovered) != (phase == "after-commit") {
				t.Fatal("process death changed canonical rows or the expected commit boundary")
			}
			pages := metadataProcessInteger(t, recovered, "PRAGMA page_count")
			page := metadataProcessInteger(t, recovered, "PRAGMA page_size")
			if pages*page > metadataProcessLogicalBytes {
				t.Fatal("recovered database exceeded its logical page bound")
			}
			if _, err := recovered.Exec(x.Schema()); err != nil {
				t.Fatal(err)
			}
			indexedPages := metadataProcessInteger(t, recovered, "PRAGMA page_count")
			if _, err := recovered.Exec(x.Schema()); err != nil {
				t.Fatal(err)
			}
			if !metadataProcessIndexPresent(t, recovered) || uriHistoryDump(t, recovered) != before || metadataProcessInteger(t, recovered, "PRAGMA page_count") != indexedPages || indexedPages*page > metadataProcessLogicalBytes {
				t.Fatal("post-kill adoption or idempotence changed canonical rows or exceeded its bound")
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := metadataProcessOpen(t, path)
			if !metadataProcessIndexPresent(t, reopened) || uriHistoryDump(t, reopened) != before {
				t.Fatal("post-adoption reopen changed canonical rows or index state")
			}
			if uriHistoryDump(t, db) != before || metadataProcessIndexPresent(t, db) {
				t.Fatal("owned subprocess changed the independent source database")
			}
			t.Logf("METADATA-INDEX-PROCESS-RECOVERY phase=%s rows=%d signal=SIGKILL joined=true transactionCommitted=%t indexPresentAfterRecovery=%t walBytes=%d pageSize=%d recoveredPages=%d indexedPages=%d logicalLimit=%d sqlite=%s allTablesSHA256=%x sourceUnchanged=true idempotent=true reopened=true", phase, metadataProcessRows, phase == "after-commit", phase == "after-commit", walBytes, page, pages, indexedPages, metadataProcessLogicalBytes, version, digest)
		})
	}
}
