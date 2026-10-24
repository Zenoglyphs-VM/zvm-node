package nft

import (
	"database/sql"
	"fmt"
	"math/big"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite"
)

func updateHistoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE nft_tokens(collection TEXT,token_id TEXT,minted_block INTEGER,updated_block INTEGER,PRIMARY KEY(collection,token_id))`,
		`CREATE TABLE logs(address TEXT,topic0 TEXT,topic3 TEXT,data BLOB,block INTEGER,idx INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestUpdateHeightRecoveryUsesRetainedEvents(t *testing.T) {
	for _, phase := range []string{"transfer", "single-metadata", "batch-before-mint", "malformed-metadata", "full-width-id"} {
		t.Run(phase, func(t *testing.T) {
			db := updateHistoryDB(t)
			id := big.NewInt(1)
			if phase == "full-width-id" {
				id.Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
			}
			for _, values := range [][]any{{id.String(), 1, 9}, {"2", 3, 9}} {
				if _, err := db.Exec("INSERT INTO nft_tokens VALUES('collection',?,?,?)", values...); err != nil {
					t.Fatal(err)
				}
			}
			topic, data, topicID, want := topicTransfer.Hex(), []byte{}, any(common.BigToHash(id).Hex()), uint64(2)
			switch phase {
			case "single-metadata", "full-width-id":
				topic, data, topicID = topicMetadataUpdate.Hex(), common.LeftPadBytes(id.Bytes(), 32), nil
			case "batch-before-mint":
				topic, data, topicID = topicBatchMetadataUpdate.Hex(), append(common.LeftPadBytes([]byte{1}, 32), common.LeftPadBytes([]byte{2}, 32)...), nil
			case "malformed-metadata":
				topic, data, topicID, want = topicMetadataUpdate.Hex(), make([]byte, 31), nil, 1
			}
			if _, err := db.Exec("INSERT INTO logs VALUES('collection',?,?,?,2,0)", topic, topicID, data); err != nil {
				t.Fatal(err)
			}
			// A transfer-shaped log without the ERC-721 token topic is ignored.
			if _, err := db.Exec("INSERT INTO logs VALUES('collection',?,NULL,?,8,0)", topicTransfer.Hex(), make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := restoreTokenUpdateHeights(tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for key, expected := range map[string]uint64{id.String(): want, "2": 3} {
				var got uint64
				if err := db.QueryRow("SELECT updated_block FROM nft_tokens WHERE token_id=?", key).Scan(&got); err != nil || got != expected {
					t.Fatalf("retained update height for %s = %d, want %d: %v", key, got, expected, err)
				}
			}
		})
	}
}

func TestBatchUpdateRecoveryUsesTheLiveResourceBoundary(t *testing.T) {
	db := updateHistoryDB(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var ids []string
	insert, err := tx.Prepare("INSERT INTO nft_tokens VALUES('collection',?,1,9)")
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()
	for i := 1; i <= 5002; i++ {
		id := fmt.Sprint(i)
		ids = append(ids, id)
		if _, err := insert.Exec(id); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(ids)
	data := append(make([]byte, 32), common.LeftPadBytes(big.NewInt(6000).Bytes(), 32)...)
	if _, err := tx.Exec("INSERT INTO logs VALUES('collection',?,NULL,?,2,0)", topicBatchMetadataUpdate.Hex(), data); err != nil {
		t.Fatal(err)
	}
	if err := restoreTokenUpdateHeights(tx); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("SELECT token_id,updated_block FROM nft_tokens ORDER BY token_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	position := 0
	for rows.Next() {
		var id string
		var got int
		if err := rows.Scan(&id, &got); err != nil {
			t.Fatal(err)
		}
		want := 1
		if position < 5000 {
			want = 2
		}
		if position >= len(ids) || id != ids[position] || got != want {
			t.Fatalf("bounded batch update for %s = %d, want %d", id, got, want)
		}
		position++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if position != len(ids) {
		t.Fatal("bounded batch recovery lost token rows")
	}
}

func TestUpdateHeightRefusalPreservesThePreviousTransaction(t *testing.T) {
	db := updateHistoryDB(t)
	for _, statement := range []string{
		`INSERT INTO nft_tokens VALUES('collection','1',1,9)`,
		`CREATE TRIGGER reject_update BEFORE UPDATE ON nft_tokens BEGIN SELECT RAISE(ABORT,'declared update-height refusal'); END`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreTokenUpdateHeights(tx); err == nil {
		t.Fatal("declared update-height refusal was not reached")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := db.QueryRow("SELECT updated_block FROM nft_tokens").Scan(&got); err != nil || got != 9 {
		t.Fatal("failed history recovery changed the previous value")
	}
}
