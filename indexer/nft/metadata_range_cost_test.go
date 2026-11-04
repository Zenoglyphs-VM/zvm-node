package nft

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"testing"
)

func metadataIndexCostID(number int) any {
	if number < 8 {
		special := []any{"+0", "-0", "00", "0001", "+1", "1\x009", []byte("2"), []byte{0xff}}
		return special[number]
	}
	if number%257 == 0 {
		return new(big.Int).Lsh(big.NewInt(1), 255).String() + fmt.Sprintf("%06d", number)
	}
	return fmt.Sprint(number)
}

// This is an isolated token-table adoption fixture, not a full-client history,
// a disk/WAL backup, a physical allocation bound or an operator migration.
func requireMetadataIndexAdoptionCost(t *testing.T, count int) {
	t.Helper()
	if count != 4096 && count != 16384 && count != 65536 {
		t.Fatal("undeclared index adoption fixture size")
	}
	x, db, _ := uriHistoryFixture(t, false)
	integer := func(statement string) int64 {
		t.Helper()
		var value int64
		if err := db.QueryRow(statement).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	exec := func(statement string) {
		t.Helper()
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	present := func() bool {
		return integer("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'") == 1
	}
	const logicalBytes = 64 << 20
	pageSize := integer("PRAGMA page_size")
	if pageSize <= 0 || logicalBytes%pageSize != 0 {
		t.Fatal("unsupported page size for the declared fixture bound")
	}
	maximumPages := int64(logicalBytes) / pageSize
	if integer(fmt.Sprintf("PRAGMA max_page_count=%d", maximumPages)) != maximumPages {
		t.Fatal("fixture page bound was not applied")
	}
	exec("PRAGMA temp_store=MEMORY")
	if integer("PRAGMA temp_store") != 2 {
		t.Fatal("fixture did not retain temporary work in memory")
	}
	exec("DROP INDEX nft_tokens_metadata_range")
	// Keep all declared rows and typed legacy IDs; bound each fixture call to
	// sixteen rows and 144 bind arguments without changing transaction scope.
	const batchRows = 16
	const bindArguments = batchRows * 9
	seedExecs := 0
	seed := func(tx *sql.Tx) {
		t.Helper()
		statement, err := tx.Prepare(`INSERT INTO nft_tokens (collection, token_id, owner, uri, meta, name, image, minted_block, updated_block)
		VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?,?,?,?,?),", batchRows), ",") + `
		ON CONFLICT(collection, token_id) DO UPDATE SET owner = excluded.owner, approved = NULL, uri = excluded.uri,
			meta = excluded.meta, name = excluded.name, image = excluded.image, updated_block = excluded.updated_block`)
		if err != nil {
			t.Fatal(err)
		}
		defer statement.Close()
		owner := "fixture-owner-" + strings.Repeat("0", 28)
		for first := 0; first < count; first += batchRows {
			arguments := make([]any, 0, bindArguments)
			for number := first; number < first+batchRows; number++ {
				arguments = append(arguments, fmt.Sprintf("collection-%031d", number%4), metadataIndexCostID(number),
					owner, "urn:fixture:token", `{"fixture":true}`, "fixture", "urn:fixture:image", number+11, number+21)
			}
			if len(arguments) != bindArguments {
				t.Fatal("fixture batch changed its declared argument bound")
			}
			seedExecs++
			result, err := statement.Exec(arguments...)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != batchRows {
				t.Fatalf("fixture batch affected %d rows: %v", changed, err)
			}
		}
		if err := statement.Close(); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seed(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if integer("SELECT count(*) FROM nft_tokens") != int64(count) || present() {
		t.Fatal("legacy fixture changed its rows or already has the new index")
	}
	before := uriHistoryDump(t, db)
	// These complete-table images agree in both normal/race modes at the
	// separately qualified CFF and 7eb3aeb source revisions. Keep the original
	// byte comparisons after every refusal, rollback and adoption below.
	expectedTables := map[int]string{
		4096:  "7390e0488f0cf609c03239b446ba8f29f0f416b640c5a48b35ecd490002b1e63",
		16384: "0258337ff291525c6dc0b0336a12902f99f93c2212f736e9ce6b0da223f75be1",
		65536: "c651e024c58d61bc282745a823216b4efbd23eaf89137acde4222529a82b94e9",
	}
	actualTables := fmt.Sprintf("%x", sha256.Sum256([]byte(before)))
	if actualTables != expectedTables[count] || seedExecs != count/batchRows {
		t.Fatalf("batched fixture changed its parent row image or call bound: rows=%d calls=%d tables=%s", count, seedExecs, actualTables)
	}
	t.Logf("METADATA-INDEX-FIXTURE-BATCH rows=%d batchRows=%d insertExecs=%d bindArguments=%d allTablesSHA256=%s",
		count, batchRows, seedExecs, bindArguments, actualTables)
	unchanged := func() {
		t.Helper()
		if uriHistoryDump(t, db) != before || present() {
			t.Fatal("refused or rolled-back adoption changed a fixture table or retained an index")
		}
	}
	exec("PRAGMA query_only=ON")
	_, refused := db.Exec(x.Schema())
	exec("PRAGMA query_only=OFF")
	if refused == nil || !strings.Contains(refused.Error(), "readonly") {
		t.Fatalf("read-only adoption did not refuse: %v", refused)
	}
	unchanged()
	baselinePages := integer("PRAGMA page_count")
	if integer(fmt.Sprintf("PRAGMA max_page_count=%d", baselinePages)) != baselinePages {
		t.Fatal("isolated allocation-refusal bound was not applied")
	}
	_, refused = db.Exec(x.Schema())
	if integer(fmt.Sprintf("PRAGMA max_page_count=%d", maximumPages)) != maximumPages {
		t.Fatal("fixture page bound was not restored")
	}
	if refused == nil || !strings.Contains(refused.Error(), "database or disk is full") {
		t.Fatalf("bounded allocation did not refuse index adoption: %v", refused)
	}
	unchanged()
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(x.Schema()); err != nil {
		t.Fatal(err)
	}
	var inside int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='nft_tokens_metadata_range'").Scan(&inside); err != nil || inside != 1 {
		t.Fatalf("transaction did not create its index: %d %v", inside, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	unchanged()
	baselinePages = integer("PRAGMA page_count")
	exec(x.Schema())
	indexedPages := integer("PRAGMA page_count")
	if !present() || uriHistoryDump(t, db) != before || indexedPages < baselinePages || indexedPages > maximumPages {
		t.Fatal("successful adoption changed rows or exceeded the declared logical page bound")
	}
	exec(x.Schema())
	if integer("PRAGMA page_count") != indexedPages || uriHistoryDump(t, db) != before || !present() {
		t.Fatal("idempotent adoption changed a fixture table, its index or logical pages")
	}
	var version string
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("METADATA-INDEX-ADOPTION-COST rows=%d pageSize=%d baselinePages=%d indexedPages=%d extraBytes=%d logicalLimit=%d sqlite=%s readOnlyRefused=true pageFullRefused=true transactionalRollback=true idempotent=true allTablesSHA256=%x",
		count, pageSize, baselinePages, indexedPages, (indexedPages-baselinePages)*pageSize, logicalBytes, version, sha256.Sum256([]byte(before)))
}
