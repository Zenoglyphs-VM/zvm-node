package indexer_test

import (
	"context"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

// The phase split is the production reader and production writer, exposed only
// by the existing test adapter. No copy of the writer or NFT handler is used.
func TestBackfillSourceFence(t *testing.T) {
	for _, phase := range []string{"unchanged", "rewind", "same-height", "advanced", "missing-middle", "changed-middle", "completed", "empty-to-zero", "handler-refusal", "after-block-refusal", "commit-refusal"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
			if err := chain.SetURI(1, nftCollection, uriRecoveryDocument("original")); err != nil {
				t.Fatal(err)
			}
			chain.Put(2, 0, nftTransfer(nftAlice, nftBob))
			chain.Put(3, 0, nftTransfer(nftBob, nftAlice))
			ix, db, fail := nftRecoveryIndex(t, chain)
			if phase != "empty-to-zero" {
				nftIndexThrough(t, ix, 1, 3)
			}
			key := "backfill-source-fence"
			read := func() func() error {
				t.Helper()
				apply, err := ix.PrepareRecoveryBackfill(ctx, key, "nft")
				if err != nil {
					t.Fatal(err)
				}
				return apply
			}
			apply := read()
			switch phase {
			case "handler-refusal":
				if _, err := db.SQL().Exec("CREATE TRIGGER refuse_backfill BEFORE UPDATE ON nft_tokens BEGIN SELECT RAISE(ABORT,'declared backfill handler refusal'); END"); err != nil {
					t.Fatal(err)
				}
			case "after-block-refusal":
				fail("after-block")
			case "commit-refusal":
				fail("commit")
			case "rewind":
				if err := ix.Rewind(2); err != nil {
					t.Fatal(err)
				}
			case "same-height":
				if err := ix.Rewind(2); err != nil {
					t.Fatal(err)
				}
				chain.Put(3, 1, nftTransfer(nftBob, nftOperator))
				if err := ix.IndexBlock(ctx, 3); err != nil {
					t.Fatal(err)
				}
			case "completed":
				if err := read()(); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "advanced":
				chain.Put(4, 0, nftTransfer(nftAlice, nftBob))
				if err := ix.IndexBlock(ctx, 4); err != nil {
					t.Fatal(err)
				}
			case "missing-middle":
				// A declared source-store fault under an unchanged terminal head.
				if _, err := db.SQL().Exec("DELETE FROM blocks WHERE number=2"); err != nil {
					t.Fatal(err)
				}
			case "changed-middle":
				if _, err := db.SQL().Exec("UPDATE blocks SET hash=? WHERE number=2", (common.Hash{31: 99}).Hex()); err != nil {
					t.Fatal(err)
				}
			case "empty-to-zero":
				if _, err := db.SQL().Exec(`INSERT INTO blocks(number,hash,parent,timestamp,momentum,txs,user_txs,gas_used,gas_limit,base_fee,size)
				 VALUES(0,?,?,0,0,0,0,0,0,'0',0)`, (common.Hash{31: 99}).Hex(), (common.Hash{}).Hex()); err != nil {
					t.Fatal(err)
				}
			}
			before := indexer.RecoveryState(t, db)
			calls := chain.RecoveryCallCount()
			err := apply()
			if phase == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
				var marked uint64
				if err := db.SQL().QueryRow("SELECT value FROM index_meta WHERE key=?", key).Scan(&marked); err != nil || marked != 3 {
					t.Fatalf("successful backfill marker: %d %v", marked, err)
				}
				reference, referenceDB, _ := nftRecoveryIndex(t, chain)
				nftIndexThrough(t, reference, 1, 3)
				replay, err := reference.PrepareRecoveryBackfill(ctx, key, "nft")
				if err != nil {
					t.Fatal(err)
				}
				if err := replay(); err != nil {
					t.Fatal(err)
				}
				indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
				return
			}
			if phase == "completed" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("stale source input was applied or marked complete")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
			writeRefusal := phase == "handler-refusal" || phase == "after-block-refusal" || phase == "commit-refusal"
			if !writeRefusal && chain.RecoveryCallCount() != calls {
				t.Fatal("source refusal or completed-key skip consumed RPC work")
			}
			var marked int
			if err := db.SQL().QueryRow("SELECT COUNT(*) FROM index_meta WHERE key=?", key).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked != 0 && phase != "completed" {
				t.Fatal("refused source input published a completed-job marker")
			}
		})
	}
}
