package node

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// These are declared engine header observations over a real loopback RPC
// client. They do not execute op-reth/op-geth or authenticate a live chain.
type n04Headers struct {
	head    uint64
	headers map[uint64]*types.Header
}

func (h *n04Headers) GetBlockByNumber(_ context.Context, number rpc.BlockNumber, _ bool) (*types.Header, error) {
	if number == rpc.LatestBlockNumber {
		return h.headers[h.head], nil
	}
	return h.headers[uint64(number)], nil
}

func n04Header(number uint64, variant byte) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number), Time: 1800000000 + number,
		Difficulty: new(big.Int), GasLimit: 30000000, Extra: []byte{variant}}
}

func n04DeriverFixture(t *testing.T, phase string) (*Deriver, string, uint64, *n04Headers) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	headers := map[uint64]*types.Header{}
	canonical := map[uint64]znn.Hash{1: {0: 1}}
	for n := uint64(0); n <= 5; n++ {
		headers[n] = n04Header(n, 0)
		if n < 1 || n > 4 {
			continue
		}
		height := n + 1
		m := &znn.Momentum{Height: height, Hash: znn.Hash{0: byte(height)}, Timestamp: 1800000000 + height}
		canonical[height] = m.Hash
		p := &derive.Plan{Momentum: m, NewChunks: []*derive.Chunk{{Hash: znn.Hash{1: byte(n)}, Author: znn.Address{19: 8}, Height: height, Data: []byte{byte(n)}}}}
		if err := db.RecordDerived(&store.Derived{Momentum: m, L2Number: n, L2Hash: headers[n].Hash(), Attempts: 1, Plan: p}, 0); err != nil {
			t.Fatal(err)
		}
	}
	observed := &n04Headers{head: 4, headers: headers}
	want := uint64(4)
	switch phase {
	case "engine-behind", "notification-interrupted":
		observed.head, want = 2, 2
	case "engine-ahead":
		observed.head = 5
	case "same-height-changed":
		headers[4], want = n04Header(4, 1), 3
	case "engine-genesis-only":
		observed.head, want = 0, 0
	case "no-common-block":
		for n := uint64(1); n <= 4; n++ {
			headers[n] = n04Header(n, 1)
		}
		want = 0
	default:
		t.Fatal("unknown declared engine observation")
	}
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("eth", observed); err != nil {
		t.Fatal(err)
	}
	if err := rpcServer.RegisterName("engine", validEngine{}); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(rpcServer)
	t.Cleanup(web.Close)
	eth, err := ethclient.Dial(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eth.Close)
	engine, err := l2.DialEngine(context.Background(), web.URL, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	d := &Deriver{Cfg: &rollup.Config{L1StartHeight: 2, L2GenesisHash: headers[0].Hash()},
		DB: db, Eth: eth, Engine: engine, L1: nomServing(t, canonical),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return d, path, want, observed
}

func n04AssertRecoveredHead(t *testing.T, d *Deriver, want uint64) {
	t.Helper()
	head := d.Head()
	if head.L2Number != want || head.Height != want+1 {
		t.Fatalf("recovered momentum/L2 head = %d/%d, want %d/%d", head.Height, head.L2Number, want+1, want)
	}
	if want == 0 {
		stored, err := d.DB.Head()
		if err != nil || stored != nil || head.L2Hash != d.Cfg.L2GenesisHash {
			t.Fatal("genesis recovery retained a derived orphan head")
		}
	} else {
		stored, err := d.DB.Head()
		if err != nil || stored == nil || *stored != head {
			t.Fatal("persistent and serving heads disagree")
		}
	}
	var chunks int
	if err := d.DB.SQL().QueryRow("SELECT count(*) FROM chunks").Scan(&chunks); err != nil || chunks != int(want) {
		t.Fatalf("restored chunk range differs from common head: %d, %v", chunks, err)
	}
}

func TestRestartConvergesToDeclaredCommonEngineBoundary(t *testing.T) {
	for _, phase := range []string{"engine-behind", "engine-ahead", "same-height-changed", "engine-genesis-only", "no-common-block"} {
		t.Run(phase, func(t *testing.T) {
			d, path, want, _ := n04DeriverFixture(t, phase)
			calls := 0
			d.OnRewind = func(number uint64) {
				calls++
				if number != want {
					t.Error("index notification names a different common boundary")
				}
				n04AssertRecoveredHead(t, d, want)
			}
			if err := d.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			n04AssertRecoveredHead(t, d, want)
			if calls != map[bool]int{true: 1, false: 0}[want < 4] {
				t.Fatalf("unexpected rewind notification count: %d", calls)
			}
			if err := d.DB.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			d.DB = db
			if err := d.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			n04AssertRecoveredHead(t, d, want)
		})
	}
}

func TestNotificationInterruptionKeepsCommittedRecoveryBoundary(t *testing.T) {
	d, path, want, _ := n04DeriverFixture(t, "notification-interrupted")
	stop := &struct{}{}
	d.OnRewind = func(number uint64) {
		if number != want {
			t.Fatal("notification preceded common-boundary selection")
		}
		n04AssertRecoveredHead(t, d, want)
		panic(stop) // Declared caller interruption after SQL/head commit.
	}
	func() {
		defer func() {
			if recover() != stop {
				t.Fatal("declared post-commit notification interruption not observed")
			}
		}()
		if err := d.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
	}()
	if err := d.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	d.DB, d.OnRewind = db, nil
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	n04AssertRecoveredHead(t, d, want)
}
