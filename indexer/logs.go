package indexer

import (
	"context"
	"database/sql"
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

// backfill replays already-indexed logs into a table that did not exist when
// they were indexed, once per database: the key in index_meta records that it
// ran. Handlers are idempotent, so a replay over rows the live path already
// wrote converges on the same state.
func (ix *Indexer) backfill(ctx context.Context, key, where string, args []any, handle func(context.Context, *sql.Tx, LogRow) error) error {
	var done int
	if err := ix.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM index_meta WHERE key = ?", key).Scan(&done); err != nil {
		return err
	}
	if done > 0 {
		return nil
	}
	head, err := ix.Head()
	if err != nil {
		return err
	}
	logs, err := ix.loadLogs(ctx, where, args...)
	if err != nil {
		return err
	}
	dbtx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	commit, rollback := ix.guardTransaction(dbtx)
	defer rollback()
	for _, r := range logs {
		if err := handle(ctx, dbtx, r); err != nil {
			return fmt.Errorf("block %d log %d: %w", r.Block, r.Index, err)
		}
	}
	if err := ix.afterBlock(dbtx, head); err != nil {
		return err
	}
	if _, err := dbtx.Exec("INSERT OR REPLACE INTO index_meta (key, value) VALUES (?, ?)", key, head); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	ix.log.Info("backfilled from indexed logs", "job", key, "logs", len(logs), "head", head)
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
