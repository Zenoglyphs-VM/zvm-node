package orderbook

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// These are local cache errors, not checkOrder return codes.
var (
	errStaleObservation = errors.New("order-book observation was superseded; retry against the current chain")
	errCheckConflict    = errors.New("conflicting order-book observation; a fresh rewind and check are required")
)

type bookSQL interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// A check is tied to one header and the persisted rewind generation captured
// before the first RPC. Neither a later latest height nor an in-memory counter
// can establish that a response still belongs to this database state.
type observation struct {
	epoch uint64
	head  *types.Header
}

type hashCaller interface {
	CallContractAtHash(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error)
}

func initObservations(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	rows, err := tx.Query("PRAGMA table_info(nft_orders)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, required, primary int
		var name, kind string
		var value any
		if err := rows.Scan(&cid, &name, &kind, &required, &value, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, col := range []struct{ name, definition string }{
		{"checked_epoch", "INTEGER NOT NULL DEFAULT -1"},
		{"checked_block", "INTEGER NOT NULL DEFAULT 0"},
		{"checked_hash", "TEXT NOT NULL DEFAULT ''"},
		{"filled_epoch", "INTEGER NOT NULL DEFAULT -1"},
	} {
		if !columns[col.name] {
			if _, err := tx.Exec("ALTER TABLE nft_orders ADD COLUMN " + col.name + " " + col.definition); err != nil {
				return err
			}
		}
	}
	for _, statement := range closureIndexes {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO nft_order_meta(key,value) VALUES('rewind_epoch',0) ON CONFLICT(key) DO NOTHING"); err != nil {
		return err
	}
	// A restarted book must check open rows again. Advancing the durable epoch
	// also fences an older Book instance sharing this database.
	if err := advanceEpoch(tx); err != nil {
		return err
	}
	if err := invalidateOpenChecks(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func advanceEpoch(tx *sql.Tx) error {
	r, err := tx.Exec(`UPDATE nft_order_meta SET value=value+1
		WHERE key='rewind_epoch' AND typeof(value)='integer' AND value>=0 AND value<?`, int64(math.MaxInt64))
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("invalid or exhausted order-book rewind epoch")
	}
	return nil
}

func invalidateOpenChecks(tx *sql.Tx) error {
	_, err := tx.Exec("UPDATE nft_orders SET code=?,checked_at=0 WHERE status=?", CodeUnchecked, StatusOpen)
	return err
}

func bookEpoch(q bookSQL) (uint64, error) {
	var epoch uint64
	err := q.QueryRow("SELECT value FROM nft_order_meta WHERE key='rewind_epoch' AND typeof(value)='integer' AND value>=0").Scan(&epoch)
	return epoch, err
}

func (b *Book) snapshot(ctx context.Context) (observation, error) {
	epoch, err := bookEpoch(b.cfg.DB)
	if err != nil {
		return observation{}, err
	}
	var number *big.Int
	var acceptedHash common.Hash
	if b.cfg.CanonicalHead != nil {
		accepted, hash := b.cfg.CanonicalHead()
		if accepted > math.MaxInt64 || hash == (common.Hash{}) {
			return observation{}, errStaleObservation
		}
		number, acceptedHash = new(big.Int).SetUint64(accepted), hash
	}
	head, err := b.cfg.Eth.HeaderByNumber(ctx, number)
	if err != nil {
		return observation{}, err
	}
	if err := validHeader(head); err != nil {
		return observation{}, err
	}
	if number != nil && (head.Number.Cmp(number) != 0 || head.Hash() != acceptedHash) {
		return observation{}, errStaleObservation
	}
	return observation{epoch: epoch, head: head}, nil
}

func validHeader(head *types.Header) error {
	if head == nil || head.Number == nil || head.Number.Sign() < 0 || !head.Number.IsInt64() || head.Time > math.MaxInt64 {
		return errors.New("unsupported order-book observation header")
	}
	return nil
}

// This checks the configured reader's canonical header after the read. It is a
// consistency check on that reader, not independent chain/finality proof. No
// RPC runs while the local write transaction is held; an explicit rewind wins
// through the epoch guard inside that transaction.
func (b *Book) verifyHeader(ctx context.Context, want *types.Header) error {
	head, err := b.cfg.Eth.HeaderByNumber(ctx, want.Number)
	if err != nil {
		return err
	}
	if validHeader(head) != nil || head.Number.Cmp(want.Number) != 0 || head.Hash() != want.Hash() {
		return errStaleObservation
	}
	if b.cfg.CanonicalHead != nil {
		number, hash := b.cfg.CanonicalHead()
		if number != want.Number.Uint64() || hash != want.Hash() {
			return errStaleObservation
		}
	}
	return nil
}

func (b *Book) observationTx(ctx context.Context, epoch uint64) (*sql.Tx, error) {
	tx, err := b.cfg.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Acquire the SQLite writer before inspecting or changing derived rows.
	// RewindTx changes this same row in the index's transaction.
	r, err := tx.Exec(`UPDATE nft_order_meta SET value=value
		WHERE key='rewind_epoch' AND typeof(value)='integer' AND value=?`, epoch)
	if err == nil {
		var n int64
		n, err = r.RowsAffected()
		if err == nil && n != 1 {
			err = errStaleObservation
		}
	}
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func guardScanHeight(tx *sql.Tx, exchange common.Address, head *types.Header) error {
	var cursor uint64
	err := tx.QueryRow("SELECT value FROM nft_order_meta WHERE key=?", "scanned:"+exchange.Hex()).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if cursor > head.Number.Uint64() {
		return errStaleObservation
	}
	return nil
}

func storedAmount(in *Input) (int64, error) {
	if in == nil || in.Order.Amount == nil || !in.Order.Amount.Big().IsInt64() || in.Order.Amount.Big().Sign() <= 0 {
		return 0, errors.New("order amount exceeds this node's supported storage range")
	}
	return in.Order.Amount.Big().Int64(), nil
}

// observe replaces a fill cache only when the contract actually reports the
// remaining amount. Early failures (signature, nonce, time, cancellation) return
// zero without reporting fills, so they must not turn a partial fill into a
// full one. A fill from an older epoch may decrease after a rewind.
func (b *Book) observe(tx *sql.Tx, in *Input, exchange common.Address, c check, at observation, now int64) (bool, error) {
	return b.observeFill(tx, in, exchange, c, at, now, nil)
}

// A complete fill counter is supplied only by the paired hash-selected closure proof.
func (b *Book) observeFill(tx *sql.Tx, in *Input, exchange common.Address, c check, at observation, now int64, complete *int64) (bool, error) {
	var filled, fillEpoch, checkEpoch, block int64
	var hash string
	var code uint8
	err := tx.QueryRow(`SELECT filled,filled_epoch,checked_epoch,checked_block,checked_hash,code
		FROM nft_orders WHERE hash=? AND exchange=?`, in.Order.Hash().Hex(), exchange.Hex()).Scan(&filled, &fillEpoch, &checkEpoch, &block, &hash, &code)
	if err != nil {
		return false, err
	}
	if checkEpoch == int64(at.epoch) && block > at.head.Number.Int64() {
		return false, errStaleObservation
	}
	amount, err := storedAmount(in)
	if err != nil {
		return false, err
	}
	reportsFill := complete != nil || c.code == CodeOK || c.code == CodeFilled || c.code == CodeNotHeld || c.code == CodeNotFunded
	next := filled
	if complete != nil {
		if *complete < 0 || *complete > amount {
			return false, errors.New("complete fill proof exceeds the stored amount")
		}
		next = *complete
	} else if reportsFill {
		next = amount - int64(c.remaining)
	}
	conflict := reportsFill && fillEpoch == int64(at.epoch) && next < filled
	if checkEpoch == int64(at.epoch) && block == at.head.Number.Int64() && hash != "" {
		conflict = conflict || hash != at.head.Hash().Hex() || (code != CodeUnchecked && code != c.code)
		conflict = conflict || (reportsFill && fillEpoch == int64(at.epoch) && next != filled)
	}
	if conflict {
		// Preserve the previous fill and its provenance, but stop advertising a
		// contradictory cached answer as fillable. Do not invent a rewind.
		_, err := tx.Exec("UPDATE nft_orders SET code=?,checked_at=0 WHERE hash=?", CodeUnchecked, in.Order.Hash().Hex())
		return false, err
	}
	if reportsFill {
		fillEpoch = int64(at.epoch)
	} else {
		// Retain the cached number, but do not present an early refusal as a full-fill proof.
		fillEpoch = -1
	}
	status := statusFor(c.code)
	var closed any
	if status != StatusOpen {
		closed = at.head.Number.Int64()
	}
	_, err = tx.Exec(`UPDATE nft_orders SET status=?,code=?,filled=?,filled_epoch=?,checked_at=?,
		checked_epoch=?,checked_block=?,checked_hash=?,closed_block=? WHERE hash=? AND exchange=?`,
		status, c.code, next, fillEpoch, now, at.epoch, at.head.Number.Int64(), at.head.Hash().Hex(), closed, in.Order.Hash().Hex(), exchange.Hex())
	return err == nil, err
}
