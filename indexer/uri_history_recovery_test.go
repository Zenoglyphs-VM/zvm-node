package indexer_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func uriHistoryLogs() []*types.Log {
	return []*types.Log{
		{Address: nftCollection, Topics: []common.Hash{crypto.Keccak256Hash([]byte("MetadataUpdate(uint256)"))}, Data: common.LeftPadBytes([]byte{1}, 32)},
		{Address: nftCollection, Topics: []common.Hash{crypto.Keccak256Hash([]byte("ContractURIUpdated()"))}},
	}
}

func uriRecoveryDocument(name string) string {
	return `data:application/json,{"name":"` + name + `","image":"synthetic:` + name + `"}`
}

func TestURIHistoryRealIndexerRewindPreservesAllTableOracle(t *testing.T) {
	for _, phase := range []string{"graceful", "commit-refusal", "replacement"} {
		t.Run(phase, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
			if err := chain.SetURI(1, nftCollection, uriRecoveryDocument("A")); err != nil {
				t.Fatal(err)
			}
			chain.Put(2, 0, uriHistoryLogs()...)
			if err := chain.SetURI(2, nftCollection, uriRecoveryDocument("B")); err != nil {
				t.Fatal(err)
			}
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 2)
			var uri string
			if err := db.SQL().QueryRow("SELECT uri FROM nft_tokens WHERE collection=? AND token_id='1'", nftCollection.Hex()).Scan(&uri); err != nil || uri != uriRecoveryDocument("B") {
				t.Fatalf("production URI path did not select block 2: %q %v", uri, err)
			}
			var verified int
			if err := db.SQL().QueryRow("SELECT COUNT(*) FROM nft_metadata_history WHERE uri_verified=1").Scan(&verified); err != nil || verified != 4 {
				t.Fatalf("missing actual hash URI observations: %d %v", verified, err)
			}
			refuse := phase == "commit-refusal"
			ix.BeforeRewindCommit(func(*sql.Tx, uint64) error {
				if refuse {
					return errors.New("declared URI rewind commit refusal")
				}
				return nil
			})
			if refuse {
				before := indexer.RecoveryState(t, db)
				if err := ix.Rewind(1); err == nil {
					t.Fatal("declared rewind refusal did not execute")
				}
				indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
				refuse = false
			}
			if err := ix.Rewind(1); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 1)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
			if phase == "replacement" {
				chain.Put(2, 1, uriHistoryLogs()...)
				if err := chain.SetURI(2, nftCollection, uriRecoveryDocument("C")); err != nil {
					t.Fatal(err)
				}
				if err := ix.IndexBlock(context.Background(), 2); err != nil {
					t.Fatal(err)
				}
				cold, coldDB, _ := nftRecoveryIndex(t, chain)
				nftIndexThrough(t, cold, 1, 2)
				indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, coldDB))
			}
		})
	}
}
