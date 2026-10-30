package indexer

import (
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
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

// SetURI supplies distinct hash-selected URI observations and an intentionally
// wrong latest-state answer. It is confined to the existing loopback fixture.
func (c *RecoveryChain) SetURI(number uint64, address common.Address, uri string) error {
	typ, err := abi.NewType("string", "", nil)
	if err != nil {
		return err
	}
	fields := abi.Arguments{{Type: typ}}
	raw, err := fields.Pack(uri)
	if err != nil {
		return err
	}
	wrong, err := fields.Pack("unexpected-latest-state")
	if err != nil {
		return err
	}
	c.chain.mu.Lock()
	defer c.chain.mu.Unlock()
	block := c.chain.blocks[number]
	if block == nil {
		return fmt.Errorf("URI fixture block %d is missing", number)
	}
	if c.chain.hashAnswers == nil {
		c.chain.hashAnswers = map[common.Hash]map[common.Address]map[[4]byte][]byte{}
	}
	hash := block.Hash()
	if c.chain.hashAnswers[hash] == nil {
		c.chain.hashAnswers[hash] = map[common.Address]map[[4]byte][]byte{}
	}
	if c.chain.hashAnswers[hash][address] == nil {
		c.chain.hashAnswers[hash][address] = map[[4]byte][]byte{}
	}
	if c.chain.answers[address] == nil {
		c.chain.answers[address] = map[[4]byte][]byte{}
	}
	for _, sig := range []string{"tokenURI(uint256)", "contractURI()"} {
		var selector [4]byte
		copy(selector[:], crypto.Keccak256([]byte(sig))[:4])
		c.chain.hashAnswers[hash][address][selector] = raw
		c.chain.answers[address][selector] = wrong
	}
	return nil
}
