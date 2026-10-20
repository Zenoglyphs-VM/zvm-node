package nft

import (
	"database/sql"
	"fmt"
)

// zeroAddress is the burn address a token leaves the supply through, and the
// one a mint arrives from. The chain's own index has the same constant.
const zeroAddress = "0x0000000000000000000000000000000000000000"

func (x *Index) markCollection(addr string) {
	if addr == "" {
		return
	}
	x.mu.Lock()
	if x.dirty == nil {
		x.dirty = map[string]bool{}
	}
	x.dirty[addr] = true
	x.mu.Unlock()
}

func (x *Index) takeDirty() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.dirty) == 0 {
		return nil
	}
	out := make([]string, 0, len(x.dirty))
	for a := range x.dirty {
		out = append(out, a)
	}
	x.dirty = map[string]bool{}
	return out
}

func (x *Index) refreshCollectionStats(dbtx *sql.Tx, collection string, block uint64) error {
	_, err := dbtx.Exec(`INSERT INTO nft_collection_stats (collection, items, owners, sales, volume, updated_block)
		VALUES (
			?,
			(SELECT COUNT(*) FROM nft_tokens t WHERE t.collection = ?1 AND t.owner != ?2),
			(SELECT COUNT(DISTINCT t.owner) FROM nft_tokens t WHERE t.collection = ?1 AND t.owner != ?2),
			(SELECT COUNT(*) FROM nft_sales s WHERE s.collection = ?1),
			(SELECT COALESCE(SUM(s.price_num),0) FROM nft_sales s WHERE s.collection = ?1 AND s.currency = ?2),
			?3)
		ON CONFLICT(collection) DO UPDATE SET items = excluded.items, owners = excluded.owners,
			sales = excluded.sales, volume = excluded.volume, updated_block = excluded.updated_block`,
		collection, zeroAddress, block)
	return err
}

func (x *Index) refreshDirtyCollections(dbtx *sql.Tx, block uint64) error {
	for _, c := range x.takeDirty() {
		if err := x.refreshCollectionStats(dbtx, c, block); err != nil {
			return fmt.Errorf("collection stats for %s: %w", c, err)
		}
	}
	return nil
}

func (x *Index) rebuildCollectionStats(dbtx *sql.Tx, block uint64) error {
	rows, err := dbtx.Query(`SELECT address FROM nft_collections`)
	if err != nil {
		return err
	}
	var addrs []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		addrs = append(addrs, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range addrs {
		if err := x.refreshCollectionStats(dbtx, a, block); err != nil {
			return err
		}
	}
	return nil
}
