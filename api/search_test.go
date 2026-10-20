package api

import (
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Hashes are stored lower case, and a hash pasted from a wallet or another
// explorer may not be: searching for one used to answer "not found".
func TestSearchFindsAHashInAnyCase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, ddl := range []string{
		`CREATE TABLE blocks (number INTEGER PRIMARY KEY, hash TEXT)`,
		`CREATE TABLE momentums (height INTEGER PRIMARY KEY, hash TEXT)`,
		`CREATE TABLE txs (hash TEXT PRIMARY KEY)`,
		`CREATE TABLE messages (nom_hash TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	const tx = "0xd908b3519f4c87f8a53a4f098756ee367c580890bad587d8f96cd89d0126d38f"
	if _, err := db.Exec(`INSERT INTO txs (hash) VALUES (?)`, tx); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	for _, q := range []string{tx, strings.ToUpper(tx[2:]), "0x" + strings.ToUpper(tx[2:])} {
		got, err := s.search(t.Context(), httptest.NewRequest("GET", "/api/search?q="+q, nil))
		if err != nil {
			t.Errorf("search %s: %v", q, err)
			continue
		}
		if hit := got.(map[string]any); hit["kind"] != "tx" || hit["id"] != tx {
			t.Errorf("search %s found %v", q, hit)
		}
	}
}
