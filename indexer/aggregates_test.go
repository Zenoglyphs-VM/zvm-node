package indexer

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func openBlocksAndStats(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE blocks (number INTEGER PRIMARY KEY, timestamp INTEGER, user_txs INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE chain_stats (
		id INTEGER PRIMARY KEY CHECK (id = 1), max_tps REAL NOT NULL DEFAULT 0,
		max_tps_block INTEGER NOT NULL DEFAULT 0, max_tps_txs INTEGER NOT NULL DEFAULT 0, max_tps_at INTEGER NOT NULL DEFAULT 0,
		basis TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE txs (hash TEXT PRIMARY KEY, block INTEGER, type INTEGER, from_addr TEXT, status INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// addTxs adds n successful, ordinary transactions to a block, plus its
// mandatory L1Info deposit (never a user's — excluded by type and sender,
// not by lane) and, if given, some number that reverted.
func addTxs(t *testing.T, db *sql.DB, block, n, reverted int) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO txs VALUES (?,?,?,?,?)",
		hashFor(block, -1), block, depositTxType, l1InfoDepositor, receiptStatusSuccessful); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		status := receiptStatusSuccessful
		if i < reverted {
			status = 0 // types.ReceiptStatusFailed
		}
		if _, err := db.Exec("INSERT INTO txs VALUES (?,?,?,?,?)", hashFor(block, i), block, 2, "0xUser", status); err != nil {
			t.Fatal(err)
		}
	}
}

func hashFor(block, i int) string { return fmt.Sprintf("0x%d-%d", block, i) }

func addBlock(t *testing.T, db *sql.DB, number int, ts, txs int64) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO blocks VALUES (?,?,?)", number, ts, txs); err != nil {
		t.Fatal(err)
	}
}

func readMaxTPS(t *testing.T, db *sql.DB) (tps float64, block, txs, at int64) {
	t.Helper()
	err := db.QueryRow("SELECT max_tps, max_tps_block, max_tps_txs, max_tps_at FROM chain_stats WHERE id = 1").
		Scan(&tps, &block, &txs, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return
}

func readBasis(t *testing.T, db *sql.DB) string {
	t.Helper()
	var basis string
	if err := db.QueryRow("SELECT basis FROM chain_stats WHERE id = 1").Scan(&basis); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return basis
}

func TestBumpMaxTPS(t *testing.T) {
	db := openBlocksAndStats(t)
	addBlock(t, db, 0, 1000, 5) // first block: no predecessor, must not crash or count
	run := func(number int, ts, txs int64) {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := bumpMaxTPS(tx, uint64(number), ts, int(txs)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	run(0, 1000, 5)
	if tps, _, _, _ := readMaxTPS(t, db); tps != 0 {
		t.Fatalf("the genesis block should not set a record, got %v", tps)
	}

	addBlock(t, db, 1, 1010, 20) // 10s gap, 2 tx/s
	run(1, 1010, 20)
	if tps, block, txs, _ := readMaxTPS(t, db); tps != 2 || block != 1 || txs != 20 {
		t.Fatalf("got tps=%v block=%v txs=%v, want 2/1/20", tps, block, txs)
	}

	addBlock(t, db, 2, 1011, 1) // 1s gap, 1 tx/s: below the record, must not overwrite it
	run(2, 1011, 1)
	if tps, block, _, _ := readMaxTPS(t, db); tps != 2 || block != 1 {
		t.Fatalf("a lower tps overwrote the record: tps=%v block=%v", tps, block)
	}

	addBlock(t, db, 3, 1013, 10) // 2s gap, 5 tx/s: a new record
	run(3, 1013, 10)
	if tps, block, txs, _ := readMaxTPS(t, db); tps != 5 || block != 3 || txs != 10 {
		t.Fatalf("got tps=%v block=%v txs=%v, want 5/3/10", tps, block, txs)
	}
}

func TestRebuildMaxTPS(t *testing.T) {
	db := openBlocksAndStats(t)
	addBlock(t, db, 0, 1000, 0)
	addTxs(t, db, 0, 5, 0)
	addBlock(t, db, 1, 1010, 0)
	addTxs(t, db, 1, 20, 0) // 2 tx/s, all successful
	addBlock(t, db, 2, 1013, 0)
	addTxs(t, db, 2, 30, 25) // 3s gap, 30 submitted — 10 tx/s raw — but 25 reverted: 5 landed, 1.67 tx/s
	addBlock(t, db, 3, 1015, 0)
	addTxs(t, db, 3, 2, 0)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuildMaxTPS(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Block 2 submitted at a higher rate than block 1, but most of it
	// reverted; block 1's fully-successful 20 over 10s wins once only what
	// landed is counted.
	if tps, block, txs, _ := readMaxTPS(t, db); tps != 2 || block != 1 || txs != 20 {
		t.Fatalf("got tps=%v block=%v txs=%v, want 2/1/20 — a block with more failures should not win on submitted count alone", tps, block, txs)
	}
}

// TestRebuildMaxTPSDiscountsFailuresEvenWhenTheyWouldWin makes the point on
// its own: a block that would set the record by raw count does not, once its
// reverts are taken out.
func TestRebuildMaxTPSDiscountsFailuresEvenWhenTheyWouldWin(t *testing.T) {
	db := openBlocksAndStats(t)
	addBlock(t, db, 0, 1000, 0)
	addTxs(t, db, 0, 5, 0)
	addBlock(t, db, 1, 1010, 0)
	addTxs(t, db, 1, 10, 0) // 1 tx/s, all successful
	addBlock(t, db, 2, 1011, 0)
	addTxs(t, db, 2, 1000, 999) // 1000 submitted in 1s, 999 reverted: only 1 landed

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuildMaxTPS(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if tps, block, txs, _ := readMaxTPS(t, db); tps != 1 || block != 1 || txs != 10 {
		t.Fatalf("got tps=%v block=%v txs=%v, want 1/1/10 — 999 reverts should not buy a throughput record", tps, block, txs)
	}
}

// TestRebuildMaxTPSCorrectsAStaleRow simulates upgrading a node that already
// has a chain_stats row computed the old way — every submitted transaction,
// reverts included. rebuildMaxTPS must overwrite it, value and basis both,
// not leave it alone because a row already exists.
func TestRebuildMaxTPSCorrectsAStaleRow(t *testing.T) {
	db := openBlocksAndStats(t)
	addBlock(t, db, 0, 1000, 0)
	addTxs(t, db, 0, 5, 0)
	addBlock(t, db, 1, 1001, 0)
	addTxs(t, db, 1, 100, 99) // old formula: 100 tx/s. Corrected: 1 landed, 1 tx/s.

	if _, err := db.Exec(`INSERT INTO chain_stats (id, max_tps, max_tps_block, max_tps_txs, max_tps_at, basis)
		VALUES (1, 100, 1, 100, 1001, '')`); err != nil {
		t.Fatal(err)
	}
	if basis := readBasis(t, db); basis != "" {
		t.Fatalf("fixture basis = %q, want empty (simulating a pre-upgrade row)", basis)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuildMaxTPS(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if tps, _, txs, _ := readMaxTPS(t, db); tps != 1 || txs != 1 {
		t.Fatalf("got tps=%v txs=%v, want 1/1 — the stale, revert-inflated row should have been overwritten", tps, txs)
	}
	if basis := readBasis(t, db); basis != maxTPSBasis {
		t.Fatalf("basis = %q, want %q", basis, maxTPSBasis)
	}
}
