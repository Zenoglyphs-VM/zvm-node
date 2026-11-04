package nft

import (
	"database/sql"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

// Recover permissions from the same retained logs the live path consumes.
// A current cached approval is not evidence that it survived the rewind.
// Window queries return only the newest event per key; no history copy or
// contract/metadata call is needed, and the enclosing transaction owns writes.
func restoreTokenApprovals(tx *sql.Tx) error {
	if _, err := tx.Exec("UPDATE nft_tokens SET approved = NULL"); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT address, topic2, topic3, block, idx FROM (
		SELECT address, topic2, topic3, block, idx,
			ROW_NUMBER() OVER (PARTITION BY address, topic3 ORDER BY block DESC, idx DESC) AS position
		FROM logs WHERE topic0 = ? AND topic1 IS NOT NULL AND topic2 IS NOT NULL AND topic3 IS NOT NULL
			AND EXISTS (SELECT 1 FROM nft_tokens t WHERE t.collection = logs.address)
	) WHERE position = 1`, topicApproval.Hex())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var collection, spender, id string
		var block, index uint64
		if err := rows.Scan(&collection, &spender, &id, &block, &index); err != nil {
			return err
		}
		tokenID := common.HexToHash(id).Big().String()
		var transferBlock, transferIndex uint64
		var owner string
		err := tx.QueryRow(`SELECT t.block, t.idx, t.to_addr FROM token_transfers t
			JOIN logs l ON l.tx = t.tx AND l.idx = t.idx AND l.block = t.block AND l.address = t.token
			WHERE t.token = ? AND t.token_id = ? AND l.topic0 = ? AND l.topic3 IS NOT NULL
			ORDER BY t.block DESC, t.idx DESC LIMIT 1`, collection, tokenID, topicTransfer.Hex()).Scan(&transferBlock, &transferIndex, &owner)
		if errors.Is(err, sql.ErrNoRows) {
			continue // Without retained ownership history the approval is unknown.
		}
		if err != nil {
			return err
		}
		if block < transferBlock || (block == transferBlock && index <= transferIndex) {
			continue // A later transfer cleared this approval.
		}
		if _, err := tx.Exec(`UPDATE nft_tokens SET approved = ? WHERE collection = ? AND token_id = ? AND owner = ?`,
			common.HexToAddress(spender).Hex(), collection, tokenID, owner); err != nil {
			return err
		}
	}
	return rows.Err()
}

func restoreOperatorApprovals(tx *sql.Tx) error {
	if _, err := tx.Exec("DELETE FROM nft_approvals"); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT address, topic1, topic2, data, block FROM (
		SELECT address, topic1, topic2, data, block,
			ROW_NUMBER() OVER (PARTITION BY address, substr(topic1,-40), substr(topic2,-40) ORDER BY block DESC, idx DESC) AS position
		FROM logs WHERE topic0 = ? AND topic1 IS NOT NULL AND topic2 IS NOT NULL AND topic3 IS NULL AND length(data) = 32
	) WHERE position = 1`, topicApprovalForAll.Hex())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var collection, owner, operator string
		var data []byte
		var block uint64
		if err := rows.Scan(&collection, &owner, &operator, &data, &block); err != nil {
			return err
		}
		approved := 0
		for _, b := range data {
			if b != 0 {
				approved = 1
				break
			}
		}
		if _, err := tx.Exec(`INSERT INTO nft_approvals (owner, collection, operator, approved, block) VALUES (?,?,?,?,?)`,
			common.HexToAddress(owner).Hex(), collection, common.HexToAddress(operator).Hex(), approved, block); err != nil {
			return err
		}
	}
	return rows.Err()
}
