package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	_ "modernc.org/sqlite"
)

// This port declares read-only headers, logs and contract replies. It has no
// key, signer, wallet, simulated backend, transaction sender or network client.
type recoveryRPC struct {
	Chain        // Unexpected operations fail rather than silently simulating them.
	mu           sync.Mutex
	header       *types.Header
	headers      map[uint64]*types.Header
	code         uint8
	remaining    uint64
	logs         []types.Log
	blockCall    bool
	blockLogs    bool
	blockHeader  uint64
	mutate       func([]recoveryRPCResult) []recoveryRPCResult
	started      chan struct{}
	release      chan struct{}
	once         sync.Once
	hashCalls    int
	numberCalls  int
	selectedHash common.Hash
}

type recoveryRPCResult struct {
	Success    bool
	ReturnData []byte
}

func (p *recoveryRPC) BlockNumber(context.Context) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.header.Number.Uint64(), nil
}
func (p *recoveryRPC) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	p.mu.Lock()
	head := p.header
	if number != nil && number.Cmp(head.Number) != 0 {
		head = p.headers[number.Uint64()]
	}
	if head == nil {
		p.mu.Unlock()
		return nil, errors.New("undeclared header requested")
	}
	copy := types.CopyHeader(head)
	blocked := number != nil && p.blockHeader != 0 && number.Uint64() == p.blockHeader
	p.mu.Unlock()
	if blocked {
		if err := p.wait(ctx); err != nil {
			return nil, err
		}
	}
	return copy, nil
}
func (p *recoveryRPC) wait(ctx context.Context) error {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *recoveryRPC) FilterLogs(ctx context.Context, _ ethereum.FilterQuery) ([]types.Log, error) {
	if p.blockLogs {
		if err := p.wait(ctx); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]types.Log(nil), p.logs...), nil
}
func (p *recoveryRPC) CallContract(ctx context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	p.mu.Lock()
	p.numberCalls++
	p.mu.Unlock()
	return p.reply(ctx, msg)
}
func (p *recoveryRPC) CallContractAtHash(ctx context.Context, msg ethereum.CallMsg, hash common.Hash) ([]byte, error) {
	p.mu.Lock()
	p.hashCalls++
	p.selectedHash = hash
	p.mu.Unlock()
	return p.reply(ctx, msg)
}
func (p *recoveryRPC) reply(ctx context.Context, msg ethereum.CallMsg) ([]byte, error) {
	p.mu.Lock()
	code, remaining, blocked, mutate := p.code, p.remaining, p.blockCall, p.mutate
	p.mu.Unlock()
	if blocked {
		if err := p.wait(ctx); err != nil {
			return nil, err
		}
	}
	args, err := mcABI.Methods["aggregate3"].Inputs.Unpack(msg.Data[4:])
	if err != nil {
		return nil, err
	}
	n := reflect.ValueOf(args[0]).Len()
	word := append(common.LeftPadBytes([]byte{code}, 32), common.LeftPadBytes(new(big.Int).SetUint64(remaining).Bytes(), 32)...)
	rows := make([]recoveryRPCResult, n)
	for i := range rows {
		rows[i].Success = true
		rows[i].ReturnData = word
	}
	if mutate != nil {
		rows = mutate(rows)
	}
	return mcABI.Methods["aggregate3"].Outputs.Pack(rows)
}

func recoveryBook(t *testing.T) (*Book, *recoveryRPC, *sql.DB, common.Hash) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	// apps imports orderbook for deployment helpers. Read its pinned artifact
	// file directly so the complete orderbook test package stays acyclic.
	arts, err := bench.LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	rawArtifacts, err := os.ReadFile("../apps/artifacts/apps.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := arts.AddCombined(rawArtifacts); err != nil {
		t.Fatal(err)
	}
	p := &recoveryRPC{header: &types.Header{Number: big.NewInt(2), Time: 1020, GasLimit: 30000000, Difficulty: new(big.Int), Extra: []byte{1}}, remaining: 8, started: make(chan struct{}), release: make(chan struct{})}
	exchange := common.Address{19: 61}
	b, err := New(Config{DB: db, Eth: p, ChainID: 69, Artifact: arts.MustGet("ZVMExchange"), Exchange: func() common.Address { return exchange }})
	if err != nil {
		t.Fatal(err)
	}
	in := &Input{Order: Order{Trader: common.Address{19: 62}, Side: SideBuy, Collection: common.Address{19: 63}, TokenID: U(1), Amount: U(10), PaymentToken: common.Address{19: 64}, Price: U(1000), ListingTime: 0, ExpirationTime: 1 << 32, Nonce: U(4), Salt: U(7)}, Kind: KindSingle, Signature: hexutil.Bytes{1, 2, 3}, Proof: []common.Hash{}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	hash := in.Order.Hash()
	_, err = db.Exec(`INSERT INTO nft_orders(hash,exchange,trader,side,collection,token_id,amount,filled,payment_token,price,price_num,listing_time,expiration_time,nonce,kind,input,status,code,block,timestamp,checked_at,source)
		VALUES(?,?,?,?,?,?,10,0,?,'1000',0.001,0,4294967296,'4',?,?,'open',0,1,1010,111,'declared-opaque-fixture')`, hash.Hex(), exchange.Hex(), in.Order.Trader.Hex(), in.Order.Side, in.Order.Collection.Hex(), "1", in.Order.PaymentToken.Hex(), in.Kind, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.setMeta("scanned:"+exchange.Hex(), 2); err != nil {
		t.Fatal(err)
	}
	return b, p, db, hash
}

func recoveryBookState(t *testing.T, db *sql.DB) string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"nft_orders", "nft_order_meta"} {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			v := make([]any, len(cols))
			ptr := make([]any, len(v))
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], string(raw))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(out[table])
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func awaitRecoveryRPC(t *testing.T, p *recoveryRPC) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		t.Fatal("declared RPC barrier was not reached")
	}
}

func TestRPCRewindRejectsAnInflightCheck(t *testing.T) {
	b, p, db, _ := recoveryBook(t)
	p.blockCall = true
	p.code = CodeFilled
	p.remaining = 0
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; err == nil {
		t.Fatal("stale check response was accepted after an explicit rewind")
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("stale check changed persisted observations or original input")
	}
}

func TestRPCRewindRejectsInflightLogsAndCursor(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	p.blockLogs = true
	p.logs = []types.Log{{Address: b.cfg.Exchange(), Topics: []common.Hash{topicOrderCancelled, hash, common.BytesToHash((common.Address{19: 62}).Bytes())}, BlockNumber: 2, BlockHash: p.header.Hash()}}
	if err := b.setMeta("scanned:"+b.cfg.Exchange().Hex(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; err == nil {
		t.Fatal("old log page was accepted after rewind")
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("old log page changed orders or advanced the rewound cursor")
	}
}

func TestRPCRewindMarksTheCachedCheckUnknown(t *testing.T) {
	b, _, _, hash := recoveryBook(t)
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	in, status, code, err := b.Get(hash)
	if err != nil || in.Order.Hash() != hash {
		t.Fatalf("original input unavailable: %v", err)
	}
	if status == StatusOpen && code == CodeOK {
		t.Fatal("rewind exposed an order as fillable before a fresh check")
	}
}

func TestRPCCheckUsesAnExactBlockHash(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	hashCalls, numberCalls, selected := p.hashCalls, p.numberCalls, p.selectedHash
	p.mu.Unlock()
	if hashCalls != 1 || numberCalls != 0 || selected != p.header.Hash() {
		t.Fatalf("check selector=%d hash/%d number, hash=%s", hashCalls, numberCalls, selected.Hex())
	}
	var filled int64
	if err := db.QueryRow("SELECT filled FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&filled); err != nil || filled != 2 {
		t.Fatalf("declared observation=%d: %v", filled, err)
	}
}

func TestRPCLateSubmitCannotOverwriteRewind(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	in, _, _, err := b.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	// The declared on-chain kind takes no signing or recovery path. It is only
	// submitted to this read-only stub; no transaction is constructed or sent.
	in.Kind, in.Signature = KindOnChain, nil
	p.blockCall = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := b.Submit(ctx, []*Input{in}); done <- err }()
	awaitRecoveryRPC(t, p)
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; err == nil {
		t.Fatal("late submission overwrote a newer rewind")
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("late submission changed the original payload or observation")
	}
}

func TestRPCRewindAllowsFreshFillDecreaseAndForcesASweep(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	var original string
	if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&original); err != nil {
		t.Fatal(err)
	}
	p.code, p.remaining = CodeFilled, 0
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	p.code, p.remaining = CodeOK, 8
	// Recheck's normal timer has not elapsed. The new epoch must still cause
	// the reopened order to be checked on this very next step.
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	var raw, status string
	var code, filled int64
	var closed sql.NullInt64
	if err := db.QueryRow("SELECT input,status,code,filled,closed_block FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&raw, &status, &code, &filled, &closed); err != nil {
		t.Fatal(err)
	}
	if raw != original || status != StatusOpen || code != int64(CodeOK) || filled != 2 || closed.Valid {
		t.Fatalf("fresh recovery changed intent or retained old closure/fill: %s/%d/%d/%v", status, code, filled, closed)
	}
}

func TestRPCCancellationReplyDoesNotInventAFullFill(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	b.cfg.Recheck = time.Nanosecond
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.header.Number, p.header.Time = big.NewInt(3), 1030
	p.code, p.remaining = CodeCancelled, 0
	// The independent fill counter is declared as two, not inferred from the refusal.
	b.cfg.Eth = &closureProofRPC{recoveryRPC: p, book: b, legacySingleCheck: true,
		replies: map[common.Hash]closureProofReply{p.header.Hash(): {uint64(CodeCancelled), 0, 2, nil}}}
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	var filled, closed int64
	var status string
	if err := db.QueryRow("SELECT status,filled,closed_block FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&status, &filled, &closed); err != nil {
		t.Fatal(err)
	}
	if status != StatusCancelled || filled != 2 || closed != 3 {
		t.Fatalf("early cancellation reply invented fills: %s/%d/%d", status, filled, closed)
	}
	var checkEpoch, fillEpoch int64
	var checkedHash string
	if err := db.QueryRow("SELECT checked_epoch,filled_epoch,checked_hash FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&checkEpoch, &fillEpoch, &checkedHash); err != nil {
		t.Fatal(err)
	}
	if checkEpoch < 0 || fillEpoch != checkEpoch || checkedHash != p.header.Hash().Hex() {
		t.Fatal("cancellation lacks its independent current fill proof")
	}
}

func TestRPCConflictingFillIsHiddenUntilAnExplicitRewind(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	b.cfg.Recheck = time.Nanosecond
	p.remaining = 6
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	epoch, err := bookEpoch(db)
	if err != nil {
		t.Fatal(err)
	}
	p.header.Number, p.header.Time, p.remaining = big.NewInt(3), 1030, 8
	if err := b.step(context.Background()); !errors.Is(err, errCheckConflict) {
		t.Fatalf("contradictory fill was accepted: %v", err)
	}
	_, _, code, err := b.Get(hash)
	var filled int64
	if err != nil || db.QueryRow("SELECT filled FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&filled) != nil || filled != 4 || code != CodeUnchecked {
		t.Fatalf("conflict remained fillable or rewrote the fill: %d/%d/%v", filled, code, err)
	}
	if now, err := bookEpoch(db); err != nil || now != epoch {
		t.Fatalf("a response invented a rewind: %d/%v", now, err)
	}
	if err := b.Rewind(1); err != nil {
		t.Fatal(err)
	}
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT filled FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&filled); err != nil || filled != 2 {
		t.Fatalf("fresh post-rewind fill=%d: %v", filled, err)
	}
}

func TestRPCLateOlderCheckPreservesTheNewerObservation(t *testing.T) {
	b, p, db, _ := recoveryBook(t)
	b.cfg.Recheck = time.Nanosecond
	p.blockCall = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	p.mu.Lock()
	old := types.CopyHeader(p.header)
	p.headers = map[uint64]*types.Header{2: old}
	p.header = types.CopyHeader(old)
	p.header.Number, p.header.Time = big.NewInt(3), 1030
	p.blockCall, p.remaining = false, 7
	p.mu.Unlock()
	if err := b.step(ctx); err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; !errors.Is(err, errStaleObservation) {
		t.Fatalf("older observation was accepted: %v", err)
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("older observation changed a newer persisted check")
	}
}

func TestRPCChangedCanonicalHeaderRejectsTheReply(t *testing.T) {
	b, p, db, _ := recoveryBook(t)
	p.blockCall = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	p.mu.Lock()
	p.header.Extra = []byte{2}
	p.mu.Unlock()
	before := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; !errors.Is(err, errStaleObservation) {
		t.Fatalf("reply from the replaced hash was accepted: %v", err)
	}
	if recoveryBookState(t, db) != before {
		t.Fatal("replaced-header reply changed persisted state")
	}
}

func TestRPCMalformedRepliesLeaveAllRowsUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]recoveryRPCResult) []recoveryRPCResult
	}{
		{"missing-result", func(r []recoveryRPCResult) []recoveryRPCResult { return nil }},
		{"extra-result", func(r []recoveryRPCResult) []recoveryRPCResult { return append(r, r[0]) }},
		{"failed-call", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].Success = false; return r }},
		{"short-word", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData = r[0].ReturnData[:63]; return r }},
		{"wide-code", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData[0] = 1; return r }},
		{"unknown-code", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData[31] = 10; return r }},
		{"wide-remaining", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData[32] = 1; return r }},
		{"excess-remaining", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData[63] = 11; return r }},
		{"zero-fillable-remaining", func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData[63] = 0; return r }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, p, db, _ := recoveryBook(t)
			p.mutate = tc.mutate
			before := recoveryBookState(t, db)
			if err := b.step(context.Background()); err == nil {
				t.Fatal("malformed reply was accepted")
			}
			if recoveryBookState(t, db) != before {
				t.Fatal("malformed reply changed an observation or original input")
			}
		})
	}
}

type recoveryNumberOnlyChain struct{ Chain }

func TestRPCMissingHashCallSupportFailsClosed(t *testing.T) {
	b, p, db, _ := recoveryBook(t)
	b.cfg.Eth = recoveryNumberOnlyChain{p}
	before := recoveryBookState(t, db)
	if err := b.step(context.Background()); err == nil {
		t.Fatal("a number-only reader was accepted for a hash-bound check")
	}
	if p.numberCalls != 0 || p.hashCalls != 0 || recoveryBookState(t, db) != before {
		t.Fatal("unsupported reader called latest or changed persisted state")
	}
}

func TestRPCLogPageAndCursorCommitTogether(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	p.logs = []types.Log{{Address: b.cfg.Exchange(), Topics: []common.Hash{topicOrderCancelled, hash, common.BytesToHash((common.Address{19: 62}).Bytes())}, BlockNumber: 2, BlockHash: p.header.Hash()}}
	if err := b.setMeta("scanned:"+b.cfg.Exchange().Hex(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER declared_cursor_refusal BEFORE UPDATE ON nft_order_meta WHEN OLD.key LIKE 'scanned:%' BEGIN SELECT RAISE(ABORT,'declared cursor write refusal'); END"); err != nil {
		t.Fatal(err)
	}
	before := recoveryBookState(t, db)
	if err := b.step(context.Background()); err == nil || !strings.Contains(err.Error(), "declared cursor write refusal") {
		t.Fatalf("cursor fault not reached: %v", err)
	}
	if recoveryBookState(t, db) != before || p.hashCalls != 0 {
		t.Fatal("log mutation survived the refused cursor commit")
	}
}

func TestRPCInvalidLogPagesDoNotAdvanceTheCursor(t *testing.T) {
	for _, field := range []string{"block-hash", "range", "exchange", "removed"} {
		t.Run(field, func(t *testing.T) {
			b, p, db, hash := recoveryBook(t)
			l := types.Log{Address: b.cfg.Exchange(), Topics: []common.Hash{topicOrderCancelled, hash, {}}, BlockNumber: 2, BlockHash: p.header.Hash()}
			switch field {
			case "block-hash":
				l.BlockHash = common.Hash{31: 99}
			case "range":
				l.BlockNumber = 3
			case "exchange":
				l.Address = common.Address{19: 99}
			case "removed":
				l.Removed = true
			}
			p.logs = []types.Log{l}
			if err := b.setMeta("scanned:"+b.cfg.Exchange().Hex(), 0); err != nil {
				t.Fatal(err)
			}
			before := recoveryBookState(t, db)
			if err := b.step(context.Background()); err == nil {
				t.Fatal("invalid log page was accepted")
			}
			if recoveryBookState(t, db) != before {
				t.Fatal("invalid log page advanced or changed the book")
			}
		})
	}
}

func TestRPCEpochExhaustionAndUnsupportedRewindAreAtomic(t *testing.T) {
	for _, test := range []string{"epoch-exhaustion", "unsupported-height"} {
		t.Run(test, func(t *testing.T) {
			b, _, db, _ := recoveryBook(t)
			height := uint64(1)
			if test == "epoch-exhaustion" {
				if _, err := db.Exec("UPDATE nft_order_meta SET value=? WHERE key='rewind_epoch'", int64(math.MaxInt64)); err != nil {
					t.Fatal(err)
				}
			} else {
				height = uint64(math.MaxInt64) + 1
			}
			before := recoveryBookState(t, db)
			if err := b.Rewind(height); err == nil {
				t.Fatal("unrepresentable rewind was accepted")
			}
			if recoveryBookState(t, db) != before {
				t.Fatal("failed epoch/height guard left a partial rewind")
			}
		})
	}
}

func TestRPCLegacyMigrationPreservesIntentAndRollsBackOnRefusal(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusal-%t", refuse), func(t *testing.T) {
			b, _, db, hash := recoveryBook(t)
			for _, name := range []string{"nft_orders_closure_check", "nft_orders_closure_fill", "nft_orders_closure_unproved"} {
				if _, err := db.Exec("DROP INDEX IF EXISTS " + name); err != nil {
					t.Fatal(err)
				}
			}
			for _, column := range []string{"checked_epoch", "checked_block", "checked_hash", "filled_epoch"} {
				if _, err := db.Exec("ALTER TABLE nft_orders DROP COLUMN " + column); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec("DELETE FROM nft_order_meta WHERE key='rewind_epoch'"); err != nil {
				t.Fatal(err)
			}
			var input string
			if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&input); err != nil {
				t.Fatal(err)
			}
			before := recoveryBookState(t, db)
			if refuse {
				if _, err := db.Exec("CREATE TRIGGER declared_migration_refusal BEFORE UPDATE ON nft_orders BEGIN SELECT RAISE(ABORT,'declared migration refusal'); END"); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := New(b.cfg)
			if refuse {
				if err == nil || !strings.Contains(err.Error(), "declared migration refusal") || recoveryBookState(t, db) != before {
					t.Fatalf("refused migration left a partial change: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			in, _, code, err := restarted.Get(hash)
			if err != nil || in.Order.Hash() != hash || code != CodeUnchecked {
				t.Fatalf("legacy order unavailable or incorrectly fillable: %d/%v", code, err)
			}
			var after string
			if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&after); err != nil || after != input {
				t.Fatalf("migration changed the stored signature/input: %v", err)
			}
		})
	}
}

func TestRPCRestartFencesAnInflightCheck(t *testing.T) {
	b, p, db, _ := recoveryBook(t)
	p.blockCall = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	restarted, err := New(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; !errors.Is(err, errStaleObservation) {
		t.Fatalf("pre-restart check was accepted: %v", err)
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("pre-restart reply changed the restarted book")
	}
	if err := restarted.step(ctx); err != nil {
		t.Fatal(err)
	}
}

func recoveryApprovalLog(t *testing.T, b *Book, in *Input, head *types.Header) types.Log {
	t.Helper()
	data, err := b.cfg.Artifact.ABI.Events["OrderApproved"].Inputs.NonIndexed().Pack(in.Order.ABI())
	if err != nil {
		t.Fatal(err)
	}
	return types.Log{Address: b.cfg.Exchange(), Topics: []common.Hash{topicOrderApproved, in.Order.Hash(), common.BytesToHash(in.Order.Trader.Bytes())},
		Data: data, BlockNumber: head.Number.Uint64(), BlockHash: head.Hash()}
}

func TestRPCApprovalLogsKeepOriginalSignaturesAndCheckNewInputs(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprintf("existing-%t", existing), func(t *testing.T) {
			b, p, db, originalHash := recoveryBook(t)
			in, _, _, err := b.Get(originalHash)
			if err != nil {
				t.Fatal(err)
			}
			var original string
			if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", originalHash.Hex()).Scan(&original); err != nil {
				t.Fatal(err)
			}
			if !existing {
				in.Order.Salt = U(8)
			}
			p.logs = []types.Log{recoveryApprovalLog(t, b, in, p.header)}
			if err := b.setMeta("scanned:"+b.cfg.Exchange().Hex(), 0); err != nil {
				t.Fatal(err)
			}
			if err := b.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, status, code, err := b.Get(in.Order.Hash())
			if err != nil || status != StatusOpen || code != CodeOK || (!existing && got.Kind != KindOnChain) {
				t.Fatalf("approved input was not freshly checked: %s/%d/%v", status, code, err)
			}
			var after string
			if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", originalHash.Hex()).Scan(&after); err != nil || after != original {
				t.Fatalf("approval overwrote an opaque signed payload: %v", err)
			}
		})
	}
}

func TestRPCLateApprovalHeaderCannotInsertAfterRewind(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	in, _, _, err := b.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	in.Order.Salt = U(8)
	older := types.CopyHeader(p.header)
	older.Number, older.Time = big.NewInt(1), 1010
	p.headers = map[uint64]*types.Header{1: older}
	p.header.ParentHash = older.Hash()
	p.logs = []types.Log{recoveryApprovalLog(t, b, in, older)}
	p.blockHeader = 1
	if err := b.setMeta("scanned:"+b.cfg.Exchange().Hex(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.step(ctx) }()
	awaitRecoveryRPC(t, p)
	// With one connection this also proves the header RPC holds no SQL writer.
	if err := b.Rewind(0); err != nil {
		t.Fatal(err)
	}
	after := recoveryBookState(t, db)
	close(p.release)
	if err := <-done; !errors.Is(err, errStaleObservation) {
		t.Fatalf("late approval header was accepted: %v", err)
	}
	if recoveryBookState(t, db) != after {
		t.Fatal("late approval inserted an order or changed the scan cursor")
	}
}

func TestRPCReaderRetreatCannotWritePastTheScanCursor(t *testing.T) {
	b, p, db, hash := recoveryBook(t)
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	in, _, _, err := b.Get(hash)
	if err != nil {
		t.Fatal(err)
	}
	in.Kind, in.Signature, in.Order.Salt = KindOnChain, nil, U(8)
	p.header.Number, p.header.Time = big.NewInt(1), 1010
	before := recoveryBookState(t, db)
	if err := b.step(context.Background()); !errors.Is(err, errStaleObservation) {
		t.Fatalf("a retreating reader rewrote the scan observation: %v", err)
	}
	if _, err := b.Submit(context.Background(), []*Input{in}); !errors.Is(err, errStaleObservation) {
		t.Fatalf("submission wrote behind the scan cursor: %v", err)
	}
	if recoveryBookState(t, db) != before {
		t.Fatal("reader retreat changed an order, observation or cursor")
	}
}
