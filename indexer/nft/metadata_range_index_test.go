package nft

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func rangeIndexFixture(t *testing.T, ids []any) (*Index, *sql.DB, common.Address) {
	t.Helper()
	x, db, _ := uriHistoryFixture(t, false)
	address := common.Address{19: 21}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare("INSERT INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'existing',1,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	for i, id := range ids {
		if _, err := statement.Exec(address.Hex(), id, i+11); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := statement.Exec(common.Address{19: 99}.Hex(), "1", 99); err != nil {
		t.Fatal(err)
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return x, db, address
}

func rangeIndexMatches(t *testing.T, db *sql.DB, address common.Address, from, to *big.Int, limit int) ([]string, int) {
	t.Helper()
	statement, args := metadataRangeQuery(address, from, to, false)
	rows, err := db.QueryContext(context.Background(), statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var selected []string
	candidates := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		candidates++
		if id, ok := new(big.Int).SetString(raw, 10); ok && id.Cmp(from) >= 0 && id.Cmp(to) <= 0 {
			selected = append(selected, id.String())
			if len(selected) == limit {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return selected, candidates
}

func requireRangeIndexPlan(t *testing.T, db *sql.DB, address common.Address, from, to *big.Int, widths int) {
	t.Helper()
	statement, args := metadataRangeQuery(address, from, to, false)
	if len(statement) > 64<<10 || len(args) > 313 {
		t.Fatal("unsigned range statement exceeds its declared bound")
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	searches, ranges := 0, 0
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "TEMP B-TREE") || strings.Contains(detail, "SCAN nft_tokens") {
			t.Fatalf("range query sorts or scans a token table: %s", detail)
		}
		if strings.HasPrefix(detail, "SEARCH ") && strings.Contains(detail, "nft_tokens_metadata_range") {
			searches++
			if strings.Contains(detail, "token_id>?") && strings.Contains(detail, "token_id<?") {
				ranges++
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if searches != widths+1 || ranges != widths {
		t.Fatalf("indexed branches: searches=%d ranges=%d widths=%d", searches, ranges, widths)
	}
	t.Logf("METADATA-RANGE-INDEX-PLAN widths=%d searches=%d ranges=%d sqlBytes=%d parameters=%d", widths, searches, ranges, len(statement), len(args))
}

func requireFullDomainRangePlan(t *testing.T, db *sql.DB, address common.Address, maximum *big.Int) {
	t.Helper()
	statement, args := metadataRangeQuery(address, big.NewInt(0), maximum, false)
	if len(statement) != 200 || len(args) != 2 || strings.Contains(statement, " LIMIT ") {
		t.Fatal("whole-domain query changed its statement, arguments or parser boundary")
	}
	rows, err := db.Query("EXPLAIN QUERY PLAN "+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	searches, collection, required := 0, 0, 0
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "TEMP B-TREE") || strings.Contains(detail, "SCAN nft_tokens") {
			t.Fatalf("whole-domain query sorts or scans a token table: %s", detail)
		}
		if strings.HasPrefix(detail, "SEARCH ") {
			searches++
			if strings.Contains(detail, "nft_tokens_collection") {
				collection++
			}
			if strings.Contains(detail, "nft_tokens_metadata_range") {
				required++
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if searches != 2 || collection != 1 || required != 1 {
		t.Fatalf("whole-domain indexes: searches=%d collection=%d required=%d", searches, collection, required)
	}
	t.Logf("METADATA-RANGE-FULL-DOMAIN-PLAN searches=%d collection=%d required=%d sqlBytes=%d parameters=%d", searches, collection, required, len(statement), len(args))
}

func TestPreparedURIRangeIndex(t *testing.T) {
	if metadataIndexProcessChild(t) {
		return
	}
	maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	oversized := new(big.Int).Lsh(big.NewInt(1), 256)
	sparse := make([]any, 0, 16384)
	for i := 1; i <= 8192; i++ {
		sparse = append(sparse, fmt.Sprint(i))
	}
	for i := 0; i < 4096; i++ {
		sparse = append(sparse, fmt.Sprintf("7000%06d", i), fmt.Sprintf("7001%06d", i))
	}
	for _, test := range []struct {
		name     string
		from, to *big.Int
		ids      []any
		widths   int
	}{
		{"sparse-seek", big.NewInt(7000), big.NewInt(7002), sparse, 1},
		{"cross-width-seek", big.NewInt(9), big.NewInt(100), []any{"0", "8", "9", "10", "11", "99", "100", "101", "1000", "999999999999999"}, 3},
		{"all-widths", big.NewInt(1), maximum, []any{"0", "1", "9", "10", "99", "100", maximum.String(), oversized.String(), strings.Repeat("9", 78), "-0", []byte("7")}, 78},
		{"full-domain", big.NewInt(0), maximum, []any{"0", "+0", "-0", "00", "1", "+1", maximum.String(), "+" + maximum.String(), oversized.String(), strings.Repeat("9", 79), "1\x009", []byte("7"), []byte{0xff}}, 0},
		{"full-domain-empty", big.NewInt(0), maximum, nil, 0},
		{"legacy-merge", big.NewInt(0), big.NewInt(11), []any{"0", "+0", "-0", "00", "1", "+1", "01", "10", "0010", "+10", "1_0", "x", "\uff11\uff12", "1\x009", []byte("7"), []byte("+2"), []byte{0xff}}, 2},
		{"signed-fallback", big.NewInt(-2), big.NewInt(2), []any{"-2", "-1", "0", "1", "2", "3", "+1"}, 0},
		{"oversized-bound-fallback", oversized, new(big.Int).Add(new(big.Int).Set(oversized), big.NewInt(2)), []any{oversized.String(), new(big.Int).Add(new(big.Int).Set(oversized), big.NewInt(1)).String(), new(big.Int).Add(new(big.Int).Set(oversized), big.NewInt(2)).String(), "0", "-1"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, db, address := rangeIndexFixture(t, test.ids)
			// The retained oracle scans the original collection ordering and
			// parses in Go. It does not use the expression or query builder.
			want := oldMetadataRangeSelection(t, db, address, test.from, test.to)
			got, candidates := rangeIndexMatches(t, db, address, test.from, test.to, 5000)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("indexed matches %v differ from original %v", got, want)
			}
			if test.name == "full-domain" || test.name == "full-domain-empty" {
				requireFullDomainRangePlan(t, db, address, maximum)
			}
			if test.widths > 0 {
				requireRangeIndexPlan(t, db, address, test.from, test.to, test.widths)
			}
			if test.name == "sparse-seek" && candidates != 3 {
				t.Fatalf("dense-prefix sparse query returned %d candidates, want 3", candidates)
			}
			if test.name == "sparse-seek" {
				var version string
				if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
					t.Fatal(err)
				}
				t.Logf("METADATA-RANGE-INDEX-RUNTIME %s", version)
			}
		})
	}
	t.Run("lexicographic-lookahead", func(t *testing.T) {
		ids := make([]any, 0, 6200)
		for i := 1; i <= 6200; i++ {
			ids = append(ids, fmt.Sprint(i))
		}
		_, db, address := rangeIndexFixture(t, ids)
		from, to := big.NewInt(1), big.NewInt(6200)
		want := oldMetadataRangeSelection(t, db, address, from, to)
		got, _ := rangeIndexMatches(t, db, address, from, to, 5001)
		if len(got) != 5001 || len(want) != 5000 || fmt.Sprint(got[:5000]) != fmt.Sprint(want) {
			t.Fatal("original first 5000 and one lookahead changed")
		}
		requireRangeIndexPlan(t, db, address, from, to, 4)
	})
	t.Run("full-domain-lookahead", func(t *testing.T) {
		ids := make([]any, 0, 11230)
		for i := 0; i < 5012; i++ {
			ids = append(ids, fmt.Sprintf("!%06d", i))
		}
		for i := 1; i <= 6200; i++ {
			ids = append(ids, fmt.Sprint(i))
		}
		ids = append(ids, "0", "+0", "-0", "00", maximum.String(), oversized.String(), strings.Repeat("9", 79), []byte("7"))
		_, db, address := rangeIndexFixture(t, ids)
		want := oldMetadataRangeSelection(t, db, address, big.NewInt(0), maximum)
		got, candidates := rangeIndexMatches(t, db, address, big.NewInt(0), maximum, 5001)
		if len(got) != 5001 || len(want) != 5000 || candidates <= 10012 || fmt.Sprint(got[:5000]) != fmt.Sprint(want) {
			t.Fatal("whole-domain legacy or oversized candidates changed the original matching-ID boundary")
		}
		requireFullDomainRangePlan(t, db, address, maximum)
	})
	t.Run("legacy-does-not-consume-limit", func(t *testing.T) {
		ids := make([]any, 0, 5020)
		for i := 0; i < 5012; i++ {
			ids = append(ids, fmt.Sprintf("!%06d", i))
		}
		for i := 0; i < 8; i++ {
			ids = append(ids, fmt.Sprint(i))
		}
		_, db, address := rangeIndexFixture(t, ids)
		from, to := big.NewInt(0), big.NewInt(7)
		want := oldMetadataRangeSelection(t, db, address, from, to)
		got, candidates := rangeIndexMatches(t, db, address, from, to, 5001)
		if len(got) != 8 || candidates <= 5001 || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatal("legacy candidates consumed the matching-ID limit")
		}
	})
	t.Run("block-columns", func(t *testing.T) {
		_, db, address := rangeIndexFixture(t, []any{"1", "2", "+3", []byte("4")})
		type key struct {
			id              string
			minted, updated uint64
		}
		read := func(statement string, args []any) []key {
			rows, err := db.Query(statement, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []key
			for rows.Next() {
				var item key
				if err := rows.Scan(&item.id, &item.minted, &item.updated); err != nil {
					t.Fatal(err)
				}
				out = append(out, item)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			return out
		}
		want := read("SELECT token_id,minted_block,updated_block FROM nft_tokens WHERE collection=? ORDER BY token_id", []any{address.Hex()})
		for _, bounds := range [][2]*big.Int{{big.NewInt(1), big.NewInt(4)}, {big.NewInt(0), maximum}} {
			statement, args := metadataRangeQuery(address, bounds[0], bounds[1], true)
			got := read(statement, args)
			if len(got) != 4 || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatal("discovery keys or source block columns changed")
			}
		}
	})
	t.Run("cancelled-query", func(t *testing.T) {
		_, db, address := rangeIndexFixture(t, []any{"1"})
		before := uriHistoryDump(t, db)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		statement, args := metadataRangeQuery(address, big.NewInt(0), big.NewInt(1), false)
		rows, err := db.QueryContext(ctx, statement, args...)
		if rows != nil {
			for rows.Next() {
			}
			if err == nil {
				err = rows.Err()
			}
			rows.Close()
		}
		if !errors.Is(err, context.Canceled) || uriHistoryDump(t, db) != before {
			t.Fatalf("cancelled range changed tables or error: %v", err)
		}
	})
	indexPresent := func(t *testing.T, db *sql.DB) bool {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 1
	}
	t.Run("schema-upgrade", func(t *testing.T) {
		x, db, address := rangeIndexFixture(t, []any{"0", "1", "+2", []byte("3")})
		if _, err := db.Exec("DROP INDEX nft_tokens_metadata_range"); err != nil {
			t.Fatal(err)
		}
		before := uriHistoryDump(t, db)
		for i := 0; i < 2; i++ {
			if _, err := db.Exec(x.Schema()); err != nil {
				t.Fatal(err)
			}
		}
		if !indexPresent(t, db) || uriHistoryDump(t, db) != before {
			t.Fatal("idempotent index adoption changed canonical rows")
		}
		requireRangeIndexPlan(t, db, address, big.NewInt(0), big.NewInt(4), 1)
	})
	t.Run("schema-upgrade-refusal", func(t *testing.T) {
		x, db, _ := rangeIndexFixture(t, []any{"1"})
		if _, err := db.Exec("DROP INDEX nft_tokens_metadata_range"); err != nil {
			t.Fatal(err)
		}
		before := uriHistoryDump(t, db)
		if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(x.Schema()); err == nil {
			t.Fatal("read-only schema update unexpectedly succeeded")
		}
		if indexPresent(t, db) || uriHistoryDump(t, db) != before {
			t.Fatal("refused index adoption changed canonical state")
		}
	})
	t.Run("missing-index-refusal", func(t *testing.T) {
		x, db, address := rangeIndexFixture(t, []any{"1"})
		if _, err := db.Exec("DROP INDEX nft_tokens_metadata_range"); err != nil {
			t.Fatal(err)
		}
		before := uriHistoryDump(t, db)
		for _, discovery := range []bool{false, true} {
			for _, upper := range []*big.Int{big.NewInt(1), maximum} {
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if discovery {
					p, openErr := newPreparedURI(context.Background(), x, preparedURIBytes)
					if openErr != nil {
						t.Fatal(openErr)
					}
					p.collect, openErr = p.db.Begin()
					if openErr != nil {
						p.Close()
						t.Fatal(openErr)
					}
					err = p.loadRange(context.Background(), tx, address, big.NewInt(0), upper)
					p.Close()
				} else {
					err = x.refreshRange(context.Background(), tx, address, big.NewInt(0), upper, 1)
				}
				tx.Rollback()
				if err == nil || !strings.Contains(err.Error(), "nft_tokens_metadata_range") {
					t.Fatalf("missing-index path did not refuse: %v", err)
				}
				if uriHistoryDump(t, db) != before {
					t.Fatal("missing-index refusal changed a canonical table")
				}
			}
		}
	})
	t.Run("unusable-full-domain-index-refusal", func(t *testing.T) {
		for _, name := range []string{"nft_tokens_collection", "nft_tokens_metadata_range"} {
			x, db, address := rangeIndexFixture(t, []any{"1"})
			if _, err := db.Exec("DROP INDEX " + name); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("CREATE INDEX " + name + " ON nft_tokens(collection,token_id) WHERE collection='unselected'"); err != nil {
				t.Fatal(err)
			}
			before := uriHistoryDump(t, db)
			for _, discovery := range []bool{false, true} {
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if discovery {
					p, openErr := newPreparedURI(context.Background(), x, preparedURIBytes)
					if openErr != nil {
						t.Fatal(openErr)
					}
					p.collect, openErr = p.db.Begin()
					if openErr != nil {
						p.Close()
						t.Fatal(openErr)
					}
					err = p.loadRange(context.Background(), tx, address, big.NewInt(0), maximum)
					p.Close()
				} else {
					err = x.refreshRange(context.Background(), tx, address, big.NewInt(0), maximum, 1)
				}
				tx.Rollback()
				if err == nil || !strings.Contains(err.Error(), "no query solution") {
					t.Fatalf("unusable whole-domain index did not refuse: %v", err)
				}
				if uriHistoryDump(t, db) != before {
					t.Fatal("unusable whole-domain index changed a canonical table")
				}
			}
		}
	})
	t.Run("adoption-cost", func(t *testing.T) {
		for _, count := range []int{4096, 16384, 65536} {
			t.Run(fmt.Sprint(count), func(t *testing.T) {
				requireMetadataIndexAdoptionCost(t, count)
			})
		}
	})
	t.Run("file-backup", func(t *testing.T) {
		requireMetadataIndexFileBackup(t)
	})
	t.Run("process-interruption", func(t *testing.T) {
		requireMetadataIndexProcessInterruption(t)
	})
	t.Run("transactional-upgrade-rollback", func(t *testing.T) {
		x, db, _ := rangeIndexFixture(t, []any{"1", "+2"})
		if _, err := db.Exec("DROP INDEX nft_tokens_metadata_range"); err != nil {
			t.Fatal(err)
		}
		before := uriHistoryDump(t, db)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(x.Schema()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if indexPresent(t, db) || uriHistoryDump(t, db) != before {
			t.Fatal("rolled-back index adoption changed canonical state")
		}
	})
}
