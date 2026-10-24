package indexer

import (
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/ethereum/go-ethereum/core/types"
)

// These adapters are compiled only in tests. External integration tests can
// install the real NFT plugin without importing it into this package (a cycle),
// and without duplicating the read-only loopback chain or all-table oracle.
type RecoveryChain struct{ chain *fakeChain }

func NewRecoveryChain() *RecoveryChain { return &RecoveryChain{newFakeChain()} }

func (c *RecoveryChain) Put(number uint64, variant byte, logs ...*types.Log) {
	c.chain.putWithLogs(number, variant, logs)
}

func NewRecoveryFailure() (Plugin, func(string)) {
	p := &n04Plugin{}
	return p, func(phase string) { p.fail = phase }
}

func NewRecoveryIndex(t *testing.T, chain *RecoveryChain, plugins ...Plugin) (*Indexer, *store.DB) {
	t.Helper()
	base, db := newReorgIndex(t, chain.chain)
	db.SQL().SetMaxOpenConns(1)
	if _, err := db.SQL().Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	var enabled int
	if err := db.SQL().QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("deferred commit refusal unavailable: %v", err)
	}
	ix, err := New(db, base.eth, base.cfg, base.log, plugins...)
	if err != nil {
		t.Fatal(err)
	}
	return ix, db
}

func RecoveryState(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	return n04IndexState(t, db)
}

func AssertRecoveryState(t *testing.T, got, want map[string]string) {
	t.Helper()
	n04IndexSame(t, got, want)
}
