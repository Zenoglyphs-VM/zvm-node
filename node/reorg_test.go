package node

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

type validEngine struct{}

func (validEngine) ForkchoiceUpdatedV3(engine.ForkchoiceStateV1, *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	return engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: engine.VALID}}, nil
}

// nomServing answers ledger.getDetailedMomentumsByHeight from `canonical`,
// the momentum hashes NoM holds now, and ledger.getFrontierMomentum with the
// highest of them.
func nomServing(t *testing.T, canonical map[uint64]znn.Hash) *znn.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "ledger.getFrontierMomentum" {
			top := uint64(0)
			for h := range canonical {
				top = max(top, h)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": &znn.Momentum{Hash: canonical[top], Height: top},
			})
			return
		}
		var height uint64
		json.Unmarshal(req.Params[0], &height)
		list := []*znn.DetailedMomentum{}
		if h, ok := canonical[height]; ok {
			list = append(list, &znn.DetailedMomentum{Momentum: &znn.Momentum{Hash: h, Height: height}})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"count": len(list), "list": list},
		})
	}))
	t.Cleanup(srv.Close)
	return znn.NewClient(srv.URL)
}

// When NoM replaces a momentum the node has derived, the node's head has to be
// back on the fork point before the index is told. Told first, the index
// rewound while the node still reported the orphan as its head, and read the
// orphan straight back from op-reth — which keeps serving it by number after
// a forkchoice to its parent — and kept it for good (block 23,790, chain 69).
func TestReorgMovesTheHeadBeforeTellingTheIndex(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "zvm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Momentums 2-5 derived as L2 blocks 1-4. NoM has since replaced 5.
	canonical := map[uint64]znn.Hash{}
	for h := uint64(2); h <= 5; h++ {
		mh := znn.Hash{byte(h)}
		canonical[h] = mh
		if _, err := db.SQL().Exec(`INSERT INTO momentums
			(height, hash, timestamp, l2_number, l2_hash, txs, messages, dropped, nom_blocks, zvm_bytes, zvm_plasma, gas_used, build_ms, attempts)
			VALUES (?,?,0,?,?,1,0,0,0,0,0,0,0,1)`, h, mh.String(), h-1, common.Hash{0xb, byte(h)}.Hex()); err != nil {
			t.Fatal(err)
		}
	}
	canonical[5] = znn.Hash{0xee}

	rpcSrv := rpc.NewServer()
	if err := rpcSrv.RegisterName("engine", validEngine{}); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(rpcSrv)
	t.Cleanup(web.Close)
	eng, err := l2.DialEngine(context.Background(), web.URL, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}

	d := &Deriver{
		Cfg: &rollup.Config{L1StartHeight: 2}, L1: nomServing(t, canonical), Engine: eng, DB: db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	orphan, err := db.MomentumHead(5)
	if err != nil {
		t.Fatal(err)
	}
	d.setHead(*orphan)

	var seen *store.Head
	told := uint64(0)
	d.OnRewind = func(l2Number uint64) {
		h := d.Head()
		seen, told = &h, l2Number
	}
	if err := d.reorg(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen == nil {
		t.Fatal("the index was never told")
	}
	if told != 3 {
		t.Errorf("the index was told to rewind to L2 block %d, want 3", told)
	}
	if seen.Height != 4 || seen.L2Number != 3 {
		t.Errorf("while the index was told, the node's head was momentum %d / L2 block %d; want 4 / 3, the fork point",
			seen.Height, seen.L2Number)
	}
}

// NoM replaces the momentum this node's head came from, and nothing newer
// arrives. The node must notice from the frontier alone: waiting for a later
// momentum to name another parent left block 23,790 on chain 69 an orphan
// served as the head for 8 minutes 40 seconds.
func TestAReplacedHeadIsNoticedWithoutANewerMomentum(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "zvm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	canonical := map[uint64]znn.Hash{}
	for h := uint64(2); h <= 5; h++ {
		mh := znn.Hash{byte(h)}
		canonical[h] = mh
		if _, err := db.SQL().Exec(`INSERT INTO momentums
			(height, hash, timestamp, l2_number, l2_hash, txs, messages, dropped, nom_blocks, zvm_bytes, zvm_plasma, gas_used, build_ms, attempts)
			VALUES (?,?,0,?,?,1,0,0,0,0,0,0,0,1)`, h, mh.String(), h-1, common.Hash{0xb, byte(h)}.Hex()); err != nil {
			t.Fatal(err)
		}
	}
	canonical[5] = znn.Hash{0xee} // the replacement, at the head's own height

	rpcSrv := rpc.NewServer()
	if err := rpcSrv.RegisterName("engine", validEngine{}); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(rpcSrv)
	t.Cleanup(web.Close)
	eng, err := l2.DialEngine(context.Background(), web.URL, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	d := &Deriver{
		Cfg: &rollup.Config{L1StartHeight: 2}, L1: nomServing(t, canonical), Engine: eng, DB: db,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	orphan, err := db.MomentumHead(5)
	if err != nil {
		t.Fatal(err)
	}
	d.setHead(*orphan)
	rewoundTo := int64(-1)
	d.OnRewind = func(l2Number uint64) { rewoundTo = int64(l2Number) }

	if _, err := d.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := d.Head(); h.Height != 4 || h.L2Number != 3 {
		t.Errorf("after NoM replaced the head momentum, the head is momentum %d / L2 block %d; want 4 / 3", h.Height, h.L2Number)
	}
	if rewoundTo != 3 {
		t.Errorf("the index was told to rewind to %d, want 3", rewoundTo)
	}

	// Level with an unchanged frontier is an idle chain, not a reorg.
	canonical[5] = orphan.Hash
	d.setHead(*orphan)
	rewoundTo = -1
	if _, err := d.step(context.Background()); err != nil || d.Head().Height != 5 || rewoundTo != -1 {
		t.Errorf("an idle chain was rewound: head %d, told %d, err %v", d.Head().Height, rewoundTo, err)
	}
}
