//go:build fullclient_recovery

package orderbook

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// AssertRecoveryWithOrderObservations keeps every canonical business field
// exact. A cached check records where it was observed, not the fill's receipt
// height: sequential cold replay and batched recovery can have different valid
// proof coordinates. Validate each coordinate before projecting only those
// three fields, and retain the original row vectors in the bounded job output.
// The plain Book value below calls only the existing read-only check method;
// it never runs New, initObservations, observe, a worker or any SQL writer.
func (f *FullClientRecoveryCLIFixture) AssertRecoveryWithOrderObservations(got, want map[string][]string) {
	f.r.t.Helper()
	inputs, err := json.Marshal(struct {
		Got  []string `json:"got"`
		Want []string `json:"want"`
	}{Got: got["nft_orders"], Want: want["nft_orders"]})
	if err != nil || len(inputs) > 16<<10 || len(got["nft_orders"]) != 2 || len(want["nft_orders"]) != 2 {
		f.r.t.Fatal("order observation proof inputs are missing or exceed their exact fixture bound")
	}
	f.r.t.Logf("owned_full_client_order_observation_inputs %s", inputs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checker := &Book{cfg: Config{Eth: f.r.eth, Artifact: f.r.scenario.arts.MustGet("ZVMExchange")}}
	tip := f.r.backend.chain.CurrentBlock()
	validate := func(state map[string][]string) map[string][]string {
		out := make(map[string][]string, len(state))
		for name, values := range state {
			out[name] = append([]string{}, values...)
		}
		out["nft_orders"] = []string{}
		for _, encoded := range state["nft_orders"] {
			var row map[string]json.RawMessage
			if err := json.Unmarshal([]byte(encoded), &row); err != nil {
				f.r.t.Fatal(err)
			}
			read := func(key string, value any) {
				if err := json.Unmarshal(row[key], value); err != nil {
					f.r.t.Fatalf("invalid retained order observation %s: %v", key, err)
				}
			}
			var block uint64
			var hash, raw, orderHash, exchange, status string
			var code uint8
			var filled int64
			var closed *uint64
			read("checked_block", &block)
			read("checked_hash", &hash)
			read("input", &raw)
			read("hash", &orderHash)
			read("exchange", &exchange)
			read("status", &status)
			read("code", &code)
			read("filled", &filled)
			read("closed_block", &closed)
			if block == 0 || block > tip.Number.Uint64() || exchange != f.r.scenario.exchange.Hex() {
				f.r.t.Fatal("order observation is outside the declared canonical context")
			}
			head, err := f.r.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(block))
			direct := f.r.backend.chain.GetHeaderByNumber(block)
			if err != nil || validHeader(head) != nil || direct == nil || head.Number.Uint64() != block || direct.Number.Uint64() != block || head.Hash().Hex() != hash || direct.Hash() != head.Hash() {
				f.r.t.Fatal("order observation does not name the same canonical RPC/direct header")
			}
			var input Input
			if err := json.Unmarshal([]byte(raw), &input); err != nil || input.Order.Check() != nil || input.Order.Hash().Hex() != orderHash {
				f.r.t.Fatal("retained original order input or hash changed")
			}
			amount, err := storedAmount(&input)
			if err != nil {
				f.r.t.Fatal(err)
			}
			checks, err := checker.check(ctx, common.HexToAddress(exchange), []*Input{&input}, head)
			if err != nil || len(checks) != 1 || checks[0].code != code || statusFor(code) != status || filled != amount-int64(checks[0].remaining) {
				f.r.t.Fatal("order observation disagrees with its block-hash-selected contract check")
			}
			if status == StatusOpen {
				if code != CodeOK || closed != nil || block != tip.Number.Uint64() {
					f.r.t.Fatal("open order is not checked at the current canonical tip")
				}
			} else if status != StatusFilled || code != CodeFilled || closed == nil || *closed != block || filled != amount {
				f.r.t.Fatal("closed order lacks the declared full-fill observation")
			}
			for _, key := range []string{"checked_block", "checked_hash", "closed_block"} {
				delete(row, key)
			}
			business, err := json.Marshal(row)
			if err != nil {
				f.r.t.Fatal(err)
			}
			out["nft_orders"] = append(out["nft_orders"], string(business))
		}
		sort.Strings(out["nft_orders"])
		return out
	}
	normalGot, normalWant := validate(got), validate(want)
	assertExecutionState(f.r.t, normalGot, normalWant)
	f.r.t.Log("owned_full_client_order_observation_equivalence=true canonical_business_fields_equal=true original_rows_retained=true hash_selected_contract_checks=true rpc_and_direct_headers_verified=true validated_rows=4 projected_observation_fields=checked_block,checked_hash,closed_block full_N04_acceptance=false")
}
