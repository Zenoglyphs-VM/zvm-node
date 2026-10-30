package nft

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	_ "modernc.org/sqlite"
)

type uriHistoryHost struct {
	values  map[common.Hash]string
	refused map[common.Hash]bool
	latest  int
}

func (h *uriHistoryHost) Eth() *ethclient.Client { return nil }
func (h *uriHistoryHost) Log() *slog.Logger      { return slog.New(slog.DiscardHandler) }
func (h *uriHistoryHost) CallString(context.Context, common.Address, string) string {
	return "collection"
}
func (h *uriHistoryHost) CallURI(context.Context, common.Address, []byte) string {
	h.latest++
	return "unexpected-latest"
}
func (h *uriHistoryHost) CallURIAtHash(_ context.Context, _ common.Address, _ []byte, hash common.Hash) (string, error) {
	if h.refused[hash] {
		return "", errors.New("declared hash URI refusal")
	}
	value, ok := h.values[hash]
	if !ok {
		return "", errors.New("unknown fixture hash")
	}
	return value, nil
}

func uriDocument(name string) string {
	return `data:application/json,{"name":"` + name + `","image":"synthetic:` + name + `"}`
}

func uriHistoryFixture(t *testing.T, legacy bool) (*Index, *sql.DB, *uriHistoryHost) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	x := New(Config{})
	for _, statement := range []string{x.Schema(), `CREATE TABLE blocks(number INTEGER PRIMARY KEY,hash TEXT NOT NULL); CREATE TABLE index_meta(key TEXT PRIMARY KEY,value INTEGER);
	 CREATE TABLE logs(address TEXT,topic0 TEXT,topic3 TEXT,data BLOB,block INTEGER,idx INTEGER)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if !legacy {
		if _, err := db.Exec("INSERT INTO index_meta VALUES(?,1)", metadataHistoryVersionKey); err != nil {
			t.Fatal(err)
		}
	}
	host := &uriHistoryHost{values: map[common.Hash]string{}, refused: map[common.Hash]bool{}}
	if err := x.Attach(host); err != nil {
		t.Fatal(err)
	}
	return x, db, host
}

func uriBlock(t *testing.T, db *sql.DB, number uint64, hash common.Hash) {
	t.Helper()
	if _, err := db.Exec("INSERT OR REPLACE INTO blocks VALUES(?,?)", number, hash.Hex()); err != nil {
		t.Fatal(err)
	}
}

func uriLog(x *Index, db *sql.DB, number uint64, hash common.Hash, mint bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	row := indexer.LogRow{Address: common.Address{19: 21}, Block: number, BlockHash: hash}
	if mint {
		row.Topics = []common.Hash{topicTransfer, {}, common.Hash{31: 22}, common.Hash{31: 1}}
	} else {
		row.Topics = []common.Hash{topicMetadataUpdate}
		row.Data = common.LeftPadBytes([]byte{1}, 32)
	}
	var topicID any
	if len(row.Topics) == 4 {
		topicID = row.Topics[3].Hex()
	}
	if _, err := tx.Exec("INSERT INTO logs VALUES(?,?,?,?,?,0)", row.Address.Hex(), row.Topics[0].Hex(), topicID, row.Data, number); err != nil {
		return err
	}
	if err := x.Handle(context.Background(), tx, row); err != nil {
		x.AfterRollback()
		return err
	}
	return tx.Commit()
}

// Compare every fixture table, including all new journal/payload/archive rows.
func uriHistoryDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	names, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := names.Err(); err != nil {
		t.Fatal(err)
	}
	names.Close()
	result := map[string][]string{}
	for _, name := range tables {
		rows, err := db.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var values []string
		for rows.Next() {
			value := make([]any, len(columns))
			args := make([]any, len(columns))
			for i := range value {
				args[i] = &value[i]
			}
			if err := rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, string(raw))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(values)
		result[name] = values
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestURIHistoryRewindMatchesColdTables(t *testing.T) {
	for _, phase := range []string{"changed", "refused", "replacement"} {
		t.Run(phase, func(t *testing.T) {
			x, db, host := uriHistoryFixture(t, false)
			a, b, c := common.Hash{31: 1}, common.Hash{31: 2}, common.Hash{31: 3}
			host.values[a], host.values[b], host.values[c] = uriDocument("A"), uriDocument("B"), uriDocument("C")
			uriBlock(t, db, 1, a)
			if err := uriLog(x, db, 1, a, true); err != nil {
				t.Fatal(err)
			}
			uriBlock(t, db, 2, b)
			host.refused[b] = phase == "refused"
			if err := uriLog(x, db, 2, b, false); err != nil {
				t.Fatal(err)
			}
			var got sql.NullString
			if err := db.QueryRow("SELECT uri FROM nft_tokens").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if phase == "refused" && got.Valid {
				t.Fatal("failed URI read invented a proven cache")
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("DELETE FROM blocks WHERE number>1"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("DELETE FROM logs WHERE block>1"); err != nil {
				t.Fatal(err)
			}
			if err := restoreTokenUpdateHeights(tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := restoreMetadata(tx, 1); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, referenceHost := uriHistoryFixture(t, false)
			referenceHost.values[a] = uriDocument("A")
			uriBlock(t, referenceDB, 1, a)
			if err := uriLog(reference, referenceDB, 1, a, true); err != nil {
				t.Fatal(err)
			}
			if uriHistoryDump(t, db) != uriHistoryDump(t, referenceDB) {
				t.Fatal("rewind did not restore every cold-replay table")
			}
			if phase == "replacement" {
				uriBlock(t, db, 2, c)
				uriBlock(t, referenceDB, 2, c)
				referenceHost.values[c] = uriDocument("C")
				if err := uriLog(x, db, 2, c, false); err != nil {
					t.Fatal(err)
				}
				if err := uriLog(reference, referenceDB, 2, c, false); err != nil {
					t.Fatal(err)
				}
				if uriHistoryDump(t, db) != uriHistoryDump(t, referenceDB) {
					t.Fatal("replacement branch retained orphan URI history")
				}
			}
			if host.latest != 0 || referenceHost.latest != 0 {
				t.Fatal("historical metadata fell back to latest state")
			}
		})
	}
}

func TestURIHistoryWriteRefusalRetainsEveryTable(t *testing.T) {
	for _, phase := range []string{"payload", "history", "cache", "restore"} {
		t.Run(phase, func(t *testing.T) {
			x, db, host := uriHistoryFixture(t, false)
			a, b := common.Hash{31: 1}, common.Hash{31: 2}
			host.values[a], host.values[b] = uriDocument("A"), uriDocument("B")
			uriBlock(t, db, 1, a)
			if err := uriLog(x, db, 1, a, true); err != nil {
				t.Fatal(err)
			}
			uriBlock(t, db, 2, b)
			if phase == "restore" {
				if err := uriLog(x, db, 2, b, false); err != nil {
					t.Fatal(err)
				}
			}
			table, event := "nft_tokens", "UPDATE"
			if phase == "payload" {
				table, event = "nft_metadata_payloads", "INSERT"
			}
			if phase == "history" {
				table, event = "nft_metadata_history", "INSERT"
			}
			if _, err := db.Exec("CREATE TRIGGER refuse_uri BEFORE " + event + " ON " + table + " BEGIN SELECT RAISE(ABORT,'declared URI write refusal'); END"); err != nil {
				t.Fatal(err)
			}
			before := uriHistoryDump(t, db)
			if phase == "restore" {
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if err := restoreMetadata(tx, 1); err == nil {
					tx.Rollback()
					t.Fatal("restore refusal was not reached")
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			} else if err := uriLog(x, db, 2, b, false); err == nil {
				t.Fatal("URI write refusal was not reached")
			}
			if uriHistoryDump(t, db) != before {
				t.Fatal("failed URI transaction changed a table")
			}
		})
	}
}

func TestURIHistorySourceFenceAndCorruptPayloadRefusal(t *testing.T) {
	for _, phase := range []string{"source-hash", "payload-hash"} {
		t.Run(phase, func(t *testing.T) {
			x, db, host := uriHistoryFixture(t, false)
			a := common.Hash{31: 1}
			host.values[a] = uriDocument("A")
			uriBlock(t, db, 1, a)
			if err := uriLog(x, db, 1, a, true); err != nil {
				t.Fatal(err)
			}
			if phase == "source-hash" {
				uriBlock(t, db, 1, common.Hash{31: 99})
			} else {
				if _, err := db.Exec("UPDATE nft_metadata_payloads SET payload=X'00'"); err != nil {
					t.Fatal(err)
				}
			}
			before := uriHistoryDump(t, db)
			if phase == "source-hash" {
				if err := uriLog(x, db, 1, a, false); err == nil {
					t.Fatal("moved source hash was accepted")
				}
			} else {
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if err := restoreMetadata(tx, 1); err == nil {
					tx.Rollback()
					t.Fatal("corrupt payload was accepted")
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			if uriHistoryDump(t, db) != before {
				t.Fatal("refused metadata proof changed existing evidence")
			}
		})
	}
}

func TestURIHistoryPreservesUnprovedLegacyBytes(t *testing.T) {
	_, db, _ := uriHistoryFixture(t, true)
	hash := common.Hash{31: 1}
	uriBlock(t, db, 1, hash)
	if _, err := db.Exec(`INSERT INTO nft_collections(address,name,symbol,contract_uri,meta,first_block) VALUES('collection','old','OLD','legacy-collection','{}',1);
	 INSERT INTO nft_tokens(collection,token_id,owner,uri,meta,name,image,minted_block,updated_block) VALUES('collection','1','owner','legacy-token','{}','old-name','old-image',1,1)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
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
	var uri, meta, name, image string
	if err := db.QueryRow("SELECT uri,meta,name,image FROM nft_metadata_legacy WHERE kind='token'").Scan(&uri, &meta, &name, &image); err != nil {
		t.Fatal(err)
	}
	if uri != "legacy-token" || meta != "{}" || name != "old-name" || image != "old-image" {
		t.Fatal("legacy bytes were changed or assigned invented provenance")
	}
	var known, live int
	if err := db.QueryRow("SELECT COUNT(*) FROM nft_metadata_history").Scan(&known); err != nil || known != 0 {
		t.Fatal("legacy archive became a block proof")
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM nft_tokens WHERE uri IS NOT NULL OR meta IS NOT NULL OR name IS NOT NULL OR image IS NOT NULL").Scan(&live); err != nil || live != 0 {
		t.Fatal("unproved legacy fields remained presented as current metadata")
	}
}

func TestURIHistoryDeduplicatesPayloadsAndKeepsBudgetAfterRollback(t *testing.T) {
	x, db, host := uriHistoryFixture(t, false)
	a, b := common.Hash{31: 1}, common.Hash{31: 2}
	host.values[a], host.values[b] = uriDocument("A"), uriDocument("B")
	uriBlock(t, db, 1, a)
	if err := uriLog(x, db, 1, a, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM nft_metadata_payloads").Scan(&count); err != nil || count != 1 {
		t.Fatal("identical collection/token payloads were copied")
	}
	uriBlock(t, db, 2, b)
	if _, err := db.Exec("CREATE TRIGGER refuse_uri BEFORE INSERT ON nft_metadata_history BEGIN SELECT RAISE(ABORT,'declared history refusal'); END"); err != nil {
		t.Fatal(err)
	}
	if err := uriLog(x, db, 2, b, false); err == nil {
		t.Fatal("declared history refusal was not reached")
	}
	if x.historyBlock != 2 || x.historyBytes <= 0 {
		t.Fatal("rollback renewed the payload admission budget")
	}
	if _, err := db.Exec("DROP TRIGGER refuse_uri"); err != nil {
		t.Fatal(err)
	}
	x.historyBytes = metadataPayloadBlockBudget
	if err := uriLog(x, db, 2, b, false); err != nil {
		t.Fatal(err)
	}
	var value sql.NullString
	if err := db.QueryRow("SELECT uri FROM nft_tokens").Scan(&value); err != nil || value.Valid {
		t.Fatal("exhausted payload budget invented metadata")
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM nft_metadata_payloads").Scan(&count); err != nil || count != 1 {
		t.Fatal(fmt.Sprintf("budget refusal retained a new payload: %d", count))
	}
}
