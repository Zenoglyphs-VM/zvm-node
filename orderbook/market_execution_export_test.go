package orderbook

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// The public scalar-one test key is constructed only in memory. It is not a
// wallet, credential or public-network account. Both native NoM observations
// and the loopback RPC control plane remain declared test adapters; contract
// calls and blocks execute the pinned op-geth engine and shipped artifacts.
func executedMarketKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	raw := make([]byte, 32)
	raw[31] = 1
	key, err := crypto.ToECDSA(raw)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type executedMarketRPC struct {
	*ethclient.Client
	mu              sync.Mutex
	calls, failures int
	hashes          map[common.Hash]int
}

// Observe, then delegate the complete call to ethclient. No ABI answer, check
// code or remaining quantity is fabricated by this counter.
func (p *executedMarketRPC) CallContractAtHash(ctx context.Context, call ethereum.CallMsg, hash common.Hash) ([]byte, error) {
	raw, err := p.Client.CallContractAtHash(ctx, call, hash)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.hashes[hash]++
	if err != nil {
		p.failures++
	}
	return raw, err
}

// ExecutedMarketFixture is exported only to this package's external tests,
// which can install the actual Market routes without an import cycle.
type ExecutedMarketFixture struct {
	r     *executionRun
	probe *executedMarketRPC
}

func NewExecutedMarketFixture(t *testing.T, kind string) *ExecutedMarketFixture {
	t.Helper()
	key := executedMarketKey(t)
	options := executionScenarioInputs{
		seller:    znn.Address(crypto.PubkeyToAddress(key.PublicKey)),
		timestamp: uint64(time.Now().Add(-time.Hour).Unix()),
		sign: func(s *executionScenario) []*Input {
			domain := Domain(s.cfg.L2ChainID, s.exchange)
			if kind == "bulk" {
				inputs, err := SignBulk(key, domain, s.orders)
				if err != nil {
					t.Fatal(err)
				}
				return inputs
			}
			if kind != "single" {
				t.Fatal("unsupported executed-market signature profile")
			}
			var inputs []*Input
			for _, order := range s.orders {
				input, err := Sign(key, domain, order)
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, input)
			}
			return inputs
		},
	}
	scenario := newExecutionScenarioInputs(t, true, false, options)
	r := newExecutionRun(t, scenario)
	probe := &executedMarketRPC{Client: r.eth, hashes: map[common.Hash]int{}}
	r.book.cfg.Eth = probe
	return &ExecutedMarketFixture{r: r, probe: probe}
}

func (f *ExecutedMarketFixture) DB() *sql.DB                { return f.r.db.SQL() }
func (f *ExecutedMarketFixture) Book() *Book                { return f.r.book }
func (f *ExecutedMarketFixture) Rollup() *rollup.Config     { return f.r.scenario.cfg }
func (f *ExecutedMarketFixture) Exchange() common.Address   { return f.r.scenario.exchange }
func (f *ExecutedMarketFixture) Collection() common.Address { return f.r.scenario.token }
func (f *ExecutedMarketFixture) Seller() common.Address     { return common.Address(f.r.scenario.seller) }
func (f *ExecutedMarketFixture) Buyer() common.Address      { return common.Address(f.r.scenario.buyer) }

func (f *ExecutedMarketFixture) Inputs(t *testing.T) []*Input {
	t.Helper()
	raw, err := json.Marshal(f.r.scenario.inputs)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []*Input
	if err := json.Unmarshal(raw, &inputs); err != nil {
		t.Fatal(err)
	}
	return inputs
}

func (f *ExecutedMarketFixture) Sign(t *testing.T, order *Order, chainID uint64) *Input {
	t.Helper()
	input, err := Sign(executedMarketKey(t), Domain(chainID, f.Exchange()), order)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

// Workers alone derive, reconcile, index and recheck. The caller selects a
// synthetic source branch/horizon, never a SQL row, rewind or EVM check result.
// All three production loops join before stable HTTP observations are made.
func (f *ExecutedMarketFixture) Advance(branch int, heights ...uint64) {
	f.r.t.Helper()
	if branch < 0 || branch >= len(f.r.scenario.branches) || len(heights) == 0 {
		f.r.t.Fatal("invalid market source horizon")
	}
	for _, height := range heights {
		if height < 6 || height > 9 {
			f.r.t.Fatal("market source horizon outside executed fixture")
		}
	}
	f.r.nomMu.Lock()
	f.r.branch = branch
	f.r.nomMu.Unlock()
	f.r.workerStages(heights...)
}

func (f *ExecutedMarketFixture) RPCCounts() (int, int, int) {
	f.probe.mu.Lock()
	defer f.probe.mu.Unlock()
	return f.probe.calls, f.probe.failures, len(f.probe.hashes)
}

func (f *ExecutedMarketFixture) Epoch(t *testing.T) uint64 {
	t.Helper()
	value, err := bookEpoch(f.DB())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *ExecutedMarketFixture) HeadHash() common.Hash { return f.r.d.Head().L2Hash }

func (f *ExecutedMarketFixture) AssertNoOrderApprovalLogs(t *testing.T) {
	t.Helper()
	var count int
	if err := f.DB().QueryRow("SELECT count(*) FROM logs WHERE address=? AND topic0=?", f.Exchange().Hex(), topicOrderApproved.Hex()).Scan(&count); err != nil || count != 0 {
		t.Fatal("signed market profile unexpectedly used on-chain order approval")
	}
}

func (f *ExecutedMarketFixture) ExecutedOwner(t *testing.T, id int64) common.Address {
	t.Helper()
	data, err := f.r.scenario.arts.MustGet("ZVMCollection").Call("ownerOf", big.NewInt(id))
	if err != nil {
		t.Fatal(err)
	}
	token := f.Collection()
	raw, err := f.r.eth.CallContractAtHash(context.Background(), ethereum.CallMsg{To: &token, Data: data}, f.HeadHash())
	if err != nil || len(raw) != 32 {
		t.Fatalf("executed owner read failed: bytes=%d error=%v", len(raw), err)
	}
	return common.BytesToAddress(raw)
}

func (f *ExecutedMarketFixture) AssertCurrentChecks(t *testing.T, filled int) {
	t.Helper()
	epoch := f.Epoch(t)
	head := f.r.d.Head()
	for i, input := range f.r.scenario.inputs {
		var status, hash, source string
		var code uint8
		var quantity, checkedEpoch, block uint64
		err := f.DB().QueryRow(`SELECT status,code,filled,checked_epoch,checked_block,checked_hash,source
            FROM nft_orders WHERE hash=?`, input.Order.Hash().Hex()).Scan(&status, &code, &quantity, &checkedEpoch, &block, &hash, &source)
		if err != nil {
			t.Fatal(err)
		}
		if checkedEpoch != epoch || source != "api" {
			t.Fatal("market check did not retain API intent in the current epoch")
		}
		header, err := f.r.eth.HeaderByNumber(context.Background(), new(big.Int).SetUint64(block))
		if err != nil || header.Hash().Hex() != hash {
			t.Fatal("market check is not bound to an executed canonical header")
		}
		if i == filled {
			if status != StatusFilled || code != CodeFilled || quantity != 1 {
				t.Fatal("executed fill was not reflected in the book")
			}
		} else if status != StatusOpen || code != CodeOK || quantity != 0 || hash != head.L2Hash.Hex() {
			t.Fatal("open order was not freshly rechecked at the selected canonical head")
		}
	}
}
