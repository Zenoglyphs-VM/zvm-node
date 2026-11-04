package main

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// These are declared synthetic NoM inputs, not authenticated chain evidence.
type nomFixture struct {
	cfg           *rollup.Config
	momentums     []*znn.DetailedMomentum
	plans         []*derive.Plan
	mints         []*big.Int
	frontier      atomic.Uint64
	frontierReads atomic.Uint64
	requests      atomic.Uint64
	unknown       atomic.Uint64
	server        *http.Server
	done          chan error
	url           string
}

func startNoM(ctx context.Context, cfg *rollup.Config, name string) *nomFixture {
	f := &nomFixture{cfg: cfg, done: make(chan error, 1)}
	f.momentums = append(f.momentums, &znn.DetailedMomentum{Momentum: &znn.Momentum{Version: 1, ChainIdentifier: cfg.L1ChainID,
		Height: cfg.L1StartHeight - 1, Timestamp: cfg.L2GenesisTimestamp, Hash: znn.Sha3([]byte("offline-node/" + name + "/0"))}})
	f.plans = append(f.plans, nil)
	f.mints = append(f.mints, nil)
	var previousAccount znn.Hash
	for number := uint64(1); number <= 8; number++ {
		m := &znn.Momentum{Version: 1, ChainIdentifier: cfg.L1ChainID, Height: cfg.L1StartHeight + number - 1,
			Timestamp: cfg.L2GenesisTimestamp + number, Hash: znn.Sha3([]byte(fmtNumber(name, number))), PreviousHash: f.momentums[number-1].Momentum.Hash}
		amount := number * 100_000_000
		b := &znn.AccountBlock{Version: 1, ChainIdentifier: cfg.L1ChainID, BlockType: znn.BlockTypeUserSend, Height: number,
			Address: znn.Address(depositor), PreviousHash: previousAccount, ToAddress: cfg.Vault, TokenStandard: znn.ZNN,
			Amount: znn.BigIntFromUint64(amount), BasePlasma: znn.BaseSendPlasma(0), UsedPlasma: znn.BaseSendPlasma(0)}
		b.FusedPlasma = b.BasePlasma
		b.Hash = b.ComputeHash()
		previousAccount = b.Hash
		m.Content = []znn.AccountHeader{{Address: b.Address, Hash: b.Hash, Height: b.Height}}
		dm := &znn.DetailedMomentum{Momentum: m, Blocks: []*znn.AccountBlock{b}}
		p, err := derive.BuildPlan(cfg, dm, nil)
		must(err)
		check(len(p.Txs) == 2 && p.Txs[0].Lane == derive.LaneSystem && p.Txs[1].Lane == derive.LaneDeposit, "synthetic source-planned lanes differ")
		mint := new(big.Int).Mul(new(big.Int).SetUint64(amount), rollup.ZNNScale)
		check(p.Txs[1].Tx.Mint().Cmp(mint) == 0 && p.Txs[1].Tx.From() == depositor, "synthetic source-planned mint differs")
		f.momentums = append(f.momentums, dm)
		f.plans = append(f.plans, p)
		f.mints = append(f.mints, mint)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	f.url = "http://" + listener.Addr().String()
	f.server = &http.Server{Handler: http.HandlerFunc(f.handle), ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	go func() { f.done <- f.server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = f.server.Close() }()
	return f
}
func (f *nomFixture) handle(w http.ResponseWriter, r *http.Request) {
	check(f.requests.Add(1) <= 1024, "synthetic NoM request limit")
	var request struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request) != nil {
		http.Error(w, "invalid synthetic request", 400)
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	frontier := f.frontier.Load()
	check(frontier <= 8, "synthetic frontier outside cohort")
	switch request.Method {
	case "ledger.getFrontierMomentum":
		f.frontierReads.Add(1)
		response["result"] = f.momentums[frontier].Momentum
	case "ledger.getDetailedMomentumsByHeight":
		var height, count uint64
		check(len(request.Params) == 2 && json.Unmarshal(request.Params[0], &height) == nil && json.Unmarshal(request.Params[1], &count) == nil, "synthetic range arguments differ")
		check(count > 0 && count <= 64, "synthetic range count outside bound")
		list := []*znn.DetailedMomentum{}
		for _, dm := range f.momentums[:frontier+1] {
			if dm.Momentum.Height >= height && uint64(len(list)) < count {
				list = append(list, dm)
			}
		}
		response["result"] = map[string]any{"count": len(list), "list": list}
	default:
		f.unknown.Add(1)
		response["error"] = map[string]any{"code": -32601, "message": "method outside declared synthetic NoM scope"}
	}
	w.Header().Set("Content-Type", "application/json")
	must(json.NewEncoder(w).Encode(response))
}
func (f *nomFixture) stop() {
	must(f.server.Close())
	err := <-f.done
	check(err == http.ErrServerClosed, "synthetic NoM server did not join")
	check(f.unknown.Load() == 0, "unexpected NoM method")
}
