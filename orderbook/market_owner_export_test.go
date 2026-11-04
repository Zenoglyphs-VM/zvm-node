package orderbook

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// These independent reads execute contract views at the selected canonical
// hash. Neither the worker readiness predicate nor cached book replies can
// determine the approval, order nonce, cancellation bit or remaining amount.
func (p *ExecutedMarketProviderFixture) AssertOwnerAction(t *testing.T, included bool) map[string]any {
	t.Helper()
	if p.Action() == "buy" {
		t.Fatal("owner state assertions require an owner action")
	}
	header := p.f.HeadHash()
	read := func(artifact, method string, to common.Address, args ...any) []byte {
		data, err := p.f.r.scenario.arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := p.f.r.eth.CallContractAtHash(context.Background(), ethereum.CallMsg{To: &to, Data: data}, header)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	nonce := read("ZVMExchange", "nonces", p.f.Exchange(), p.f.Seller())
	wantNonce := int64(0)
	if included && p.Action() == "nonce" {
		wantNonce = 1
	}
	if len(nonce) != 32 || new(big.Int).SetBytes(nonce).Cmp(big.NewInt(wantNonce)) != 0 {
		t.Fatal("canonical exchange order nonce differs")
	}
	approval := read("ZVMCollection", "isApprovedForAll", p.f.Collection(), p.f.Seller(), p.f.Exchange())
	wantApproval := int64(1)
	if included && p.Action() == "approval" {
		wantApproval = 0
	}
	if len(approval) != 32 || new(big.Int).SetBytes(approval).Cmp(big.NewInt(wantApproval)) != 0 {
		t.Fatal("canonical collection approval differs")
	}
	epoch := p.f.Epoch(t)
	states := make([]map[string]any, 0, 2)
	for i, input := range p.f.r.scenario.inputs {
		if p.f.ExecutedOwner(t, int64(i+1)) != p.f.Seller() {
			t.Fatal("an owner invalidation moved a token")
		}
		wantStatus, wantCode, wantFilled := expectedExecutedMarketState(p.f.r.scenario, p.f.r.branch, p.f.r.d.Head().Height, i)
		checked := read("ZVMExchange", "checkOrder", p.f.Exchange(), input.ABI())
		remaining := int64(1)
		if wantCode == CodeCancelled || wantCode == CodeStaleNonce {
			remaining = 0
		}
		if len(checked) != 64 || new(big.Int).SetBytes(checked[:32]).Uint64() != uint64(wantCode) ||
			new(big.Int).SetBytes(checked[32:]).Cmp(big.NewInt(remaining)) != 0 {
			t.Fatal("direct canonical checkOrder differs from the exact invalidation")
		}
		cancelled := read("ZVMExchange", "cancelled", p.f.Exchange(), input.Order.Hash())
		wantCancelled := int64(0)
		if included && p.Action() == "cancel" && i == 0 {
			wantCancelled = 1
		}
		if len(cancelled) != 32 || new(big.Int).SetBytes(cancelled).Cmp(big.NewInt(wantCancelled)) != 0 {
			t.Fatal("the canonical cancellation mapping differs")
		}
		var status, source, hash string
		var code, filled int
		var checkedEpoch, checkedBlock uint64
		var closedBlock *uint64
		if err := p.f.DB().QueryRow(`SELECT status,code,filled,source,checked_epoch,checked_block,checked_hash,closed_block
            FROM nft_orders WHERE hash=?`, input.Order.Hash().Hex()).Scan(&status, &code, &filled, &source, &checkedEpoch, &checkedBlock, &hash, &closedBlock); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus || code != int(wantCode) || filled != wantFilled || source != "api" {
			t.Fatal("cached book status differs from the direct executed action")
		}
		if status == StatusOpen {
			if checkedEpoch != epoch || hash != header.Hex() {
				t.Fatal("an open owner order is not checked at the current canonical hash and epoch")
			}
		} else {
			p.f.r.backend.mu.Lock()
			block, tx, _ := (executionEth{p.f.r.backend}).canonicalTransaction(p.TransactionHash())
			valid := block != nil && tx != nil && closedBlock != nil && *closedBlock == block.NumberU64()
			p.f.r.backend.mu.Unlock()
			if !valid {
				t.Fatal("a terminal owner order does not name the actual canonical transaction block")
			}
		}
		states = append(states, map[string]any{"status": wantStatus, "code": wantCode, "fillable": wantCode == CodeOK, "remaining": remaining})
	}
	return map[string]any{"action": p.Action(), "included": included, "approval": wantApproval == 1, "exchangeOrderNonce": wantNonce,
		"states": states, "directHashBoundContractReads": 8, "originalOwnersPreserved": true}
}
