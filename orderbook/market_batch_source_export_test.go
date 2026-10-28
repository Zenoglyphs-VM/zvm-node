package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// This fixture supplies source authority only. The unmodified relayer chooses,
// signs and publishes both actual account blocks and the predecessor chain.
func (p *ExecutedMarketProviderFixture) sequencedSourceRPC(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		http.Error(w, "bounded sequenced provider source request required", 400)
		return
	}
	var call struct {
		ID     json.RawMessage
		Method string
		Params []json.RawMessage
	}
	if json.Unmarshal(raw, &call) != nil {
		http.Error(w, "invalid sequenced provider source request", 400)
		return
	}
	if call.Method == "ledger.getFrontierMomentum" || call.Method == "ledger.getDetailedMomentumsByHeight" {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		p.f.r.nomReply(w, r)
		return
	}
	p.f.r.nomMu.RLock()
	limit, branch := p.f.r.limit, p.f.r.branch
	source := p.f.r.scenario.branches[branch]
	p.f.r.nomMu.RUnlock()
	var result any
	var failure error
	p.mu.Lock()
	copyBlock := func(index int) *znn.AccountBlock {
		leg := &p.providerLegs()[index]
		if leg.published == nil {
			return nil
		}
		copy := *leg.published
		height := p.FundingHeight() + 1 + uint64(index)
		if leg.bound && branch == 0 && limit >= height {
			copy.ConfirmationDetail = &znn.ConfirmationDetail{NumConfirmations: limit - height + 1,
				MomentumHeight: height, MomentumHash: source[height].Momentum.Hash, MomentumTimestamp: source[height].Momentum.Timestamp}
		}
		return &copy
	}
	switch call.Method {
	case "embedded.plasma.get":
		result = &znn.PlasmaInfo{CurrentPlasma: 10_500_000, MaxPlasma: 10_500_000, QsrAmount: znn.BigIntFromUint64(500_000_000_000)}
	case "ledger.getFrontierAccountBlock":
		for i := range p.providerLegs() {
			if block := copyBlock(i); block != nil {
				result = block
			}
		}
	case "ledger.getAccountBlockByHash":
		var hash znn.Hash
		if len(call.Params) != 1 || json.Unmarshal(call.Params[0], &hash) != nil {
			failure = errors.New("exact account hash required")
			break
		}
		for i := range p.providerLegs() {
			if block := copyBlock(i); block != nil && block.Hash == hash {
				result = block
			}
		}
	case "ledger.getAccountBlocksByHeight", "ledger.getUnconfirmedBlocksByAddress":
		list := []*znn.AccountBlock{}
		start, count := uint64(1), uint64(2)
		if call.Method == "ledger.getAccountBlocksByHeight" && (len(call.Params) != 3 || json.Unmarshal(call.Params[1], &start) != nil || json.Unmarshal(call.Params[2], &count) != nil || count > 64) {
			failure = errors.New("bounded account-block height range required")
			break
		}
		for i := range p.providerLegs() {
			if block := copyBlock(i); block != nil && block.Height >= start && block.Height-start < count && (call.Method != "ledger.getUnconfirmedBlocksByAddress" || block.ConfirmationDetail == nil) {
				list = append(list, block)
			}
		}
		result = &znn.BlockList{Count: uint64(len(list)), List: list}
	case "ledger.getMomentumsByHeight":
		var start, count uint64
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &start) != nil || json.Unmarshal(call.Params[1], &count) != nil || count > 64 {
			failure = errors.New("undeclared sequenced provider source range")
			break
		}
		list := []*znn.Momentum{}
		for h := start; h < start+count && h <= limit; h++ {
			if dm := source[h]; dm != nil {
				list = append(list, dm.Momentum)
			}
		}
		result = map[string]any{"list": list}
	case "ledger.publishRawTransaction":
		index := p.publications
		var block znn.AccountBlock
		if index < 0 || index >= len(p.providerLegs()) || len(call.Params) != 1 || json.Unmarshal(call.Params[0], &block) != nil {
			failure = errors.New("unexpected sequenced provider publication")
			break
		}
		if err := block.Verify(); err != nil {
			failure = err
			break
		}
		leg := &p.providerLegs()[index]
		var previous znn.Hash
		if index == 1 {
			if p.providerLegs()[0].published == nil || !p.providerLegs()[0].bound {
				failure = errors.New("the first account block is not confirmed")
				break
			}
			previous = p.providerLegs()[0].published.Hash
		}
		ack := p.FundingHeight() + uint64(index)
		if branch != 0 || limit != ack || block.Address != devnet.RelayerKey(0).Address || block.ToAddress != p.f.Rollup().Inbox ||
			block.ChainIdentifier != p.f.Rollup().L1ChainID || block.BlockType != znn.BlockTypeUserSend ||
			block.Height != uint64(index+1) || block.PreviousHash != previous || block.AmountBig().Sign() != 0 ||
			block.TokenStandard != znn.ZeroTokenStandard || block.MomentumAcknowledged.Height != ack ||
			block.MomentumAcknowledged.Hash != source[ack].Momentum.Hash || len(block.Data) < 2 ||
			block.Data[0] != derive.MsgBatch || leg.published != nil || len(leg.raw) == 0 || p.submissions != index+1 {
			failure = errors.New("relayer changed the sequenced provider source ordering or account chain")
			break
		}
		txs, err := derive.DecodeBatch(block.Data[1:], p.f.Rollup().MaxBatchBytes, true)
		if err != nil || len(txs) != 1 || !bytes.Equal(txs[0], leg.raw) {
			failure = errors.New("relayer changed the exact sequenced provider provider bytes")
			break
		}
		leg.published = &block
		p.publications++
	default:
		failure = errors.New("undeclared sequenced provider source operation: " + call.Method)
	}
	if failure != nil {
		p.unexpected++
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	envelope := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
	if failure != nil {
		delete(envelope, "result")
		envelope["error"] = map[string]any{"code": -32602, "message": failure.Error()}
	}
	json.NewEncoder(w).Encode(envelope)
}

func (p *ExecutedMarketProviderFixture) bindProviderLeg(t *testing.T, ctx context.Context, index int) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var block *znn.AccountBlock
	for block == nil {
		p.mu.Lock()
		leg := &p.providerLegs()[index]
		if leg.published != nil && leg.missing > 0 && !leg.bound && p.unexpected == 0 {
			copy := *leg.published
			block = &copy
		}
		p.mu.Unlock()
		if block != nil {
			break
		}
		select {
		case <-deadline.Done():
			t.Fatal("the actual sequenced provider publication and pending application receipt were not observed")
		case <-ticker.C:
		}
	}
	prefix := p.FundingHeight() + uint64(index)
	actor, nonce := p.f.Seller(), p.BatchSellerNonce()
	if index == 1 {
		actor, nonce = p.f.Buyer(), p.ActorNonce()
	}
	if p.tokenApproval != nil {
		actor, nonce = p.f.Buyer(), p.ActorNonce()+uint64(index)
	}
	if p.f.r.d.Head().Height != prefix {
		t.Fatal("sequenced provider source prefix is not joined")
	}
	if got, err := p.f.r.eth.NonceAt(ctx, actor, nil); err != nil || got != nonce {
		t.Fatal("pending sequenced provider leg consumed a nonce")
	}
	p.f.r.nomMu.Lock()
	defer p.f.r.nomMu.Unlock()
	if p.f.r.limit != prefix || p.f.r.branch != 0 {
		t.Fatal("sequenced provider binding crossed its exact source horizon")
	}
	var future [2]map[uint64]*znn.DetailedMomentum
	for branch, old := range p.f.r.scenario.branches {
		future[branch] = make(map[uint64]*znn.DetailedMomentum, len(old))
		for height, dm := range old {
			raw, err := json.Marshal(dm)
			if err != nil {
				t.Fatal(err)
			}
			var copy znn.DetailedMomentum
			if err := json.Unmarshal(raw, &copy); err != nil {
				t.Fatal(err)
			}
			if height <= prefix {
				after, err := json.Marshal(&copy)
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("sequenced provider binding changed an already-derived source byte")
				}
			}
			future[branch][height] = &copy
		}
	}
	dm := future[0][prefix+1]
	dm.Blocks = append(dm.Blocks, block)
	sort.Slice(dm.Blocks, func(i, j int) bool {
		a, b := dm.Blocks[i], dm.Blocks[j]
		if c := bytes.Compare(a.Address[:], b.Address[:]); c != 0 {
			return c < 0
		}
		return a.Height < b.Height
	})
	dm.Momentum.Content = nil
	for _, block := range dm.Blocks {
		dm.Momentum.Content = append(dm.Momentum.Content, znn.AccountHeader{Address: block.Address, Height: block.Height, Hash: block.Hash})
	}
	p.f.r.scenario.branches = future
	p.mu.Lock()
	p.providerLegs()[index].bound = true
	p.bound = p.providerLegs()[0].bound && p.providerLegs()[1].bound
	p.mu.Unlock()
}
