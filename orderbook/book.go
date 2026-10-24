package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// Where the order book lives, and how it stays honest.
//
// Unfilled orders are not chain state: they are signatures somebody handed this
// node. So they live in their own table, beside the index but never dropped or
// rewound with it (D28, D36) — an index can be rebuilt from the chain, an order
// book cannot. Fills, cancellations, nonce bumps and on-chain approvals *are*
// chain state, and the book follows them from the exchange's logs.
//
// Nothing is shown on the strength of the row alone. Every order is re-checked
// with ZVMExchange.checkOrder — the same check a fill makes — when it arrives,
// when an exchange event names it, and on a timer, because a listing also stops
// being fillable when its token moves, and that emits nothing the exchange sees.

const schema = `
CREATE TABLE IF NOT EXISTS nft_orders (
	hash            TEXT PRIMARY KEY,
	exchange        TEXT    NOT NULL,
	trader          TEXT    NOT NULL,
	side            INTEGER NOT NULL,
	collection      TEXT    NOT NULL,
	token_id        TEXT,
	amount          INTEGER NOT NULL,
	filled          INTEGER NOT NULL DEFAULT 0,
	payment_token   TEXT    NOT NULL,
	price           TEXT    NOT NULL,
	price_num       REAL    NOT NULL,
	listing_time    INTEGER NOT NULL,
	expiration_time INTEGER NOT NULL,
	nonce           TEXT    NOT NULL,
	kind            INTEGER NOT NULL,
	root            TEXT,
	input           TEXT    NOT NULL,
	status          TEXT    NOT NULL,
	code            INTEGER NOT NULL,
	block           INTEGER NOT NULL,
	timestamp       INTEGER NOT NULL,
	checked_at      INTEGER NOT NULL,
	checked_epoch   INTEGER NOT NULL DEFAULT -1,
	checked_block   INTEGER NOT NULL DEFAULT 0,
	checked_hash    TEXT NOT NULL DEFAULT '',
	filled_epoch    INTEGER NOT NULL DEFAULT -1,
	closed_block    INTEGER,
	source          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS nft_orders_book ON nft_orders(collection, side, status, code);
CREATE INDEX IF NOT EXISTS nft_orders_trader ON nft_orders(trader, status);
CREATE INDEX IF NOT EXISTS nft_orders_token ON nft_orders(collection, token_id, side, status);
CREATE INDEX IF NOT EXISTS nft_orders_open ON nft_orders(status, checked_at);
CREATE TABLE IF NOT EXISTS nft_order_meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
`

// Order statuses. An open order is shown only while its last check said OK.
const (
	StatusOpen      = "open"
	StatusFilled    = "filled"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
	StatusInvalid   = "invalid"
)

func statusFor(code uint8) string {
	switch code {
	case CodeFilled:
		return StatusFilled
	case CodeCancelled, CodeStaleNonce:
		return StatusCancelled
	case CodeExpired:
		return StatusExpired
	case CodeBadSignature, CodeMalformed:
		return StatusInvalid
	}
	return StatusOpen
}

var (
	topicOrderFilled      = crypto.Keccak256Hash([]byte("OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)"))
	topicOrderCancelled   = crypto.Keccak256Hash([]byte("OrderCancelled(bytes32,address)"))
	topicNonceIncremented = crypto.Keccak256Hash([]byte("NonceIncremented(address,uint256)"))
	topicOrderApproved    = crypto.Keccak256Hash([]byte("OrderApproved(bytes32,address,(address,uint8,address,uint256,bool,uint256,address,uint256,uint64,uint64,uint256,uint256))"))

	multicall3 = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")
	mcABI      = mustABI(`[{"type":"function","name":"aggregate3","stateMutability":"view","inputs":[{"name":"calls","type":"tuple[]","components":[{"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}]}],"outputs":[{"name":"returnData","type":"tuple[]","components":[{"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}]}]}]`)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(err)
	}
	return a
}

// Chain is what the book reads from the ZVM: an ethclient.Client, or a
// simulated backend in tests.
type Chain interface {
	ethereum.ChainReader
	ethereum.BlockNumberReader
	ethereum.ContractCaller
	ethereum.LogFilterer
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
}

// Config wires a Book to one chain and one exchange.
type Config struct {
	DB       *sql.DB
	Eth      Chain
	ChainID  uint64
	Exchange func() common.Address
	// Artifact is ZVMExchange's compiled ABI.
	Artifact *bench.Artifact
	// Poll is how often the exchange's logs are read; Recheck how often every
	// open order is checked again whatever the logs said.
	Poll    time.Duration
	Recheck time.Duration
	// MaxOpenPerTrader and MaxOpen bound the book: one trader's open orders,
	// and everyone's. An order costs nothing to sign — one funded wallet can
	// sign any number of distinct, individually fillable orders offline — and
	// every open one is re-checked on each sweep, so without a bound the book
	// was a table anyone could grow, and a sweep anyone could make as slow as
	// they liked. Defaults 5,000 and 200,000.
	MaxOpenPerTrader int
	MaxOpen          int
	Log              *slog.Logger
}

type Book struct {
	cfg Config

	mu             sync.Mutex
	lastSweep      time.Time
	lastSweepEpoch uint64
	lastErr        string
}

func New(cfg Config) (*Book, error) {
	if cfg.Poll == 0 {
		cfg.Poll = 3 * time.Second
	}
	if cfg.Recheck == 0 {
		cfg.Recheck = 30 * time.Second
	}
	if cfg.MaxOpenPerTrader <= 0 {
		cfg.MaxOpenPerTrader = 5_000
	}
	if cfg.MaxOpen <= 0 {
		cfg.MaxOpen = 200_000
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := initObservations(cfg.DB); err != nil {
		return nil, fmt.Errorf("order book schema: %w", err)
	}
	return &Book{cfg: cfg}, nil
}

// Domain is the EIP-712 domain of the exchange this book serves, or false
// while no exchange is deployed.
func (b *Book) Domain() (common.Hash, common.Address, bool) {
	ex := b.cfg.Exchange()
	if ex == (common.Address{}) {
		return common.Hash{}, ex, false
	}
	return Domain(b.cfg.ChainID, ex), ex, true
}

// Result is what happened to one submitted order.
type Result struct {
	Hash   common.Hash `json:"hash"`
	OK     bool        `json:"ok"`
	Code   uint8       `json:"code"`
	Reason string      `json:"reason,omitempty"`
}

// MaxSubmit bounds one submission: a bulk listing is one signature over this
// many orders at most.
const MaxSubmit = 500

// Submit checks orders against the exchange and keeps the fillable ones.
func (b *Book) Submit(ctx context.Context, inputs []*Input) ([]Result, error) {
	domain, exchange, ok := b.Domain()
	if !ok {
		return nil, errors.New("no exchange is deployed on this chain")
	}
	if len(inputs) == 0 || len(inputs) > MaxSubmit {
		return nil, fmt.Errorf("submit between 1 and %d orders", MaxSubmit)
	}
	at, err := b.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Result, len(inputs))
	var toCheck []*Input
	var idx []int
	for i, in := range inputs {
		if in == nil {
			results[i] = Result{Reason: "missing order input", Code: CodeMalformed}
			continue
		}
		if err := in.Order.Check(); err != nil {
			results[i] = Result{Reason: err.Error(), Code: CodeMalformed}
			continue
		}
		if _, err := storedAmount(in); err != nil {
			results[i] = Result{Reason: err.Error(), Code: CodeMalformed}
			continue
		}
		results[i].Hash = in.Order.Hash()
		switch in.Kind {
		case KindSingle, KindBulk:
			// An EOA's signature can be refused here without a call. Anything
			// else — a contract wallet's ERC-1271 answer — is the exchange's
			// to judge.
			if signer, ok := Recover(domain, in); ok && signer != in.Order.Trader {
				if code, _ := b.cfg.Eth.CodeAt(ctx, in.Order.Trader, at.head.Number); len(code) == 0 {
					results[i].Code, results[i].Reason = CodeBadSignature, fmt.Sprintf("signed by %s, not by the trader", signer)
					continue
				}
			}
			if in.Kind == KindBulk {
				root := ProcessProof(results[i].Hash, in.Proof)
				in.Root = &root
			}
		case KindOnChain:
		default:
			results[i].Code, results[i].Reason = CodeMalformed, "unknown signature kind"
			continue
		}
		toCheck = append(toCheck, in)
		idx = append(idx, i)
	}
	checks, err := b.check(ctx, exchange, toCheck, at.head)
	if err != nil {
		return nil, err
	}
	if err := b.verifyHeader(ctx, at.head); err != nil {
		return nil, err
	}
	tx, err := b.observationTx(ctx, at.epoch)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := guardScanHeight(tx, exchange, at.head); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var open int
	if err := tx.QueryRow("SELECT COUNT(*) FROM nft_orders WHERE status = ?", StatusOpen).Scan(&open); err != nil {
		return nil, err
	}
	perTrader := map[common.Address]int{}
	for j, in := range toCheck {
		i := idx[j]
		c := checks[j]
		results[i].Code = c.code
		if c.code != CodeOK {
			results[i].Reason = CodeText(c.code)
			continue
		}
		// An order already in the book takes no new room; a new one has to fit.
		var known int
		if err := tx.QueryRow("SELECT COUNT(*) FROM nft_orders WHERE hash = ?", in.Order.Hash().Hex()).Scan(&known); err != nil {
			return nil, err
		}
		if known == 0 {
			trader := in.Order.Trader
			n, counted := perTrader[trader]
			if !counted {
				if err := tx.QueryRow("SELECT COUNT(*) FROM nft_orders WHERE trader = ? AND status = ?",
					trader.Hex(), StatusOpen).Scan(&n); err != nil {
					return nil, err
				}
			}
			if n >= b.cfg.MaxOpenPerTrader {
				results[i].Reason = fmt.Sprintf("%s already has %d open orders on this node; cancel or let some expire first",
					trader.Hex(), b.cfg.MaxOpenPerTrader)
				perTrader[trader] = n
				continue
			}
			if open >= b.cfg.MaxOpen {
				results[i].Reason = "this node's order book is full"
				continue
			}
			perTrader[trader], open = n+1, open+1
		}
		accepted, err := b.insert(tx, in, exchange, c, at, "api", now)
		if err != nil {
			return nil, err
		}
		if !accepted {
			results[i].Code, results[i].Reason = CodeUnchecked, errCheckConflict.Error()
			continue
		}
		results[i].OK = true
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func (b *Book) insert(tx *sql.Tx, in *Input, exchange common.Address, c check, at observation, source string, now int64) (bool, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return false, err
	}
	o := &in.Order
	var tokenID any = o.TokenID.Big().String()
	if o.AnyToken {
		tokenID = nil
	}
	var root any
	if in.Root != nil {
		root = in.Root.Hex()
	}
	amount, err := storedAmount(in)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(`INSERT INTO nft_orders
		(hash, exchange, trader, side, collection, token_id, amount, filled, payment_token, price, price_num,
		 listing_time, expiration_time, nonce, kind, root, input, status, code, block, timestamp, checked_at, source)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hash) DO NOTHING`,
		o.Hash().Hex(), exchange.Hex(), o.Trader.Hex(), o.Side, o.Collection.Hex(), tokenID, amount,
		0, o.PaymentToken.Hex(), o.Price.Big().String(), b.priceNum(tx, o.PaymentToken, o.Price.Big()),
		o.ListingTime, o.ExpirationTime, o.Nonce.Big().String(), in.Kind, root, string(raw),
		StatusOpen, CodeUnchecked, at.head.Number.Int64(), at.head.Time, 0, source)
	if err != nil {
		return false, err
	}
	if c.code == CodeUnchecked {
		return true, nil // A log supplied the input, not a fillability check.
	}
	accepted, err := b.observe(tx, in, exchange, c, at, now)
	if err != nil || !accepted {
		return accepted, err
	}
	// Keep a submitted signature for an input previously learned only from an
	// on-chain approval. Existing signed inputs and their hashes stay intact.
	if in.Kind != KindOnChain {
		_, err = tx.Exec("UPDATE nft_orders SET input=?,kind=?,root=? WHERE hash=? AND kind=? AND exchange=?",
			string(raw), in.Kind, root, o.Hash().Hex(), KindOnChain, exchange.Hex())
	}
	return err == nil, err
}

// priceNum is a price in whole units of its currency, for sorting and floors.
// ZNN and wrapped ZNN have 18 decimals; anything the index knows says its own.
func (b *Book) priceNum(q bookSQL, token common.Address, price *big.Int) float64 {
	decimals := 18
	if token != (common.Address{}) {
		var d sql.NullInt64
		if q.QueryRow("SELECT decimals FROM tokens WHERE address = ?", token.Hex()).Scan(&d) == nil && d.Valid {
			decimals = int(d.Int64)
		}
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(price), new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))).Float64()
	return f
}

type check struct {
	code      uint8
	remaining uint64
}

// check runs checkOrder for many inputs in as few calls as Multicall3 allows.
func (b *Book) check(ctx context.Context, exchange common.Address, inputs []*Input, head *types.Header) ([]check, error) {
	out := make([]check, len(inputs))
	if len(inputs) == 0 {
		return out, nil
	}
	client, ok := b.cfg.Eth.(hashCaller)
	if !ok {
		return nil, errors.New("order checks require block-hash contract calls")
	}
	type call struct {
		Target       common.Address
		AllowFailure bool
		CallData     []byte
	}
	const batch = 100
	for start := 0; start < len(inputs); start += batch {
		end := min(start+batch, len(inputs))
		calls := make([]call, 0, end-start)
		for _, in := range inputs[start:end] {
			data, err := b.cfg.Artifact.Call("checkOrder", in.ABI())
			if err != nil {
				return nil, err
			}
			calls = append(calls, call{Target: exchange, AllowFailure: true, CallData: data})
		}
		data, err := mcABI.Pack("aggregate3", calls)
		if err != nil {
			return nil, err
		}
		raw, err := client.CallContractAtHash(ctx, ethereum.CallMsg{To: &multicall3, Data: data}, head.Hash())
		if err != nil {
			return nil, fmt.Errorf("checking orders: %w", err)
		}
		var res []struct {
			Success    bool
			ReturnData []byte
		}
		if err := mcABI.UnpackIntoInterface(&res, "aggregate3", raw); err != nil {
			return nil, err
		}
		if len(res) != end-start {
			return nil, errors.New("checking orders: result count does not match request")
		}
		for i, r := range res {
			if !r.Success || len(r.ReturnData) != 64 {
				return nil, errors.New("checking orders: missing or malformed result")
			}
			code := new(big.Int).SetBytes(r.ReturnData[:32])
			remaining := new(big.Int).SetBytes(r.ReturnData[32:])
			amount, err := storedAmount(inputs[start+i])
			if err != nil {
				return nil, err
			}
			if !code.IsUint64() || code.Uint64() > uint64(CodeMalformed) || !remaining.IsInt64() || remaining.Cmp(big.NewInt(amount)) > 0 {
				return nil, errors.New("checking orders: out-of-range result")
			}
			c := check{code: uint8(code.Uint64()), remaining: remaining.Uint64()}
			switch c.code {
			case CodeOK, CodeNotHeld, CodeNotFunded:
				if c.remaining == 0 {
					return nil, errors.New("checking orders: inconsistent remaining amount")
				}
			default:
				if c.remaining != 0 {
					return nil, errors.New("checking orders: unexpected remaining amount")
				}
			}
			out[start+i] = c
		}
	}
	return out, nil
}

// Get returns one order and how its trader agreed to it, which is what a fill
// sends to the exchange.
func (b *Book) Get(hash common.Hash) (*Input, string, uint8, error) {
	var raw, status string
	var code uint8
	err := b.cfg.DB.QueryRow(`SELECT o.input,o.status,CASE
		WHEN o.status='open' AND (o.checked_epoch!=m.value OR typeof(m.value)!='integer') THEN ? ELSE o.code END
		FROM nft_orders o JOIN nft_order_meta m ON m.key='rewind_epoch' WHERE o.hash=?`, CodeUnchecked, hash.Hex()).Scan(&raw, &status, &code)
	if err != nil {
		return nil, "", 0, err
	}
	in := new(Input)
	if err := json.Unmarshal([]byte(raw), in); err != nil {
		return nil, "", 0, err
	}
	return in, status, code, nil
}

// Run follows the exchange's logs and re-checks orders until ctx ends.
func (b *Book) Run(ctx context.Context) error {
	t := time.NewTicker(b.cfg.Poll)
	defer t.Stop()
	for {
		if err := b.step(ctx); err != nil && !errors.Is(err, context.Canceled) {
			b.mu.Lock()
			if msg := err.Error(); msg != b.lastErr {
				b.cfg.Log.Warn("order book", "err", err)
				b.lastErr = msg
			}
			b.mu.Unlock()
		} else {
			b.mu.Lock()
			b.lastErr = ""
			b.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (b *Book) meta(key string) (uint64, error) {
	var v uint64
	err := b.cfg.DB.QueryRow("SELECT value FROM nft_order_meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (b *Book) setMeta(key string, v uint64) error {
	return setBookMeta(b.cfg.DB, key, v)
}

func setBookMeta(q bookSQL, key string, v uint64) error {
	_, err := q.Exec(`INSERT INTO nft_order_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, v)
	return err
}

func (b *Book) step(ctx context.Context) error {
	exchange := b.cfg.Exchange()
	if exchange == (common.Address{}) {
		return nil
	}
	at, err := b.snapshot(ctx)
	if err != nil {
		return err
	}
	key := "scanned:" + exchange.Hex()
	from, err := b.meta(key)
	if err != nil {
		return err
	}
	touched := map[common.Hash]bool{}
	head := at.head.Number.Uint64()
	if from > head {
		return errStaleObservation
	}
	for from < head {
		to := min(from+2_000, head)
		logs, err := b.cfg.Eth.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(from + 1), ToBlock: new(big.Int).SetUint64(to),
			Addresses: []common.Address{exchange},
			Topics:    [][]common.Hash{{topicOrderFilled, topicOrderCancelled, topicNonceIncremented, topicOrderApproved}},
		})
		if err != nil {
			return err
		}
		prepared, err := b.prepareLogs(ctx, exchange, logs, from, to, at.head)
		if err != nil {
			return err
		}
		if err := b.verifyHeader(ctx, at.head); err != nil {
			return err
		}
		if err := func() error {
			tx, err := b.observationTx(ctx, at.epoch)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var cursor uint64
			err = tx.QueryRow("SELECT value FROM nft_order_meta WHERE key=?", key).Scan(&cursor)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if cursor != from {
				return errStaleObservation
			}
			for _, l := range prepared {
				if err := b.apply(tx, exchange, l, touched, at.epoch); err != nil {
					return err
				}
			}
			if err := setBookMeta(tx, key, to); err != nil {
				return err
			}
			return tx.Commit()
		}(); err != nil {
			return err
		}
		from = to
	}

	b.mu.Lock()
	sweep := b.lastSweepEpoch != at.epoch || time.Since(b.lastSweep) >= b.cfg.Recheck
	b.mu.Unlock()
	if err := b.recheck(ctx, exchange, touched, sweep, at); err != nil {
		return err
	}
	if sweep {
		b.mu.Lock()
		b.lastSweep, b.lastSweepEpoch = time.Now(), at.epoch
		b.mu.Unlock()
	}
	return nil
}

type preparedLog struct {
	log      types.Log
	head     *types.Header
	approved *Input
}

// Read and decode all headers before acquiring a writer. Every returned log
// must belong to the requested range, exchange and canonical header observed by
// this reader. The epoch and page cursor are fenced later in one transaction.
func (b *Book) prepareLogs(ctx context.Context, exchange common.Address, logs []types.Log, from, to uint64, head *types.Header) ([]preparedLog, error) {
	headers := map[uint64]*types.Header{head.Number.Uint64(): head}
	out := make([]preparedLog, 0, len(logs))
	for _, l := range logs {
		if l.Removed || l.Address != exchange || l.BlockNumber <= from || l.BlockNumber > to {
			return nil, errors.New("exchange log does not belong to the requested page")
		}
		hdr := headers[l.BlockNumber]
		if hdr == nil {
			var err error
			hdr, err = b.cfg.Eth.HeaderByNumber(ctx, new(big.Int).SetUint64(l.BlockNumber))
			if err != nil {
				return nil, err
			}
			if validHeader(hdr) != nil || hdr.Number.Uint64() != l.BlockNumber {
				return nil, errStaleObservation
			}
			headers[l.BlockNumber] = hdr
		}
		if hdr.Hash() != l.BlockHash {
			return nil, errStaleObservation
		}
		entry := preparedLog{log: l, head: hdr}
		if len(l.Topics) == 3 && l.Topics[0] == topicOrderApproved {
			ev, err := b.cfg.Artifact.ABI.Unpack("OrderApproved", l.Data)
			if err == nil && len(ev) == 1 {
				raw, err := json.Marshal(ev[0])
				if err != nil {
					return nil, err
				}
				var v ABIOrder
				if json.Unmarshal(raw, &v) == nil {
					o := FromABI(v)
					if o.Hash() == l.Topics[1] && o.Check() == nil && common.BytesToAddress(l.Topics[2][:]) == o.Trader {
						entry.approved = &Input{Order: o, Kind: KindOnChain, Signature: []byte{}, Proof: []common.Hash{}}
					}
				}
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (b *Book) apply(tx *sql.Tx, exchange common.Address, entry preparedLog, touched map[common.Hash]bool, epoch uint64) error {
	l := entry.log
	if len(l.Topics) == 0 {
		return nil
	}
	switch l.Topics[0] {
	case topicOrderFilled:
		if len(l.Topics) == 4 {
			touched[l.Topics[1]] = true
		}
	case topicOrderCancelled:
		if len(l.Topics) != 3 {
			return nil
		}
		_, err := tx.Exec(`UPDATE nft_orders SET status = ?, code = ?, closed_block = ? WHERE hash = ? AND status = ? AND exchange=?`,
			StatusCancelled, CodeCancelled, l.BlockNumber, l.Topics[1].Hex(), StatusOpen, exchange.Hex())
		return err
	case topicNonceIncremented:
		if len(l.Topics) != 2 || len(l.Data) != 32 {
			return nil
		}
		trader := common.BytesToAddress(l.Topics[1][:])
		nonce := new(big.Int).SetBytes(l.Data)
		rows, err := tx.Query(`SELECT hash, nonce FROM nft_orders WHERE trader = ? AND status = ? AND exchange=?`, trader.Hex(), StatusOpen, exchange.Hex())
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var h, n string
			if err := rows.Scan(&h, &n); err != nil {
				rows.Close()
				return err
			}
			if v, ok := new(big.Int).SetString(n, 10); ok && v.Cmp(nonce) < 0 {
				stale = append(stale, h)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, h := range stale {
			if _, err := tx.Exec(`UPDATE nft_orders SET status = ?, code = ?, closed_block = ? WHERE hash = ?`,
				StatusCancelled, CodeStaleNonce, l.BlockNumber, h); err != nil {
				return err
			}
		}
	case topicOrderApproved:
		if entry.approved == nil {
			return nil
		}
		at := observation{epoch: epoch, head: entry.head}
		if _, err := b.insert(tx, entry.approved, exchange, check{code: CodeUnchecked}, at, "chain", time.Now().Unix()); err != nil {
			return err
		}
		touched[entry.approved.Order.Hash()] = true
	}
	return nil
}

// recheck asks the exchange about the orders the logs touched and, on a sweep,
// about every open one.
func (b *Book) recheck(ctx context.Context, exchange common.Address, touched map[common.Hash]bool, sweep bool, at observation) error {
	selected := map[string]bool{}
	for h := range touched {
		selected[h.Hex()] = true
	}
	if sweep {
		rows, err := b.cfg.DB.Query(`SELECT hash FROM nft_orders WHERE status = ? AND exchange = ?`, StatusOpen, exchange.Hex())
		if err != nil {
			return err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return err
			}
			selected[h] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	var hashes []string
	for h := range selected {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	var inputs []*Input
	for _, h := range hashes {
		var raw string
		err := b.cfg.DB.QueryRow("SELECT input FROM nft_orders WHERE hash=? AND exchange=?", h, exchange.Hex()).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue // an order this node never had: somebody else's fill
		}
		if err != nil {
			return err
		}
		in := new(Input)
		if err := json.Unmarshal([]byte(raw), in); err != nil {
			return err
		}
		if in.Order.Check() != nil || in.Order.Hash().Hex() != h {
			return errors.New("stored order does not match its original hash")
		}
		inputs = append(inputs, in)
	}
	checks, err := b.check(ctx, exchange, inputs, at.head)
	if err != nil {
		return err
	}
	if err := b.verifyHeader(ctx, at.head); err != nil {
		return err
	}
	tx, err := b.observationTx(ctx, at.epoch)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := guardScanHeight(tx, exchange, at.head); err != nil {
		return err
	}
	now := time.Now().Unix()
	conflict := false
	for i, in := range inputs {
		accepted, err := b.observe(tx, in, exchange, checks[i], at, now)
		if err != nil {
			return err
		}
		conflict = conflict || !accepted
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if conflict {
		return errCheckConflict
	}
	return nil
}

// Rewind reopens what a chain reorganisation undid: orders closed above the
// block are checked again, and the logs are read again from there.
func (b *Book) Rewind(block uint64) error {
	tx, err := b.cfg.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := b.RewindTx(tx, block); err != nil {
		return err
	}
	return tx.Commit()
}

// RewindTx participates in a rewind owned by the index's shared database
// transaction. It changes derived cache markers and cursors, never the original
// order payload or hash. The caller owns commit/rollback of the transaction.
func (b *Book) RewindTx(tx *sql.Tx, block uint64) error {
	if block > math.MaxInt64 {
		return errors.New("unsupported order-book rewind height")
	}
	if err := advanceEpoch(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE nft_orders SET status = ?, closed_block = NULL, checked_at = 0
		WHERE closed_block > ?`, StatusOpen, block); err != nil {
		return err
	}
	if err := invalidateOpenChecks(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE nft_order_meta SET value = ? WHERE key LIKE 'scanned:%' AND value > ?`, block, block)
	return err
}

// Stats is the book at a glance, for /api/nft/book.
func (b *Book) Stats() map[string]any {
	out := map[string]any{}
	rows, err := b.cfg.DB.Query(`SELECT status, side, COUNT(*) FROM nft_orders GROUP BY status, side`)
	if err == nil {
		for rows.Next() {
			var status string
			var side, n int
			if rows.Scan(&status, &side, &n) == nil {
				out[fmt.Sprintf("%s_%s", status, map[int]string{0: "listings", 1: "bids"}[side])] = n
			}
		}
		rows.Close()
	}
	b.mu.Lock()
	if b.lastErr != "" {
		out["lastError"] = b.lastErr
	}
	b.mu.Unlock()
	return out
}
