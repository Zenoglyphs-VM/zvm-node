package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// PreparationSource only lends readers to a plugin's discovery phase. All
// discovery writes belong in the plugin's separate, disposable preparation DB.
type PreparationSource interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// LogReplay visits the complete selected input in its original chain order.
// A capacity or iterator failure must refuse the whole job, never a suffix.
type LogReplay func(func(LogRow) error) error

// PreparingPlugin is optional. CollectLogs discovers the exact external reads
// without making them. Resolve runs after the source snapshot is closed, before
// the canonical write transaction starts. Handle consumes only those responses.
type PreparingPlugin interface {
	CollectLogs(context.Context, PreparationSource, LogReplay) (PreparedLogs, error)
}

type PreparedLogs interface {
	Resolve(context.Context) error
	Context(context.Context) context.Context
	Complete() error
	Close() error
}

type preparedInputs struct {
	items  []PreparedLogs
	closed bool
}

func collectInputs(ctx context.Context, source PreparationSource, replay LogReplay, plugins []Plugin) (*preparedInputs, error) {
	inputs := &preparedInputs{}
	for _, plugin := range plugins {
		preparer, ok := plugin.(PreparingPlugin)
		if !ok {
			continue
		}
		item, err := preparer.CollectLogs(ctx, source, replay)
		if err != nil {
			if item != nil {
				item.Close()
			}
			inputs.Close()
			return nil, fmt.Errorf("preparing %s inputs: %w", plugin.Name(), err)
		}
		if item == nil {
			inputs.Close()
			return nil, fmt.Errorf("preparing %s returned no complete input", plugin.Name())
		}
		inputs.items = append(inputs.items, item)
	}
	return inputs, nil
}

func (inputs *preparedInputs) resolve(ctx context.Context) error {
	if inputs.closed {
		return errors.New("prepared inputs are already closed")
	}
	for _, item := range inputs.items {
		if err := item.Resolve(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (inputs *preparedInputs) context(ctx context.Context) (context.Context, error) {
	if inputs.closed {
		return nil, errors.New("prepared inputs are already closed")
	}
	for _, item := range inputs.items {
		ctx = item.Context(ctx)
	}
	return ctx, nil
}

func (inputs *preparedInputs) finish() error {
	if inputs.closed {
		return errors.New("prepared inputs are already closed")
	}
	for _, item := range inputs.items {
		if err := item.Complete(); err != nil {
			return err
		}
	}
	return inputs.Close()
}

func (inputs *preparedInputs) Close() error {
	if inputs == nil || inputs.closed {
		return nil
	}
	var err error
	inputs.closed = true
	for _, item := range inputs.items {
		err = errors.Join(err, item.Close())
	}
	inputs.items = nil
	return err
}

type preparedBlock struct {
	head   backfillHead
	inputs *preparedInputs
}

// collectBlock closes the main read transaction before issuing any URI or
// collection-name calls. The writer rechecks this exact source head afterward.
func (ix *Indexer) collectBlock(ctx context.Context, block *types.Block, receipts types.Receipts) (*preparedBlock, error) {
	needed := false
	for _, plugin := range ix.plugins {
		if _, ok := plugin.(PreparingPlugin); ok {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	source, err := ix.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer source.Rollback()
	head, err := readBackfillHead(ctx, source)
	if err != nil {
		return nil, err
	}
	var existing string
	err = source.QueryRowContext(ctx, "SELECT hash FROM blocks WHERE number=?", block.NumberU64()).Scan(&existing)
	if err == nil {
		if existing != block.Hash().Hex() {
			return nil, &indexedBlockMismatch{number: block.NumberU64(), observed: block.Hash(), stored: existing}
		}
		// The writer retains its idempotence check. No reads are needed for a
		// block already indexed, even when another pass then advances the head.
		return nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	replay := func(visit func(LogRow) error) error {
		for i, txn := range block.Transactions() {
			from := common.Address{}
			if txn.Type() == types.DepositTxType {
				from = txn.From()
			} else if sender, err := types.Sender(ix.signer, txn); err == nil {
				from = sender
			}
			for _, log := range receipts[i].Logs {
				if err := ctx.Err(); err != nil {
					return err
				}
				if len(log.Topics) != 0 {
					if err := visit(rowOf(block, txn, from, log)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	inputs, err := collectInputs(ctx, source, replay, ix.plugins)
	if err != nil {
		return nil, err
	}
	if err := source.Commit(); err != nil {
		inputs.Close()
		return nil, err
	}
	if err := inputs.resolve(ctx); err != nil {
		inputs.Close()
		return nil, err
	}
	return &preparedBlock{head: head, inputs: inputs}, nil
}

func (block *preparedBlock) verify(ctx context.Context, tx *sql.Tx) error {
	head, err := readBackfillHead(ctx, tx)
	if err != nil {
		return err
	}
	if head != block.head {
		return errors.New("prepared block source head moved before write")
	}
	return nil
}
