package indexer_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

// A provider callback writes through the real index DB's one-connection pool.
// A retained read snapshot or canonical write transaction would block it until
// the bounded probe times out. It changes no data in the successful schedules.
func TestPreparedURIWriterBoundary(t *testing.T) {
	for _, phase := range []string{"live", "backfill", "head-moved"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
			if err := chain.SetURI(1, nftCollection, uriRecoveryDocument("prepared-first")); err != nil {
				t.Fatal(err)
			}
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 1)
			chain.Put(2, 0, nftTransfer(common.Address{}, nftBob))
			if err := chain.SetURI(2, nftCollection, uriRecoveryDocument("prepared-second")); err != nil {
				t.Fatal(err)
			}
			probes := make(chan error, 32)
			moved := make(chan map[string]string, 1)
			var once sync.Once
			chain.SetRecoveryCallProbe(func(callctx context.Context) error {
				bounded, cancel := context.WithTimeout(callctx, time.Second)
				defer cancel()
				result, err := db.SQL().ExecContext(bounded, "UPDATE index_meta SET value=value WHERE key='nft_metadata_history_v1'")
				if err == nil {
					var rows int64
					rows, err = result.RowsAffected()
					if err == nil && rows != 1 {
						err = fmt.Errorf("probe did not touch the real recovery marker: %d", rows)
					}
				}
				if err == nil && phase == "head-moved" {
					once.Do(func() {
						_, err = db.SQL().ExecContext(bounded, "UPDATE blocks SET hash=? WHERE number=1", (common.Hash{31: 99}).Hex())
						if err == nil {
							moved <- indexer.RecoveryState(t, db)
						}
					})
				}
				probes <- err
				return err
			})
			var err error
			if phase == "backfill" {
				var apply func() error
				apply, err = ix.PrepareRecoveryBackfill(ctx, "prepared-writer-boundary", "nft")
				if err == nil {
					err = apply()
				}
			} else {
				err = ix.IndexBlock(ctx, 2)
			}
			if len(probes) == 0 {
				t.Fatal("schedule made no real provider call")
			}
			for len(probes) > 0 {
				if probe := <-probes; probe != nil {
					t.Fatalf("provider was called with a main SQL transaction held: %v", probe)
				}
			}
			if phase == "head-moved" {
				if err == nil {
					t.Fatal("source head moved during preparation but the block committed")
				}
				if len(moved) != 1 {
					t.Fatal("declared source movement did not run")
				}
				indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), <-moved)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before := indexer.RecoveryState(t, db)
			if phase == "backfill" {
				var apply func() error
				apply, err = ix.PrepareRecoveryBackfill(ctx, "prepared-writer-boundary", "nft")
				if err == nil {
					err = apply()
				}
			} else {
				err = ix.IndexBlock(ctx, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(probes) != 0 {
				t.Fatal("already-complete input repeated external calls")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
			var incomplete sql.NullInt64
			if phase == "backfill" {
				if err := db.SQL().QueryRow("SELECT value FROM index_meta WHERE key='prepared-writer-boundary'").Scan(&incomplete); err != nil || !incomplete.Valid || incomplete.Int64 != 1 {
					t.Fatalf("whole-backfill completion marker: %v %v", incomplete, err)
				}
			}
		})
	}
}

// Use the real preparation boundary and real block writer. A source-store hole
// is a declared fixture fault; these schedules are not process-race evidence.
func TestPreparedURIIndexedGap(t *testing.T) {
	for _, phase := range []string{"unchanged", "rewind", "same-head-hole", "replacement"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			chain := indexer.NewRecoveryChain()
			chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
			chain.Put(2, 0, nftTransfer(common.Address{}, nftBob))
			chain.Put(3, 0, nftTransfer(nftBob, nftAlice))
			for number := uint64(1); number <= 3; number++ {
				if err := chain.SetURI(number, nftCollection, uriRecoveryDocument("indexed-gap")); err != nil {
					t.Fatal(err)
				}
			}
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 3)
			apply, err := ix.PrepareRecoveryIndexedBlock(ctx, 2)
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "rewind":
				if err := ix.Rewind(1); err != nil {
					t.Fatal(err)
				}
			case "same-head-hole":
				if _, err := db.SQL().Exec("DELETE FROM blocks WHERE number=2"); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				if err := ix.Rewind(1); err != nil {
					t.Fatal(err)
				}
				chain.Put(2, 1, nftTransfer(common.Address{}, nftOperator))
				if err := chain.SetURI(2, nftCollection, uriRecoveryDocument("indexed-replacement")); err != nil {
					t.Fatal(err)
				}
				if err := ix.IndexBlock(ctx, 2); err != nil {
					t.Fatal(err)
				}
			}
			before := indexer.RecoveryState(t, db)
			calls := chain.RecoveryCallCount()
			err = apply()
			if phase == "unchanged" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("the previously indexed source disappeared or changed, but stale input was applied")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
			if chain.RecoveryCallCount() != calls {
				t.Fatal("a stale idempotence decision consumed unprepared provider calls")
			}
			if phase == "rewind" {
				// Refusal requires fresh discovery, not a permanently blocked height.
				if err := ix.IndexBlock(ctx, 2); err != nil {
					t.Fatal(err)
				}
				reference, referenceDB, _ := nftRecoveryIndex(t, chain)
				nftIndexThrough(t, reference, 1, 2)
				indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
			}
		})
	}
}
