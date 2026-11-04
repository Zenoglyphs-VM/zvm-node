package indexer_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/indexer/nft"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	nftCollection = common.Address{19: 21}
	nftAlice      = common.Address{19: 22}
	nftBob        = common.Address{19: 23}
	nftOperator   = common.Address{19: 24}
	nftExchange   = common.Address{19: 25}
)

func nftTransfer(from, to common.Address) *types.Log {
	return &types.Log{Address: nftCollection, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)")), common.BytesToHash(from[:]), common.BytesToHash(to[:]), {31: 1},
	}}
}

func nftApproval() *types.Log {
	return &types.Log{Address: nftCollection, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("Approval(address,address,uint256)")), common.BytesToHash(nftBob[:]), common.BytesToHash(nftOperator[:]), {31: 1},
	}}
}

func nftSale() *types.Log {
	var data []byte
	for _, word := range [][]byte{nil, nftCollection[:], {1}, nil, big.NewInt(1_000_000_000_000_000_000).Bytes(), nil, nil} {
		data = append(data, common.LeftPadBytes(word, 32)...)
	}
	return &types.Log{Address: nftExchange, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)")), {31: 9}, common.BytesToHash(nftAlice[:]), common.BytesToHash(nftBob[:]),
	}, Data: data}
}

func nftRecoveryIndex(t *testing.T, chain *indexer.RecoveryChain) (*indexer.Indexer, *store.DB, func(string)) {
	t.Helper()
	fault, fail := indexer.NewRecoveryFailure()
	plugin := nft.New(nft.Config{Apps: func() nft.Apps { return nft.Apps{Exchange: nftExchange} }})
	ix, db := indexer.NewRecoveryIndex(t, chain, plugin, fault)
	return ix, db, fail
}

func nftIndexThrough(t *testing.T, ix *indexer.Indexer, from, through uint64) {
	t.Helper()
	for n := from; n <= through; n++ {
		if err := ix.IndexBlock(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
}

// The actual NFT plugin handles the declared mint, transfer, approval and sale
// logs. No signature, wallet, exchange call or payment is executed.
func TestNFTFailedBlockRetryMatchesCleanIndex(t *testing.T) {
	for _, phase := range []string{"handle", "after-block", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
			chain.Put(2, 0, nftTransfer(nftAlice, nftBob), nftApproval(), nftSale())
			ix, db, fail := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 1)
			prefix := indexer.RecoveryState(t, db)
			fail(phase)
			if err := ix.IndexBlock(context.Background(), 2); err == nil {
				t.Fatal("declared NFT block refusal was not reached")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), prefix)
			fail("")
			nftIndexThrough(t, ix, 2, 2)
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 2)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

func TestNFTAbortedCollectionDoesNotSurviveReplacement(t *testing.T) {
	for _, phase := range []string{"handle", "after-block", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0)
			chain.Put(2, 0, nftTransfer(common.Address{}, nftAlice))
			ix, db, fail := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 1)
			prefix := indexer.RecoveryState(t, db)
			fail(phase)
			if err := ix.IndexBlock(context.Background(), 2); err == nil {
				t.Fatal("declared collection transaction refusal was not reached")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), prefix)
			chain.Put(2, 1) // The accepted branch never contained this collection.
			fail("")
			nftIndexThrough(t, ix, 2, 2)
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 2)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

func TestNFTRewindRemovesOrphanCollectionStatistics(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0)
	chain.Put(2, 0, nftTransfer(common.Address{}, nftAlice))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 2)
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	reference, referenceDB, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, reference, 1, 1)
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
}

func TestNFTFailedRewindRetainsEveryTable(t *testing.T) {
	for _, phase := range []string{"restore", "rebuild", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0)
			chain.Put(2, 0, nftTransfer(common.Address{}, nftAlice))
			ix, db, fail := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 2)
			before := indexer.RecoveryState(t, db)
			fail(phase)
			if err := ix.Rewind(1); err == nil {
				t.Fatal("declared NFT rewind refusal was not reached")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
			fail("")
			if err := ix.Rewind(1); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 1)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

type rollbackCounter struct {
	indexer.Plugin
	count int
}

func (p *rollbackCounter) AfterRollback() { p.count++ }

func TestPluginRollbackNotificationFollowsTransactionOutcome(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0)
	chain.Put(2, 0)
	fault, fail := indexer.NewRecoveryFailure()
	counter := &rollbackCounter{Plugin: fault}
	ix, _ := indexer.NewRecoveryIndex(t, chain, counter)
	nftIndexThrough(t, ix, 1, 1)
	if counter.count != 0 {
		t.Fatal("successful initialization or block notified rollback")
	}
	fail("commit")
	if err := ix.IndexBlock(context.Background(), 2); err == nil {
		t.Fatal("deferred commit refusal was not reached")
	}
	if counter.count != 1 {
		t.Fatalf("commit refusal notified rollback %d times, want 1", counter.count)
	}
	fail("")
	nftIndexThrough(t, ix, 2, 2)
	fail("rebuild")
	if err := ix.Rewind(1); err == nil {
		t.Fatal("rewind refusal was not reached")
	}
	if counter.count != 2 {
		t.Fatalf("failed rewind notified rollback %d times, want 2 total", counter.count)
	}
	fail("")
	if err := ix.Rewind(2); err != nil {
		t.Fatal(err)
	}
	if counter.count != 2 {
		t.Fatal("successful block retry or rewind notified rollback")
	}
}

func TestNFTStartupRepairsMissingOrOrphanStatistics(t *testing.T) {
	for _, damage := range []string{"missing", "orphan", "mixed"} {
		t.Run(damage, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			other := nftTransfer(common.Address{}, nftAlice)
			other.Address = common.Address{19: 26}
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), other)
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 1)
			before := indexer.RecoveryState(t, db)
			// Declared pre-upgrade derived-table damage. Retained token,
			// collection, transfer and sale facts remain untouched.
			if damage != "orphan" {
				if _, err := db.SQL().Exec("DELETE FROM nft_collection_stats WHERE collection=?", nftCollection.Hex()); err != nil {
					t.Fatal(err)
				}
			}
			if damage != "missing" {
				if _, err := db.SQL().Exec("INSERT INTO nft_collection_stats VALUES(?,1,1,0,0,1)", (common.Address{19: 27}).Hex()); err != nil {
					t.Fatal(err)
				}
			}
			fault, _ := indexer.NewRecoveryFailure()
			_, err := indexer.New(db, ix.Eth(), &rollup.Config{L1StartHeight: 2, L2ChainID: 69}, ix.Log(), nft.New(nft.Config{}), fault)
			if err != nil {
				t.Fatal(err)
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
		})
	}
}
