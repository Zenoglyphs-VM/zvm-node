package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/ethereum/go-ethereum/common"
)

type recoveryQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type persistedIndexHead struct {
	number uint64
	hash   string
}

type indexedBlockMismatch struct {
	number   uint64
	observed common.Hash
	stored   string
}

func (e *indexedBlockMismatch) Error() string {
	return fmt.Sprintf("block %d is %s but the index holds %s", e.number, e.observed, e.stored)
}

func readIndexHead(ctx context.Context, query recoveryQuery) (persistedIndexHead, error) {
	var number int64
	var hash string
	err := query.QueryRowContext(ctx, "SELECT number,hash FROM blocks ORDER BY number DESC LIMIT 1").Scan(&number, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return persistedIndexHead{}, nil
	}
	if err != nil {
		return persistedIndexHead{}, err
	}
	if number < 0 {
		return persistedIndexHead{}, errors.New("invalid persisted index height")
	}
	return persistedIndexHead{number: uint64(number), hash: hash}, nil
}

func (ix *Indexer) checkDerivedAnchor(ctx context.Context, query recoveryQuery, head store.Head) error {
	var number uint64
	var hash string
	err := query.QueryRowContext(ctx, "SELECT l2_number,l2_hash FROM momentums ORDER BY height DESC LIMIT 1").Scan(&number, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		if head.L2Number == 0 && head.L2Hash == ix.cfg.L2GenesisHash {
			return nil
		}
	} else if err != nil {
		return err
	} else if number == head.L2Number && hash == head.L2Hash.Hex() {
		return nil
	}
	return errors.New("derived recovery anchor changed")
}

// ReconcileCanonical repairs an interrupted deriver-to-index notification.
// The caller supplies its accepted local derivation head after Init. Both the
// execution reader and the persisted derivation head must agree with it. This
// is local consistency, not independent NoM authentication or finality proof.
// Network reads occur before the guarded SQL rewind, never under a writer.
func (ix *Indexer) ReconcileCanonical(ctx context.Context, head store.Head) error {
	return ix.reconcileCanonical(ctx, head, true)
}

func (ix *Indexer) reconcileCanonical(ctx context.Context, head store.Head, bindDerived bool) error {
	ix.recoveryMu.Lock()
	defer ix.recoveryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if head.L2Number > math.MaxInt64 || head.L2Hash == (common.Hash{}) {
		return errors.New("canonical recovery requires a bounded head and hash")
	}
	verify := func() error {
		observed, err := ix.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(head.L2Number))
		if err != nil {
			return err
		}
		if observed == nil || observed.Number == nil || observed.Number.Uint64() != head.L2Number || observed.Hash() != head.L2Hash {
			return errors.New("execution recovery anchor differs")
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	if bindDerived {
		if err := ix.checkDerivedAnchor(ctx, ix.db, head); err != nil {
			return err
		}
	}
	before, err := readIndexHead(ctx, ix.db)
	if err != nil {
		return err
	}
	from := min(before.number, head.L2Number)
	// New writes check their stored parent inside the write transaction. An
	// older index's internal links need one scan per instance, not per block.
	if !ix.canonicalLinksChecked {
		var broken sql.NullInt64
		if err := ix.db.QueryRowContext(ctx, `SELECT MIN(b.number) FROM blocks b
			JOIN blocks p ON p.number=b.number-1 WHERE b.parent!=p.hash`).Scan(&broken); err != nil {
			return err
		}
		if broken.Valid {
			if broken.Int64 <= 0 {
				return errors.New("invalid persisted index link")
			}
			from = min(from, uint64(broken.Int64-1))
		}
	}
	fork := from
	for ; fork > 0; fork-- {
		var stored string
		err := ix.db.QueryRowContext(ctx, "SELECT hash FROM blocks WHERE number=?", fork).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		observed, err := ix.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(fork))
		if err != nil {
			return err
		}
		if observed != nil && observed.Number != nil && observed.Number.Uint64() == fork && observed.Hash().Hex() == stored {
			break
		}
	}
	if err := verify(); err != nil {
		return err
	}
	guard := func(tx *sql.Tx) error {
		current, err := readIndexHead(ctx, tx)
		if err != nil {
			return err
		}
		if current != before {
			return errors.New("index recovery observation was superseded")
		}
		if bindDerived {
			return ix.checkDerivedAnchor(ctx, tx, head)
		}
		return ctx.Err()
	}
	if fork < before.number {
		if err := ix.rewindGuarded(fork, guard); err != nil {
			return fmt.Errorf("reconciling canonical index: %w", err)
		}
	} else {
		// Check the index and source together even when no rewind is needed.
		tx, err := ix.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := guard(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	ix.canonicalLinksChecked = true
	return nil
}
