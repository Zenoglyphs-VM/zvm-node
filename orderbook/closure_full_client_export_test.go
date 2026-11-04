//go:build fullclient_recovery

package orderbook

import (
	"database/sql"
	"net/url"
	"path/filepath"
)

// DeclareLegacyClosedCaches creates the declared test input after its first
// child has joined. It changes only derived caches, never order intent, the
// engine, the derivation store or the index. No parent repair follows restart.
func (f *FullClientRecoveryCLIFixture) DeclareLegacyClosedCaches() {
	f.r.t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.Join(f.Directory(), "node.db")}
	q := uri.Query()
	q.Set("mode", "rw")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		f.r.t.Fatal(err)
	}
	defer db.Close()
	intents := func(query interface {
		Query(string, ...any) (*sql.Rows, error)
	}) map[string]string {
		rows, err := query.Query("SELECT hash,input FROM nft_orders ORDER BY hash")
		if err != nil {
			f.r.t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var hash, input string
			if err := rows.Scan(&hash, &input); err != nil {
				f.r.t.Fatal(err)
			}
			out[hash] = input
		}
		if err := rows.Err(); err != nil {
			f.r.t.Fatal(err)
		}
		return out
	}
	before := intents(db)
	if len(before) != 2 {
		f.r.t.Fatal("legacy closure fixture must contain exactly two original orders")
	}
	tx, err := db.Begin()
	if err != nil {
		f.r.t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE nft_orders SET status=?,code=?,filled=amount,checked_epoch=-1,filled_epoch=-1,
		checked_block=0,checked_hash='',closed_block=NULL`, StatusFilled, CodeFilled)
	if err != nil {
		f.r.t.Fatal(err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 2 {
		f.r.t.Fatalf("legacy cache fixture affected %d rows: %v", count, err)
	}
	after := intents(tx)
	if len(after) != len(before) {
		f.r.t.Fatal("legacy fixture changed original order count")
	}
	for hash, input := range before {
		if after[hash] != input {
			f.r.t.Fatal("legacy fixture changed an original order input or hash")
		}
	}
	if err := tx.Commit(); err != nil {
		f.r.t.Fatal(err)
	}
	f.r.t.Log("owned_full_client_legacy_closed_input=true original_orders=2 original_input_bytes_and_hashes_preserved=true derived_cache_only=true missing_observation_provenance=true parent_repair_after_restart=false")
}
