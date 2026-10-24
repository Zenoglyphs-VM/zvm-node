package nft

import (
	"database/sql"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

type tokenUpdate struct {
	id       *big.Int
	minted   uint64
	updated  uint64
	previous uint64
}

// Restore the height from the events that remain, using the same transfer,
// single-update and bounded batch-update rules as the live path. Restoring an
// owner alone leaves an orphan's updated_block behind. Metadata payload values
// are a separate concern: the retained event does not contain a previous URI.
func restoreTokenUpdateHeights(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT collection,token_id,minted_block,updated_block FROM nft_tokens ORDER BY collection,token_id")
	if err != nil {
		return err
	}
	tokens := map[string]map[string]*tokenUpdate{}
	for rows.Next() {
		var collection, id string
		var minted, previous uint64
		if err := rows.Scan(&collection, &id, &minted, &previous); err != nil {
			rows.Close()
			return err
		}
		value, ok := new(big.Int).SetString(id, 10)
		if !ok || value.Sign() < 0 || value.BitLen() > 256 || value.String() != id {
			rows.Close()
			return fmt.Errorf("invalid retained NFT token ID")
		}
		if tokens[collection] == nil {
			tokens[collection] = map[string]*tokenUpdate{}
		}
		tokens[collection][id] = &tokenUpdate{id: value, minted: minted, updated: minted, previous: previous}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(tokens) == 0 {
		return nil
	}
	// ORDER BY token_id in refreshRange defines which 5,000 matching tokens
	// the resource bound allows. Exclude tokens first seen after each event.
	ordered := map[string][]string{}
	for collection, entries := range tokens {
		for id := range entries {
			ordered[collection] = append(ordered[collection], id)
		}
		sort.Strings(ordered[collection])
	}
	rows, err = tx.Query(`SELECT address,topic0,topic3,data,block FROM logs
		WHERE topic0 IN (?,?,?) AND EXISTS (SELECT 1 FROM nft_tokens t WHERE t.collection=logs.address)
		ORDER BY block,idx`, topicTransfer.Hex(), topicMetadataUpdate.Hex(), topicBatchMetadataUpdate.Hex())
	if err != nil {
		return err
	}
	for rows.Next() {
		var collection, topic string
		var id sql.NullString
		var data []byte
		var block uint64
		if err := rows.Scan(&collection, &topic, &id, &data, &block); err != nil {
			rows.Close()
			return err
		}
		mark := func(key string) {
			if token := tokens[collection][key]; token != nil && token.minted <= block {
				token.updated = max(token.updated, block)
			}
		}
		switch {
		case topic == topicTransfer.Hex() && id.Valid:
			mark(common.HexToHash(id.String).Big().String())
		case topic == topicMetadataUpdate.Hex() && len(data) == 32:
			mark(new(big.Int).SetBytes(data).String())
		case topic == topicBatchMetadataUpdate.Hex() && len(data) == 64:
			from, to := new(big.Int).SetBytes(data[:32]), new(big.Int).SetBytes(data[32:])
			matched := 0
			for _, key := range ordered[collection] {
				token := tokens[collection][key]
				if token.minted <= block && token.id.Cmp(from) >= 0 && token.id.Cmp(to) <= 0 {
					if matched == 5_000 {
						break
					}
					mark(key)
					matched++
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	update, err := tx.Prepare("UPDATE nft_tokens SET updated_block=? WHERE collection=? AND token_id=? AND updated_block=?")
	if err != nil {
		return err
	}
	defer update.Close()
	for collection, entries := range tokens {
		for id, token := range entries {
			if token.previous == token.updated {
				continue
			}
			if _, err := update.Exec(token.updated, collection, id, token.previous); err != nil {
				return err
			}
		}
	}
	return nil
}
