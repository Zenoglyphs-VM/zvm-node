package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// LogRow is one log as the derived tables need it. Live indexing builds it from
// a receipt and a backfill builds it from the logs table, so a table added
// after the chain has history fills the same way it keeps up.
type LogRow struct {
	Tx        string
	Index     uint
	Block     uint64
	BlockHash common.Hash
	Time      uint64
	Address   common.Address
	Topics    []common.Hash
	Data      []byte
	// From is the transaction's sender: a relayed transaction's signer, or the
	// Zenon account behind a native call.
	From common.Address
}

func rowOf(blk *types.Block, tx *types.Transaction, from common.Address, l *types.Log) LogRow {
	return LogRow{
		Tx: tx.Hash().Hex(), Index: l.Index, Block: blk.NumberU64(), BlockHash: blk.Hash(), Time: blk.Time(),
		Address: l.Address, Topics: l.Topics, Data: l.Data, From: from,
	}
}

func (r LogRow) Addr(topic int) common.Address { return common.BytesToAddress(r.Topics[topic][:]) }
func (r LogRow) Num(topic int) *big.Int        { return new(big.Int).SetBytes(r.Topics[topic][:]) }

// Word is the i-th 32-byte word of the data, which the caller has length-checked.
func (r LogRow) Word(i int) *big.Int { return new(big.Int).SetBytes(r.Data[i*32 : (i+1)*32]) }
func (r LogRow) WordAddr(i int) common.Address {
	return common.BytesToAddress(r.Data[i*32 : (i+1)*32])
}

// loadLogs reads indexed logs matching a WHERE clause over `logs l`, in chain order.
func (ix *Indexer) loadLogs(ctx context.Context, where string, args ...any) ([]LogRow, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT l.tx, l.idx, l.block, b.timestamp, l.address,
		l.topic0, l.topic1, l.topic2, l.topic3, l.data, COALESCE(t.from_addr, ''), b.hash
		FROM logs l JOIN blocks b ON b.number = l.block LEFT JOIN txs t ON t.hash = l.tx
		WHERE `+where+` ORDER BY l.block, l.idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var r LogRow
		var addr, from, hash string
		var t0, t1, t2, t3 sql.NullString
		if err := rows.Scan(&r.Tx, &r.Index, &r.Block, &r.Time, &addr, &t0, &t1, &t2, &t3, &r.Data, &from, &hash); err != nil {
			return nil, err
		}
		r.BlockHash = common.HexToHash(hash)
		if len(hash) != 66 || r.BlockHash == (common.Hash{}) || r.BlockHash.Hex() != hash {
			return nil, fmt.Errorf("backfill log has no exact source block hash")
		}
		r.Address = common.HexToAddress(addr)
		r.From = common.HexToAddress(from)
		for _, t := range []sql.NullString{t0, t1, t2, t3} {
			if !t.Valid {
				break
			}
			r.Topics = append(r.Topics, common.HexToHash(t.String))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// backfillHead distinguishes an empty index from an indexed block zero. The
// hash matters when a replacement keeps the same height.
type backfillHead struct {
	present bool
	number  uint64
	hash    string
}

type backfillSourceReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readBackfillHead(ctx context.Context, db backfillSourceReader) (backfillHead, error) {
	var number int64
	var hash string
	err := db.QueryRowContext(ctx, "SELECT number,hash FROM blocks ORDER BY number DESC LIMIT 1").Scan(&number, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return backfillHead{}, nil
	}
	if err != nil {
		return backfillHead{}, err
	}
	decoded := common.HexToHash(hash)
	if number < 0 || len(hash) != 66 || decoded == (common.Hash{}) || decoded.Hex() != hash {
		return backfillHead{}, fmt.Errorf("backfill source head is malformed")
	}
	return backfillHead{present: true, number: uint64(number), hash: hash}, nil
}

// backfillInput is read before the write transaction. It supplies no authority
// to apply stale source rows; applyBackfill rechecks every source block first.
type backfillInput struct {
	head backfillHead
	logs []LogRow
}

func (ix *Indexer) readBackfill(ctx context.Context, key, where string, args []any) (*backfillInput, error) {
	var done int
	if err := ix.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM index_meta WHERE key = ?", key).Scan(&done); err != nil {
		return nil, err
	}
	if done > 0 {
		return nil, nil
	}
	head, err := readBackfillHead(ctx, ix.db)
	if err != nil {
		return nil, err
	}
	// A head that advances during this read must not extend the selected input
	// horizon. Copy the caller's arguments before adding the bound.
	bound := append(append([]any{}, args...), head.number)
	logs, err := ix.loadLogs(ctx, "("+where+") AND l.block <= ?", bound...)
	if err != nil {
		return nil, err
	}
	return &backfillInput{head: head, logs: logs}, nil
}

func (input *backfillInput) verify(ctx context.Context, tx *sql.Tx) error {
	head, err := readBackfillHead(ctx, tx)
	if err != nil {
		return err
	}
	if head != input.head {
		return fmt.Errorf("backfill source head moved before write")
	}
	// loadLogs orders by block and log index. Keep only the preceding block's
	// identity, rather than a map proportional to the complete history.
	var previous uint64
	var previousHash common.Hash
	havePrevious := false
	for _, row := range input.logs {
		if !head.present || row.Block > head.number || row.BlockHash == (common.Hash{}) {
			return fmt.Errorf("backfill log has no bound source block")
		}
		if havePrevious && row.Block < previous {
			return fmt.Errorf("backfill source rows are not in block order")
		}
		if havePrevious && row.Block == previous {
			if row.BlockHash != previousHash {
				return fmt.Errorf("backfill source rows disagree on a block hash")
			}
			continue
		}
		var stored string
		if err := tx.QueryRowContext(ctx, "SELECT hash FROM blocks WHERE number=?", row.Block).Scan(&stored); err != nil {
			return fmt.Errorf("backfill source block %d is unavailable: %w", row.Block, err)
		}
		if stored != row.BlockHash.Hex() {
			return fmt.Errorf("backfill source block %d moved before write", row.Block)
		}
		previous, previousHash, havePrevious = row.Block, row.BlockHash, true
	}
	return nil
}

// backfill replays already-indexed logs into a table that did not exist when
// they were indexed, once per database. The source read and its application
// are separate so their boundary can be checked without weakening atomicity.
func (ix *Indexer) backfill(ctx context.Context, key, where string, args []any, handle func(context.Context, *sql.Tx, LogRow) error) error {
	input, err := ix.readBackfill(ctx, key, where, args)
	if err != nil {
		return err
	}
	return ix.applyBackfill(ctx, key, input, handle)
}

func (ix *Indexer) applyBackfill(ctx context.Context, key string, input *backfillInput, handle func(context.Context, *sql.Tx, LogRow) error) error {
	if input == nil {
		return nil
	}
	dbtx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(dbtx)
	defer rollback()
	// Another pass may have completed this key while its source was read.
	// Do not replay old history over cache rows updated by newer live blocks.
	var done int
	if err := dbtx.QueryRowContext(ctx, "SELECT COUNT(*) FROM index_meta WHERE key = ?", key).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return commit()
	}
	// Validate the complete input before any handler, including handlers that
	// update existing ownership without making a URI observation. A refusal
	// must not consume URI RPC work or publish a completed-job marker.
	if err := input.verify(ctx, dbtx); err != nil {
		return err
	}
	for _, row := range input.logs {
		if err := handle(ctx, dbtx, row); err != nil {
			return fmt.Errorf("block %d log %d: %w", row.Block, row.Index, err)
		}
	}
	if err := ix.afterBlock(dbtx, input.head.number); err != nil {
		return err
	}
	if _, err := dbtx.Exec("INSERT OR REPLACE INTO index_meta (key, value) VALUES (?, ?)", key, input.head.number); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	ix.log.Info("backfilled from indexed logs", "job", key, "logs", len(input.logs), "head", input.head.number)
	return nil
}

func TopicList(topics []common.Hash) (string, []any) {
	args := make([]any, len(topics))
	for i, t := range topics {
		args[i] = t.Hex()
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(topics)), ","), args
}

// floatUnits is a raw amount as a float with decimals applied, for prices and
// sorting — never for anything that is paid.
func FloatUnits(x *big.Int, decimals int) float64 {
	if x == nil || x.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Float).SetInt(x).Float64()
	return f / math.Pow10(decimals)
}
