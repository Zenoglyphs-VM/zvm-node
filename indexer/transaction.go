package indexer

import "database/sql"

// guardTransaction keeps derived caches bound to committed SQL state. A handler
// can warm them with rows that the enclosing transaction later rolls back, so
// every failed write must discard those observations before the next write.
// Successful commits retain the caches for the normal indexing path.
func (ix *Indexer) guardTransaction(tx *sql.Tx) (commit func() error, rollback func()) {
	committed := false
	return func() error {
			err := tx.Commit()
			committed = err == nil
			return err
		}, func() {
			tx.Rollback()
			if !committed {
				ix.forgetPairs()
				ix.candles.reset()
			}
		}
}
