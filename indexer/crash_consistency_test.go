package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var n04Injected = errors.New("declared n04 plugin refusal")

// This is a declared application plugin, not a substitute for qualifying NFT,
// DEX or order-book semantics. Its writes participate in the real index SQL
// transaction; block and receipt replies come from the existing loopback port.
type n04Plugin struct{ fail string }

func (*n04Plugin) Name() string      { return "n04-fixture" }
func (*n04Plugin) Attach(Host) error { return nil }
func (*n04Plugin) Schema() string {
	return `CREATE TABLE IF NOT EXISTS n04_events(block INTEGER,idx INTEGER,tx TEXT,PRIMARY KEY(block,idx));
	CREATE TABLE IF NOT EXISTS n04_summary(id INTEGER PRIMARY KEY,total INTEGER);
	INSERT OR IGNORE INTO n04_summary VALUES(1,0);
	CREATE TABLE IF NOT EXISTS n04_parent(id INTEGER PRIMARY KEY);
	CREATE TABLE IF NOT EXISTS n04_commit_guard(id INTEGER REFERENCES n04_parent(id) DEFERRABLE INITIALLY DEFERRED);`
}
func (*n04Plugin) Jobs() []Job        { return nil }
func (*n04Plugin) Stale(*sql.DB) bool { return false }
func (p *n04Plugin) Handle(_ context.Context, tx *sql.Tx, row LogRow) error {
	if _, err := tx.Exec("INSERT INTO n04_events VALUES(?,?,?)", row.Block, row.Index, row.Tx); err != nil {
		return err
	}
	if p.fail == "handle" {
		return n04Injected
	}
	return nil
}
func (p *n04Plugin) AfterBlock(tx *sql.Tx, _ uint64) error {
	if _, err := tx.Exec("UPDATE n04_summary SET total=(SELECT count(*) FROM n04_events)"); err != nil {
		return err
	}
	if p.fail == "after-block" {
		return n04Injected
	}
	if p.fail == "commit" {
		_, err := tx.Exec("INSERT INTO n04_commit_guard VALUES(99)")
		return err // The deferred constraint fails only at the real Commit.
	}
	return nil
}
func (*n04Plugin) Deletes() []string { return []string{"DELETE FROM n04_events WHERE block > ?"} }
func (p *n04Plugin) Restores() []string {
	if p.fail == "restore" {
		return []string{"UPDATE n04_summary SET total=(SELECT count(*) FROM n04_events) WHERE ? >= 0", "SELECT n04_unavailable_function(?)"}
	}
	return []string{"UPDATE n04_summary SET total=(SELECT count(*) FROM n04_events) WHERE ? >= 0"}
}
func (p *n04Plugin) Rebuild(tx *sql.Tx, _ uint64) error {
	if _, err := tx.Exec("UPDATE n04_summary SET total=(SELECT count(*) FROM n04_events)"); err != nil {
		return err
	}
	if p.fail == "rebuild" {
		return n04Injected
	}
	if p.fail == "commit" {
		_, err := tx.Exec("INSERT INTO n04_commit_guard VALUES(99)")
		return err
	}
	return nil
}

func n04Index(t *testing.T, chain *fakeChain, p *n04Plugin) (*Indexer, *store.DB, string) {
	t.Helper()
	base, db := newReorgIndex(t, chain)
	// One owned connection makes the fixture's foreign-key setting explicit.
	db.SQL().SetMaxOpenConns(1)
	if _, err := db.SQL().Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	var enabled int
	if err := db.SQL().QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("deferred commit-refusal fixture unavailable: %v", err)
	}
	ix, err := New(db, base.eth, base.cfg, base.log, p)
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := db.SQL().QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil || name != "main" || !filepath.IsAbs(path) {
		t.Fatalf("owned database path unavailable: %v", err)
	}
	return ix, db, path
}

func n04IndexState(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	names, err := db.SQL().Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{}
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
	out := map[string]string{}
	for _, table := range tables {
		rows, err := db.SQL().Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := [][]any{}
		for rows.Next() {
			row := make([]any, len(columns))
			pointers := make([]any, len(row))
			for i := range row {
				pointers[i] = &row[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, value := range row {
				if raw, ok := value.([]byte); ok {
					row[i] = append([]byte(nil), raw...)
				}
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		canonical := make([]string, len(values))
		for i, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			canonical[i] = string(raw)
		}
		sort.Strings(canonical)
		raw, err := json.Marshal(canonical)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = string(raw)
	}
	return out
}

func n04IndexSame(t *testing.T, got, want map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		for table, value := range want {
			if got[table] != value {
				t.Errorf("index/plugin table %s differs from canonical reference", table)
			}
		}
		t.FailNow()
	}
}

func n04Chain() *fakeChain {
	c := newFakeChain()
	for n := uint64(1); n <= 2; n++ {
		c.putWithLogs(n, 0, []*types.Log{{Address: common.Address{19: 7}, Topics: []common.Hash{{0: 99}}, Data: []byte{byte(n)}}})
	}
	return c
}

func TestIndexPluginRefusalRollsBackAndReplaysEveryTable(t *testing.T) {
	for _, phase := range []string{"handle", "after-block", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain, plugin := n04Chain(), &n04Plugin{}
			ix, db, path := n04Index(t, chain, plugin)
			if err := ix.IndexBlock(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			before := n04IndexState(t, db)
			plugin.fail = phase
			if err := ix.IndexBlock(context.Background(), 2); err == nil {
				t.Fatal("declared index/plugin or deferred commit refusal was not reached")
			}
			n04IndexSame(t, n04IndexState(t, db), before)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			n04IndexSame(t, n04IndexState(t, reopened), before)
			plugin.fail = ""
			restored, err := New(reopened, ix.eth, ix.cfg, ix.log, plugin)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.IndexBlock(context.Background(), 2); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := n04Index(t, chain, &n04Plugin{})
			for n := uint64(1); n <= 2; n++ {
				if err := reference.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			n04IndexSame(t, n04IndexState(t, reopened), n04IndexState(t, referenceDB))
		})
	}
}

func TestIndexRewindRefusalKeepsPluginAndCoreStateTogether(t *testing.T) {
	for _, phase := range []string{"delete", "restore", "rebuild", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain, plugin := n04Chain(), &n04Plugin{}
			ix, db, _ := n04Index(t, chain, plugin)
			for n := uint64(1); n <= 2; n++ {
				if err := ix.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			before := n04IndexState(t, db)
			plugin.fail = phase
			if phase == "delete" {
				if _, err := db.SQL().Exec("CREATE TRIGGER n04_delete_refusal AFTER DELETE ON n04_events BEGIN SELECT RAISE(ABORT,'n04_delete_refusal'); END"); err != nil {
					t.Fatal(err)
				}
			}
			hooks := 0
			ix.AfterRewind(func(uint64) { hooks++ })
			if err := ix.Rewind(1); err == nil {
				t.Fatal("declared rewind refusal was not reached")
			}
			if hooks != 0 {
				t.Fatal("post-commit hook ran for an uncommitted rewind")
			}
			n04IndexSame(t, n04IndexState(t, db), before)
			plugin.fail = ""
			if phase == "delete" {
				if _, err := db.SQL().Exec("DROP TRIGGER n04_delete_refusal"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ix.Rewind(1); err != nil {
				t.Fatal(err)
			}
			if hooks != 1 {
				t.Fatal("committed rewind did not notify exactly once")
			}
			reference, referenceDB, _ := n04Index(t, chain, &n04Plugin{})
			if err := reference.IndexBlock(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
		})
	}
}
