package indexer_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/nodecmd"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func canonicalFixture(t *testing.T, chain *indexer.RecoveryChain) (*indexer.Indexer, *store.DB) {
	t.Helper()
	base, db := indexer.NewRecoveryIndex(t, chain)
	genesis, err := base.Eth().HeaderByNumber(context.Background(), big.NewInt(0))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := indexer.New(db, base.Eth(), &rollup.Config{L1StartHeight: 2, L2ChainID: 1337, L2GenesisHash: genesis.Hash()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return ix, db
}

func canonicalSourceHead(t *testing.T, ix *indexer.Indexer, db *store.DB, number uint64) store.Head {
	t.Helper()
	header, err := ix.Eth().HeaderByNumber(context.Background(), new(big.Int).SetUint64(number))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RewindTo(1); err != nil {
		t.Fatal(err)
	}
	m := &znn.Momentum{Height: number + 1, Hash: znn.Hash{0: byte(number)}, Timestamp: header.Time}
	if number > 0 {
		if err := db.RecordDerived(&store.Derived{Momentum: m, L2Number: number, L2Hash: header.Hash(), Plan: &derive.Plan{Momentum: m}}, 0); err != nil {
			t.Fatal(err)
		}
	}
	return store.Head{Height: m.Height, Hash: m.Hash, L2Number: number, L2Hash: header.Hash()}
}

func canonicalChain() *indexer.RecoveryChain {
	chain := indexer.NewRecoveryChain()
	for number := uint64(0); number <= 3; number++ {
		chain.Put(number, 0)
	}
	return chain
}

func TestCanonicalRecoveryConvergesToAcceptedLocalHead(t *testing.T) {
	for _, phase := range []string{"engine-behind", "same-height-replacement", "broken-stored-link", "genesis"} {
		t.Run(phase, func(t *testing.T) {
			chain := canonicalChain()
			ix, db := canonicalFixture(t, chain)
			for n := uint64(1); n <= 3; n++ {
				if err := ix.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			want := uint64(1)
			switch phase {
			case "same-height-replacement":
				chain.Put(2, 1)
				chain.Put(3, 1)
				want = 3
			case "broken-stored-link":
				if _, err := db.SQL().Exec("UPDATE blocks SET parent='declared broken parent' WHERE number=2"); err != nil {
					t.Fatal(err)
				}
				want = 3
			case "genesis":
				want = 0
			}
			head := canonicalSourceHead(t, ix, db, want)
			if err := ix.ReconcileCanonical(context.Background(), head); err != nil {
				t.Fatal(err)
			}
			number, err := ix.Head()
			if err != nil {
				t.Fatal(err)
			}
			for n := number + 1; n <= want; n++ {
				if err := ix.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			reference, referenceDB := canonicalFixture(t, chain)
			canonicalSourceHead(t, reference, referenceDB, want)
			for n := uint64(1); n <= want; n++ {
				if err := reference.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
			if err := ix.ReconcileCanonical(context.Background(), head); err != nil {
				t.Fatal(err)
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

func TestCanonicalRecoveryRefusalPreservesEveryTable(t *testing.T) {
	for _, phase := range []string{"zero-hash", "unbounded-height", "cancelled", "source-changed", "engine-changed", "participant", "commit"} {
		t.Run(phase, func(t *testing.T) {
			chain := canonicalChain()
			ix, db := canonicalFixture(t, chain)
			for n := uint64(1); n <= 3; n++ {
				if err := ix.IndexBlock(context.Background(), n); err != nil {
					t.Fatal(err)
				}
			}
			head := canonicalSourceHead(t, ix, db, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch phase {
			case "zero-hash":
				head.L2Hash = common.Hash{}
			case "unbounded-height":
				head.L2Number = 1 << 63
			case "cancelled":
				cancel()
			case "source-changed":
				canonicalSourceHead(t, ix, db, 3)
			case "engine-changed":
				chain.Put(1, 1)
			case "participant":
				ix.BeforeRewindCommit(func(*sql.Tx, uint64) error { return errors.New("declared canonical participant refusal") })
			case "commit":
				for _, statement := range []string{`CREATE TABLE canonical_parent(id INTEGER PRIMARY KEY)`, `CREATE TABLE canonical_child(id INTEGER REFERENCES canonical_parent(id) DEFERRABLE INITIALLY DEFERRED)`} {
					if _, err := db.SQL().Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
				ix.BeforeRewindCommit(func(tx *sql.Tx, _ uint64) error {
					_, err := tx.Exec("INSERT INTO canonical_child VALUES(1)")
					return err
				})
			}
			before := indexer.RecoveryState(t, db)
			if err := ix.ReconcileCanonical(ctx, head); err == nil {
				t.Fatal("declared canonical recovery refusal was not observed")
			}
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
		})
	}
}

func TestIndexBlockRetryIsIdempotentAndReplacementNeedsRecovery(t *testing.T) {
	chain := canonicalChain()
	ix, db := canonicalFixture(t, chain)
	if err := ix.IndexBlock(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	before := indexer.RecoveryState(t, db)
	if err := ix.IndexBlock(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
	chain.Put(1, 1)
	if err := ix.IndexBlock(context.Background(), 1); err == nil {
		t.Fatal("occupied block was replaced without canonical recovery")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
}

func TestCanonicalWorkerBarrierCanRetryWithoutStartingEarly(t *testing.T) {
	chain := canonicalChain()
	ix, db := canonicalFixture(t, chain)
	for n := uint64(1); n <= 3; n++ {
		if err := ix.IndexBlock(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	head := canonicalSourceHead(t, ix, db, 1)
	var armed bool
	var starts atomic.Uint64
	result := nodecmd.Result{Start: func(context.Context) { starts.Add(1) }, OnRewindTx: func(*sql.Tx, uint64) error {
		if armed {
			return errors.New("declared canonical start refusal")
		}
		return nil
	}}
	start, err := nodecmd.PrepareExtensions(context.Background(), ix, []nodecmd.Result{result}, func() store.Head { return head })
	if err != nil || start == nil {
		t.Fatal("canonical start barrier was not prepared", err)
	}
	armed = true
	before := indexer.RecoveryState(t, db)
	if err := start(); err == nil || starts.Load() != 0 {
		t.Fatal("canonical refusal launched a worker")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
	armed = false
	var wg sync.WaitGroup
	startErrors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); startErrors <- start() }()
	}
	wg.Wait()
	close(startErrors)
	for err := range startErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if starts.Load() != 1 {
		t.Fatal("accepted canonical barrier did not launch exactly once")
	}
	if number, err := ix.Head(); err != nil || number != 1 {
		t.Fatal("worker started above the accepted head")
	}
}

type countingCanonicalPort struct {
	client  *rpc.Client
	headers atomic.Uint64
}

func (p *countingCanonicalPort) GetBlockByNumber(ctx context.Context, number rpc.BlockNumber, full bool) (json.RawMessage, error) {
	if !full {
		p.headers.Add(1)
	}
	arg := number.String()
	if number >= 0 {
		arg = hexutil.EncodeUint64(uint64(number))
	}
	var out json.RawMessage
	err := p.client.CallContext(ctx, &out, "eth_getBlockByNumber", arg, full)
	return out, err
}

func (p *countingCanonicalPort) GetBlockReceipts(ctx context.Context, number rpc.BlockNumberOrHash) (json.RawMessage, error) {
	var out json.RawMessage
	err := p.client.CallContext(ctx, &out, "eth_getBlockReceipts", number)
	return out, err
}

func TestCanonicalFollowerRepairsQuietReplacementWithoutIdleRPCGrowth(t *testing.T) {
	chain := canonicalChain()
	base, db := canonicalFixture(t, chain)
	port := &countingCanonicalPort{client: base.Eth().Client()}
	server := rpc.NewServer()
	if err := server.RegisterName("eth", port); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(server)
	t.Cleanup(web.Close)
	client, err := ethclient.Dial(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	genesis, err := base.Eth().HeaderByNumber(context.Background(), big.NewInt(0))
	if err != nil {
		t.Fatal(err)
	}
	ix, err := indexer.New(db, client, &rollup.Config{L1StartHeight: 2, L2ChainID: 1337, L2GenesisHash: genesis.Hash()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Value
	accepted.Store(canonicalSourceHead(t, ix, db, 2))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ix.FollowCanonical(ctx, func() store.Head { return accepted.Load().(store.Head) })
	}()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(want common.Hash) {
		t.Helper()
		for {
			var got string
			if err := db.SQL().QueryRow("SELECT hash FROM blocks WHERE number=2").Scan(&got); err == nil && got == want.Hex() {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("canonical follower did not converge")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(accepted.Load().(store.Head).L2Hash)
	requests := port.headers.Load()
	time.Sleep(750 * time.Millisecond)
	if port.headers.Load() != requests {
		t.Fatal("unchanged accepted head repeated canonical RPC reads")
	}
	chain.Put(2, 1)
	accepted.Store(canonicalSourceHead(t, ix, db, 2))
	wait(accepted.Load().(store.Head).L2Hash)
	cancel()
	<-done
	reference, referenceDB := canonicalFixture(t, chain)
	cleanHead := canonicalSourceHead(t, reference, referenceDB, 2)
	cleanCtx, stopClean := context.WithTimeout(context.Background(), 4*time.Second)
	cleanDone := make(chan struct{})
	go func() {
		defer close(cleanDone)
		reference.FollowCanonical(cleanCtx, func() store.Head { return cleanHead })
	}()
	t.Cleanup(func() { stopClean(); <-cleanDone })
	for {
		var got string
		if err := referenceDB.SQL().QueryRow("SELECT hash FROM blocks WHERE number=2").Scan(&got); err == nil && got == cleanHead.L2Hash.Hex() {
			break
		}
		select {
		case <-cleanCtx.Done():
			t.Fatal("clean canonical follower did not converge")
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopClean()
	<-cleanDone
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
}
