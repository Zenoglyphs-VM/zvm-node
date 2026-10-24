package indexer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/nodecmd"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

type marketStartupFixture struct {
	ix        *indexer.Indexer
	db        *store.DB
	result    nodecmd.Result
	ctx       context.Context
	cancel    context.CancelFunc
	hash      common.Hash
	intent    string
	starts    atomic.Int32
	notices   atomic.Int32
	requests  atomic.Int32
	firstRead chan struct{}
}

func newMarketStartupFixture(t *testing.T, height uint64, withRPC bool) *marketStartupFixture {
	t.Helper()
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0)
	chain.Put(2, 0)
	base, db := indexer.NewRecoveryIndex(t, chain)
	nftIndexThrough(t, base, 1, 2)
	if _, err := orderbook.New(orderbook.Config{DB: db.SQL()}); err != nil {
		t.Fatal(err)
	}
	hash := storedRecoveryOrder(t, db.SQL())
	intent := storedRecoveryIntent(t, db.SQL(), hash)
	// Declare the old commit gap: the index rewound, then the process stopped
	// before its separate book callback. No current callback is bypassed.
	if err := base.Rewind(height); err != nil {
		t.Fatal(err)
	}
	chain.Put(2, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &marketStartupFixture{db: db, ctx: ctx, cancel: cancel, hash: hash, intent: intent, firstRead: make(chan struct{})}
	m := &nodecmd.Market{}
	flags := flag.NewFlagSet("declared-startup-gap", flag.ContinueOnError)
	m.Flags(flags)
	eth := base.Eth()
	if withRPC {
		var first sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				ID     json.RawMessage
				Method string
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			if request.Method != "eth_getBlockByNumber" {
				t.Errorf("undeclared worker RPC: %s", request.Method)
			}
			f.requests.Add(1)
			first.Do(func() { close(f.firstRead) })
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID,
				"error": map[string]any{"code": -32000, "message": "declared read-only startup port"}})
		}))
		t.Cleanup(server.Close)
		client, err := ethclient.Dial(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		eth = client
		path := filepath.Join(t.TempDir(), "declared-apps.json")
		raw, err := json.Marshal(map[string]any{"chainId": 69, "exchange": (common.Address{19: 51}).Hex()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := flags.Set("apps", path); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &rollup.Config{L1StartHeight: 2, L2ChainID: 69}
	result, err := m.Build(ctx, nodecmd.Env{Rollup: cfg, DB: db, Eth: eth, Log: base.Log()})
	if err != nil {
		t.Fatal(err)
	}
	ix, err := indexer.New(db, base.Eth(), cfg, base.Log(), result.Plugins...)
	if err != nil {
		t.Fatal(err)
	}
	if result.Start == nil {
		t.Fatal("market did not provide its deferred worker")
	}
	originalStart := result.Start
	result.Start = func(ctx context.Context) { f.starts.Add(1); originalStart(ctx) }
	result.OnRewind = func(uint64) { f.notices.Add(1) }
	f.ix, f.result = ix, result
	return f
}

func (f *marketStartupFixture) prepare(extra ...nodecmd.Result) (func() error, error) {
	return nodecmd.PrepareExtensions(f.ctx, f.ix, append([]nodecmd.Result{f.result}, extra...))
}

func TestMarketStartupReconcilesAnEarlierIndexBookCommitGap(t *testing.T) {
	f := newMarketStartupFixture(t, 1, false)
	start, err := f.prepare()
	if err != nil || start == nil {
		t.Fatalf("startup reconciliation failed: %v", err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("worker started before node initialization was accepted")
	}
	var status string
	var checked, code, cursor int64
	var closed sql.NullInt64
	if err := f.db.SQL().QueryRow("SELECT status,code,checked_at,closed_block FROM nft_orders WHERE hash=?", f.hash.Hex()).Scan(&status, &code, &checked, &closed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SQL().QueryRow("SELECT value FROM nft_order_meta WHERE key='scanned:fixture'").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if status != orderbook.StatusOpen || code != int64(orderbook.CodeUnchecked) || checked != 0 || closed.Valid || cursor != 1 {
		t.Fatalf("startup retained stale book markers: %s/%d/%d/%v cursor=%d, index=1", status, code, checked, closed, cursor)
	}
	if storedRecoveryIntent(t, f.db.SQL(), f.hash) != f.intent {
		t.Fatal("startup changed original order intent")
	}
	if f.notices.Load() != 0 {
		t.Fatal("startup emitted a post-rewind notification")
	}
	if err := start(); err != nil {
		t.Fatal(err)
	}
	if err := start(); err != nil || f.starts.Load() != 1 {
		t.Fatalf("worker starts=%d: %v", f.starts.Load(), err)
	}
	if err := f.ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	if f.notices.Load() != 1 {
		t.Fatal("the normal rewind notification was not retained")
	}
}

func TestMarketStartupRefusalRollsBackAllParticipantsBeforeWorkers(t *testing.T) {
	for _, phase := range []string{"book-cursor", "later-participant", "commit", "cancel-in-transaction"} {
		t.Run(phase, func(t *testing.T) {
			f := newMarketStartupFixture(t, 1, false)
			if phase == "book-cursor" {
				cursorRecoveryRefusal(t, f.db.SQL())
			}
			if phase == "commit" {
				if _, err := f.db.SQL().Exec(`CREATE TABLE startup_guard_parent(id INTEGER PRIMARY KEY);
                    CREATE TABLE startup_guard_child(id INTEGER PRIMARY KEY,parent INTEGER REFERENCES startup_guard_parent(id) DEFERRABLE INITIALLY DEFERRED)`); err != nil {
					t.Fatal(err)
				}
			}
			before := indexer.RecoveryState(t, f.db)
			observed := false
			extra := nodecmd.Result{OnRewindTx: func(tx *sql.Tx, height uint64) error {
				var status string
				var cursor uint64
				if err := tx.QueryRow("SELECT status FROM nft_orders WHERE hash=?", f.hash.Hex()).Scan(&status); err != nil {
					return err
				}
				if err := tx.QueryRow("SELECT value FROM nft_order_meta WHERE key='scanned:fixture'").Scan(&cursor); err != nil {
					return err
				}
				if height != 1 || status != orderbook.StatusOpen || cursor != 1 {
					t.Fatalf("participant ran before book reconciliation: %d/%s/%d", height, status, cursor)
				}
				observed = true
				switch phase {
				case "later-participant":
					return errors.New("declared later startup participant refusal")
				case "commit":
					_, err := tx.Exec("INSERT INTO startup_guard_child VALUES(1,99)")
					return err
				case "cancel-in-transaction":
					f.cancel()
				}
				return nil
			}}
			start, err := f.prepare(extra)
			if err == nil || start != nil {
				t.Fatalf("refused startup exposed a worker start: %v", err)
			}
			if phase != "book-cursor" && !observed {
				t.Fatal("book writes were not observed inside the failed transaction")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, f.db), before)
			if f.starts.Load() != 0 || f.notices.Load() != 0 {
				t.Fatal("refused startup launched a worker or notice")
			}
			if storedRecoveryIntent(t, f.db.SQL(), f.hash) != f.intent {
				t.Fatal("refused startup changed original input")
			}
		})
	}
}

func TestMarketStartupUsesZeroForAnEmptyIndex(t *testing.T) {
	f := newMarketStartupFixture(t, 0, false)
	if _, err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err := f.db.SQL().QueryRow("SELECT value FROM nft_order_meta WHERE key='scanned:fixture'").Scan(&cursor); err != nil || cursor != 0 {
		t.Fatalf("empty index cursor=%d: %v", cursor, err)
	}
	if f.starts.Load() != 0 {
		t.Fatal("empty index startup launched its worker early")
	}
	if storedRecoveryIntent(t, f.db.SQL(), f.hash) != f.intent {
		t.Fatal("empty index startup changed input")
	}
}

func TestMarketStartupReopensClosuresAboveThePersistedIndex(t *testing.T) {
	for _, state := range []struct {
		status string
		code   uint8
	}{
		{orderbook.StatusFilled, orderbook.CodeFilled},
		{orderbook.StatusCancelled, orderbook.CodeCancelled},
		{orderbook.StatusExpired, orderbook.CodeExpired},
		{orderbook.StatusInvalid, orderbook.CodeBadSignature},
	} {
		t.Run(state.status, func(t *testing.T) {
			f := newMarketStartupFixture(t, 1, false)
			if _, err := f.db.SQL().Exec("UPDATE nft_orders SET status=?,code=? WHERE hash=?", state.status, state.code, f.hash.Hex()); err != nil {
				t.Fatal(err)
			}
			if _, err := f.prepare(); err != nil {
				t.Fatal(err)
			}
			var status string
			var code uint8
			var closed sql.NullInt64
			if err := f.db.SQL().QueryRow("SELECT status,code,closed_block FROM nft_orders WHERE hash=?", f.hash.Hex()).Scan(&status, &code, &closed); err != nil {
				t.Fatal(err)
			}
			if status != orderbook.StatusOpen || code != orderbook.CodeUnchecked || closed.Valid {
				t.Fatalf("orphan closure survived startup: %s/%d/%v", status, code, closed)
			}
			if storedRecoveryIntent(t, f.db.SQL(), f.hash) != f.intent {
				t.Fatal("startup changed the original closed-order input")
			}
		})
	}
}

func TestMarketStartupDoesNotInventCompletedLogScanning(t *testing.T) {
	f := newMarketStartupFixture(t, 1, false)
	if _, err := f.db.SQL().Exec("UPDATE nft_order_meta SET value=0 WHERE key='scanned:fixture'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err := f.db.SQL().QueryRow("SELECT value FROM nft_order_meta WHERE key='scanned:fixture'").Scan(&cursor); err != nil || cursor != 0 {
		t.Fatalf("startup advanced an unscanned cursor=%d: %v", cursor, err)
	}
}

func TestMarketStartupCancellationPreservesTheStartBarrier(t *testing.T) {
	for _, phase := range []string{"before-prepare", "before-start"} {
		t.Run(phase, func(t *testing.T) {
			f := newMarketStartupFixture(t, 1, false)
			var start func() error
			if phase == "before-start" {
				var err error
				start, err = f.prepare()
				if err != nil {
					t.Fatal(err)
				}
			}
			before := indexer.RecoveryState(t, f.db)
			f.cancel()
			if phase == "before-prepare" {
				var err error
				start, err = f.prepare()
				if !errors.Is(err, context.Canceled) || start != nil {
					t.Fatalf("canceled prepare=%v", err)
				}
			} else if err := start(); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled start=%v", err)
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, f.db), before)
			if f.starts.Load() != 0 {
				t.Fatal("canceled startup launched a worker")
			}
		})
	}
}

func TestMarketWorkerFirstReadsRPCAfterTheStartBarrier(t *testing.T) {
	f := newMarketStartupFixture(t, 1, true)
	defer f.cancel()
	select {
	case <-f.firstRead:
		t.Fatal("Market.Build launched an RPC worker before preparation")
	case <-time.After(50 * time.Millisecond):
	}
	start, err := f.prepare()
	if err != nil {
		t.Fatal(err)
	}
	if f.requests.Load() != 0 || f.starts.Load() != 0 {
		t.Fatal("preparation launched the worker")
	}
	if err := start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.firstRead:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted startup never launched the real book reader")
	}
	if f.starts.Load() != 1 {
		t.Fatalf("worker start count=%d", f.starts.Load())
	}
}

func TestMarketStartupRefusesAnInvalidIndexHeight(t *testing.T) {
	f := newMarketStartupFixture(t, 1, false)
	if _, err := f.db.SQL().Exec("UPDATE blocks SET number=-1 WHERE number=1"); err != nil {
		t.Fatal(err)
	}
	before := indexer.RecoveryState(t, f.db)
	start, err := f.prepare()
	if err == nil || start != nil {
		t.Fatalf("invalid height accepted: %v", err)
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, f.db), before)
	if f.starts.Load() != 0 {
		t.Fatal("invalid height launched a worker")
	}
}

func TestExtensionStartupRequiresAnInitializedIndex(t *testing.T) {
	starts := 0
	start, err := nodecmd.PrepareExtensions(context.Background(), nil, []nodecmd.Result{{Start: func(context.Context) { starts++ }}})
	if err == nil || start != nil || starts != 0 {
		t.Fatalf("missing index bypassed preparation: %v", err)
	}
}
