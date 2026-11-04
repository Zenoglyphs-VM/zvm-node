package orderbook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
)

// Both profiles use hash-state persistence, 16 MiB clean/dirty caches, no state
// snapshots and synchronous flushing. The buffered profile turns ArchiveMode
// off and leaves recent state dirty until the real client flushes it. These are
// bounded in-process profiles, not a production daemon/default-size claim.
func TestValidatedPersistenceProfilesRecoverOwnedProcessExits(t *testing.T) {
	for _, profile := range []struct {
		name     string
		buffered bool
	}{{"archive", false}, {"buffered-hash-16mb", true}} {
		t.Run(profile.name, func(t *testing.T) {
			scenario := newExecutionScenarioProfile(t, true, profile.buffered)
			if err := scenario.cfg.CheckForks(); err != nil {
				t.Fatal(err)
			}
			ownedRecoveryMatrix(t, scenario)
		})
	}
}

// Checkpoints only pause derivation after its committed notification. Indexing
// and order processing run solely through their production follower loops;
// this helper never calls IndexBlock or Book.step. Stable source horizons make
// chain-derived closure facts comparable without dropping SQL oracle columns.
func (r *executionRun) workerStages(stages ...uint64) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	releases := map[uint64]chan struct{}{}
	for _, stage := range stages {
		releases[stage] = make(chan struct{})
	}
	r.nomMu.Lock()
	r.limit = stages[len(stages)-1]
	r.nomMu.Unlock()
	r.d.OnDerived = func(row *store.Derived) {
		if release := releases[row.Momentum.Height]; release != nil {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	r.d.OnRewind = func(number uint64) { r.callbackError = r.ix.Rewind(number) }
	r.book.cfg.Poll = 10 * time.Millisecond
	done := make(chan error, 3)
	go func() { done <- r.d.Run(ctx) }()
	go func() { r.ix.FollowCanonical(ctx, r.d.Head); done <- nil }()
	go func() { done <- r.book.Run(ctx) }()
	joined := false
	join := func() {
		if joined {
			return
		}
		cancel()
		for i := 0; i < 3; i++ {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					r.t.Errorf("worker returned an unexpected error: %v", err)
				}
			case <-time.After(35 * time.Second):
				r.t.Fatal("owned worker did not join after cancellation")
			}
		}
		joined = true
	}
	defer join()
	for _, height := range stages {
		for {
			head := r.d.Head()
			indexHead, err := r.ix.Head()
			if err != nil {
				r.t.Fatal(err)
			}
			cursor, err := r.book.meta("scanned:" + r.scenario.exchange.Hex())
			if err != nil {
				r.t.Fatal(err)
			}
			ready := head.Height == height && indexHead == height-1 && cursor == height-1
			if ready && height >= 7 {
				for i, order := range r.scenario.orders {
					var status, checkedHash string
					var filled, code int
					err := r.db.SQL().QueryRow("SELECT status,filled,code,checked_hash FROM nft_orders WHERE hash=?", order.Hash().Hex()).Scan(&status, &filled, &code, &checkedHash)
					if err != nil {
						ready = false
						break
					}
					wantStatus, wantCode, wantFilled := expectedExecutedMarketState(r.scenario, r.branch, height, i)
					ready = ready && status == wantStatus && filled == wantFilled && code == int(wantCode)
					if wantStatus == StatusOpen {
						ready = ready && checkedHash == head.L2Hash.Hex()
					}
				}
			}
			if ready {
				break
			}
			select {
			case <-ctx.Done():
				r.t.Fatalf("production worker horizon %d did not converge: derived=%d index=%d cursor=%d", height, head.Height, indexHead, cursor)
			case <-time.After(5 * time.Millisecond):
			}
		}
		close(releases[height])
	}
	join()
	if r.callbackError != nil {
		r.t.Fatal(r.callbackError)
	}
	if r.scenario.validated {
		code, err := r.eth.CodeAt(context.Background(), rollup.FeeSplitterAddr, nil)
		if err != nil || len(code) == 0 {
			r.t.Fatal("shipped fee splitter was not deployed through the native lane")
		}
		var dropped int
		if err := r.db.SQL().QueryRow("SELECT count(*) FROM messages WHERE status IN ('dropped','invalid')").Scan(&dropped); err != nil || dropped != 0 {
			r.t.Fatalf("loader-accepted native messages were refused: %d, %v", dropped, err)
		}
	}
}

func TestValidatedProductionWorkersConvergeAfterRestoreAndReplacement(t *testing.T) {
	for _, profile := range []struct {
		name     string
		buffered bool
	}{{"archive", false}, {"buffered-hash-16mb", true}} {
		t.Run(profile.name, func(t *testing.T) {
			scenario := newExecutionScenarioProfile(t, true, profile.buffered)
			reference := newExecutionRun(t, scenario)
			reference.workerStages(7, 8, 9)
			reference.assertExpected()
			want := reference.state()
			for _, phase := range []string{"reopen", "quiet-replacement"} {
				t.Run(phase, func(t *testing.T) {
					r := newExecutionRun(t, scenario)
					if phase == "reopen" {
						r.workerStages(7)
						r.close()
						r.open()
						r.workerStages(8, 9)
					} else {
						r.nomMu.Lock()
						r.branch = 1
						r.nomMu.Unlock()
						r.workerStages(7, 8, 9)
						before := strings.Join(r.state()["executed-engine"], ",")
						if before == strings.Join(want["executed-engine"], ",") {
							t.Fatal("replacement did not change actual executed state")
						}
						r.nomMu.Lock()
						r.branch = 0
						r.nomMu.Unlock()
						r.workerStages(8, 9)
					}
					r.assertExpected()
					assertExecutionState(t, r.state(), want)
					r.close()
					r.open()
					r.workerStages(9)
					r.assertExpected()
					assertExecutionState(t, r.state(), want)
					t.Logf("loader-accepted profile=%s phase=%s worker_loops=deriver,indexer,book canonical_head=%s", profile.name, phase, fmt.Sprint(r.d.Head().L2Number))
				})
			}
		})
	}
}
