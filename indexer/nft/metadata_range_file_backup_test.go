package nft

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// This component fixture does not back up a running full client or an operator
// database. Every file belongs to this test; no file copy or hard link is used.
func requireMetadataIndexFileBackup(t *testing.T) {
	t.Helper()
	const count, logicalBytes = 65536, 64 << 20
	exec := func(t *testing.T, db *sql.DB, statement string, args ...any) {
		t.Helper()
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	integer := func(t *testing.T, db *sql.DB, statement string) int64 {
		t.Helper()
		var value int64
		if err := db.QueryRow(statement).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	present := func(t *testing.T, db *sql.DB) bool {
		return integer(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'") == 1
	}
	open := func(t *testing.T, path string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal("could not open the owned SQLite fixture")
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		return db
	}
	bound := func(t *testing.T, db *sql.DB) (int64, int64) {
		t.Helper()
		size := integer(t, db, "PRAGMA page_size")
		if size <= 0 || logicalBytes%size != 0 {
			t.Fatal("unsupported fixture page size")
		}
		maximum := int64(logicalBytes) / size
		if integer(t, db, fmt.Sprintf("PRAGMA max_page_count=%d", maximum)) != maximum {
			t.Fatal("fixture logical page bound was not applied")
		}
		exec(t, db, "PRAGMA temp_store=MEMORY")
		if integer(t, db, "PRAGMA temp_store") != 2 || integer(t, db, "PRAGMA page_count") > maximum {
			t.Fatal("fixture exceeded its declared logical page bound")
		}
		return size, maximum
	}
	check := func(t *testing.T, db *sql.DB, before string, indexed bool) {
		t.Helper()
		var integrity string
		if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatal("fixture database failed integrity checking")
		}
		if integer(t, db, "SELECT count(*) FROM nft_tokens") != count || present(t, db) != indexed || uriHistoryDump(t, db) != before {
			t.Fatal("backup, restore or adoption changed an application table or index state")
		}
		if integer(t, db, "PRAGMA page_count")*integer(t, db, "PRAGMA page_size") > logicalBytes {
			t.Fatal("database exceeded the declared logical page bound")
		}
	}
	fileHash := func(t *testing.T, path string) [32]byte {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > logicalBytes {
			t.Fatal("backup is not a bounded regular fixture file")
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal("could not read the owned backup")
		}
		defer file.Close()
		hash := sha256.New()
		read, err := io.Copy(hash, io.LimitReader(file, logicalBytes+1))
		if err != nil || read != info.Size() {
			t.Fatal("could not hash the complete bounded backup")
		}
		var digest [32]byte
		copy(digest[:], hash.Sum(nil))
		return digest
	}
	independent := func(t *testing.T, paths ...string) {
		t.Helper()
		var infos []os.FileInfo
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatal("database path is not an owned regular fixture file")
			}
			for _, old := range infos {
				if os.SameFile(old, info) {
					t.Fatal("source, backup and restored database alias the same file")
				}
			}
			infos = append(infos, info)
		}
	}

	// Exercise the existing archive and observed-history writers before the file
	// copy. Merely copying empty metadata tables would not cover their payloads.
	x, memory, host := uriHistoryFixture(t, true)
	a, b := common.Hash{31: 1}, common.Hash{31: 2}
	uriBlock(t, memory, 1, a)
	exec(t, memory, `INSERT INTO nft_collections(address,name,symbol,contract_uri,meta,first_block) VALUES('collection','old','OLD','legacy-collection','{}',1);
	 INSERT INTO nft_tokens(collection,token_id,owner,uri,meta,name,image,minted_block,updated_block) VALUES('collection','1','owner','legacy-token','{}','old-name','old-image',1,1)`)
	tx, err := memory.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := restoreMetadata(tx, 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	host.values[a], host.values[b] = uriDocument("backup-A"), uriDocument("backup-B")
	if err := uriLog(x, memory, 1, a, true); err != nil {
		t.Fatal(err)
	}
	uriBlock(t, memory, 2, b)
	if err := uriLog(x, memory, 2, b, false); err != nil {
		t.Fatal(err)
	}
	if integer(t, memory, "SELECT count(*) FROM nft_tokens") != 2 {
		t.Fatal("history seed did not create its two distinct tokens")
	}
	for _, table := range []string{"nft_metadata_history", "nft_metadata_payloads", "nft_metadata_legacy"} {
		if integer(t, memory, "SELECT count(*) FROM "+table) == 0 {
			t.Fatal("history, payload and legacy fixtures must all be populated")
		}
	}
	if host.latest != 0 {
		t.Fatal("history fixture used an unqualified latest-state URI")
	}
	directory := t.TempDir()
	sourcePath, backupPath, restoredPath := filepath.Join(directory, "source.db"), filepath.Join(directory, "backup.db"), filepath.Join(directory, "restored.db")
	exec(t, memory, "VACUUM INTO ?", sourcePath)
	db := open(t, sourcePath)
	pageSize, maximumPages := bound(t, db)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("fixture did not enter WAL mode")
	}
	if integer(t, db, "PRAGMA wal_autocheckpoint=0") != 0 {
		t.Fatal("fixture auto-checkpoint was not disabled")
	}
	exec(t, db, "PRAGMA synchronous=FULL")
	if integer(t, db, "PRAGMA synchronous") != 2 {
		t.Fatal("fixture did not retain FULL synchronization")
	}
	exec(t, db, "DROP INDEX nft_tokens_metadata_range")
	exec(t, db, "CREATE TABLE backup_fixture_other(key TEXT PRIMARY KEY,payload BLOB); INSERT INTO backup_fixture_other VALUES('owned',X'0001FF')")
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO nft_tokens (collection, token_id, owner, uri, meta, name, image, minted_block, updated_block)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(collection, token_id) DO UPDATE SET owner = excluded.owner, approved = NULL, uri = excluded.uri,
			meta = excluded.meta, name = excluded.name, image = excluded.image, updated_block = excluded.updated_block`)
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for number := 0; number < count-2; number++ {
		if _, err := statement.Exec(fmt.Sprintf("collection-%031d", number%4), metadataIndexCostID(number),
			"fixture-owner-"+strings.Repeat("0", 28), "urn:fixture:token", `{"fixture":true}`,
			"fixture", "urn:fixture:image", number+11, number+21); err != nil {
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Lstat(sourcePath + "-wal")
	if err != nil || !wal.Mode().IsRegular() || wal.Size() <= 32 {
		t.Fatal("fixture did not retain committed data in a nonempty WAL")
	}
	before := uriHistoryDump(t, db)
	allTables := sha256.Sum256([]byte(before))
	check(t, db, before, false)
	sourcePages := integer(t, db, "PRAGMA page_count")
	var version string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}

	t.Run("roundtrip", func(t *testing.T) {
		exec(t, db, "VACUUM INTO ?", backupPath)
		backupHash := fileHash(t, backupPath)
		backup := open(t, backupPath)
		check(t, backup, before, false)
		backupPages := integer(t, backup, "PRAGMA page_count")
		exec(t, backup, "VACUUM INTO ?", restoredPath)
		independent(t, sourcePath, backupPath, restoredPath)
		restored := open(t, restoredPath)
		_, restoredMaximum := bound(t, restored)
		check(t, restored, before, false)
		exec(t, db, x.Schema())
		check(t, db, before, true)
		exec(t, restored, x.Schema())
		check(t, restored, before, true)
		indexedPages := integer(t, restored, "PRAGMA page_count")
		exec(t, restored, x.Schema())
		if integer(t, restored, "PRAGMA page_count") != indexedPages || indexedPages > restoredMaximum || sourcePages > maximumPages {
			t.Fatal("idempotent adoption changed its pages or exceeded the fixture bound")
		}
		if err := restored.Close(); err != nil {
			t.Fatal("could not close the restored fixture")
		}
		reopened := open(t, restoredPath)
		check(t, reopened, before, true)
		if err := backup.Close(); err != nil {
			t.Fatal("could not close the backup fixture")
		}
		if fileHash(t, backupPath) != backupHash {
			t.Fatal("source adoption or restore changed the retained backup bytes")
		}
		t.Logf("METADATA-INDEX-FILE-BACKUP case=roundtrip rows=%d pageSize=%d sourcePages=%d backupPages=%d restoredIndexedPages=%d logicalLimit=%d walBytes=%d sqlite=%s allTablesSHA256=%x backupSHA256=%x independent=true reopened=true idempotent=true backupUnchanged=true", count, pageSize, sourcePages, backupPages, indexedPages, logicalBytes, wal.Size(), version, allTables, backupHash)
	})
	t.Run("transaction-refusal", func(t *testing.T) {
		indexed := present(t, db)
		target := filepath.Join(directory, "transaction-refused.db")
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec("UPDATE backup_fixture_other SET payload=X'AA' WHERE key='owned'"); err != nil {
			t.Fatal(err)
		}
		_, refused := tx.Exec("VACUUM INTO ?", target)
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if refused == nil || !strings.Contains(refused.Error(), "within a transaction") {
			t.Fatal("active transaction did not refuse file backup")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatal("transaction refusal created a target file")
		}
		check(t, db, before, indexed)
		t.Logf("METADATA-INDEX-FILE-BACKUP case=transaction-refusal rows=%d sqlite=%s allTablesSHA256=%x refused=true rollback=true targetAbsent=true indexPreserved=true", count, version, allTables)
	})
	t.Run("existing-target-refusal", func(t *testing.T) {
		indexed := present(t, db)
		target := filepath.Join(directory, "existing-target.db")
		sentinel := open(t, target)
		exec(t, sentinel, "CREATE TABLE sentinel(value BLOB); INSERT INTO sentinel VALUES(X'0001FF')")
		if err := sentinel.Close(); err != nil {
			t.Fatal("could not close the owned refusal sentinel")
		}
		digest := fileHash(t, target)
		_, refused := db.Exec("VACUUM INTO ?", target)
		if refused == nil || !strings.Contains(refused.Error(), "already exists") || fileHash(t, target) != digest {
			t.Fatal("nonempty target was not refused with its exact bytes preserved")
		}
		check(t, db, before, indexed)
		t.Logf("METADATA-INDEX-FILE-BACKUP case=existing-target-refusal rows=%d sqlite=%s allTablesSHA256=%x targetSHA256=%x refused=true targetUnchanged=true indexPreserved=true", count, version, allTables, digest)
	})
}
