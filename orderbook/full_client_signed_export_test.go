//go:build fullclient_recovery

package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The EOA order signature uses the existing public scalar-one fixture. NoM
// observations remain synthetic/unsigned; they establish no source authority.
// Initial Submit happens only after the priming CLI joined, before any fault.
// Once admitted, recovery never resubmits or repairs an order in the parent.
type FullClientSignedRecoveryFixture struct {
	*FullClientRecoveryCLIFixture
	admitted bool
}

func NewFullClientSignedRecoveryFixture(t *testing.T) *FullClientSignedRecoveryFixture {
	t.Helper()
	key := executedMarketKey(t)
	scenario := newExecutionScenarioInputs(t, true, false, executionScenarioInputs{
		seller: znn.Address(crypto.PubkeyToAddress(key.PublicKey)), timestamp: 1_800_000_000,
		sign: func(s *executionScenario) []*Input {
			var inputs []*Input
			for _, order := range s.orders {
				input, err := Sign(key, Domain(s.cfg.L2ChainID, s.exchange), order)
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, input)
			}
			return inputs
		},
	})
	return &FullClientSignedRecoveryFixture{FullClientRecoveryCLIFixture: newFullClientRecoveryForScenario(t, scenario, true, true)}
}

// Before initial admission there must be no order rows. This reads the same
// committed source/index/book horizons without needing an on-chain approval.
func (f *FullClientSignedRecoveryFixture) Ready(ctx context.Context, height uint64) (bool, string) {
	if f.admitted {
		return f.RecoveryCLIFixture.Ready(ctx, height)
	}
	if height != 7 {
		return false, "signed orders have not been initially admitted"
	}
	var derived, number, index, cursor, count uint64
	var hash, source string
	if err := f.read.QueryRowContext(ctx, "SELECT height,l2_number,l2_hash,hash FROM momentums ORDER BY height DESC LIMIT 1").Scan(&derived, &number, &hash, &source); err != nil {
		return false, err.Error()
	}
	if err := f.read.QueryRowContext(ctx, "SELECT coalesce(max(number),0) FROM blocks").Scan(&index); err != nil {
		return false, err.Error()
	}
	if err := f.read.QueryRowContext(ctx, "SELECT value FROM nft_order_meta WHERE key=?", "scanned:"+f.r.scenario.exchange.Hex()).Scan(&cursor); err != nil {
		return false, err.Error()
	}
	if err := f.read.QueryRowContext(ctx, "SELECT count(*) FROM nft_orders").Scan(&count); err != nil {
		return false, err.Error()
	}
	expected := f.r.scenario.branches[0][7].Momentum.Hash.String()
	return derived == 7 && number == 6 && index == 6 && cursor == 6 && count == 0 && source == expected && common.HexToHash(hash) != (common.Hash{}), fmt.Sprintf("signed_prime derived=%d number=%d index=%d cursor=%d orders=%d", derived, number, index, cursor, count)
}

// This is initial fixture admission through the actual Book.Submit contract
// checks against the real full client, not an HTTP/production-wallet test.
func (f *FullClientSignedRecoveryFixture) AdmitSignedOrders() {
	t := f.r.t
	t.Helper()
	if f.admitted || f.client == nil {
		t.Fatal("invalid initial signed-order admission phase")
	}
	db, err := store.Open(filepath.Join(f.Directory(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SQL().SetMaxOpenConns(1)
	head, err := db.Head()
	if err != nil || head == nil || head.Height != 7 || head.L2Number != 6 {
		t.Fatal("signed admission requires the committed priming horizon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actual, err := f.r.eth.HeaderByNumber(ctx, nil)
	if err != nil || actual.Number.Uint64() != head.L2Number || actual.Hash() != head.L2Hash {
		t.Fatal("signed admission full-client head differs from the durable local source")
	}
	canonical := func() (uint64, common.Hash) {
		current, err := db.Head()
		if err != nil || current == nil {
			return 0, common.Hash{}
		}
		return current.L2Number, current.L2Hash
	}
	book, err := New(Config{DB: db.SQL(), Eth: f.r.eth, ChainID: f.r.scenario.cfg.L2ChainID,
		Exchange: func() common.Address { return f.r.scenario.exchange }, Artifact: f.r.scenario.arts.MustGet("ZVMExchange"),
		CanonicalHead: canonical, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(f.r.scenario.inputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range f.r.scenario.inputs {
		signer, ok := Recover(Domain(f.r.scenario.cfg.L2ChainID, f.r.scenario.exchange), input)
		if !ok || signer != input.Order.Trader || input.Kind != KindSingle || len(input.Signature) == 0 {
			t.Fatal("initial input is not the declared genuine EOA signature")
		}
	}
	results, err := book.Submit(ctx, f.r.scenario.inputs)
	if err != nil || len(results) != 2 {
		t.Fatalf("signed admission failed: %v", err)
	}
	for i, result := range results {
		if !result.OK || result.Code != CodeOK || result.Hash != f.r.scenario.inputs[i].Order.Hash() {
			t.Fatalf("signed admission %d refused: %+v", i, result)
		}
	}
	after, err := json.Marshal(f.r.scenario.inputs)
	if err != nil || string(before) != string(after) {
		t.Fatal("initial admission changed the original signed input")
	}
	f.admitted = true
	t.Log("full_client_signed_initial_admission=true genuine_EOA_signatures=2 current_hash_contract_checks=true original_inputs_unchanged=true parent_admission_before_fault_only=true NoM_signatures=false HTTP_admission=false")
}

func (f *FullClientSignedRecoveryFixture) SignedSnapshot() map[string][]string {
	t := f.r.t
	t.Helper()
	if !f.admitted {
		t.Fatal("signed snapshot requires original admission")
	}
	f.r.assertExpectedInputsFrom(f.read, f.r.scenario.inputs)
	var count int
	if err := f.read.QueryRow("SELECT count(*) FROM logs WHERE address=? AND topic0=?", f.r.scenario.exchange.Hex(), topicOrderApproved.Hex()).Scan(&count); err != nil || count != 0 {
		t.Fatal("signed native fills used an on-chain order approval")
	}
	state := f.r.stateFrom(f.read)
	t.Log("full_client_signed_terminal_state=true balances_ownership_orders_chunks_deferred_receipts_verified=true original_signatures_preserved=true approval_logs=0 readonly_all_canonical_tables=true full_N04_acceptance=false")
	return state
}
