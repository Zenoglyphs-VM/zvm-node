package nft

import (
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

// Keep the original lexicographic selection. Canonical unsigned decimals can
// be rejected by exact length/text comparisons before crossing the SQL driver.
// Legacy spellings and non-text storage still reach the existing big.Int parser;
// filtering them with an integer/REAL cast would change historical behavior.
// This uses the existing collection index, not an indexed numeric-range seek.
const metadataRangeWhere = ` WHERE collection=? AND (
    NOT (typeof(token_id)='text' AND (token_id='0' OR
        (token_id GLOB '[1-9]*' AND token_id NOT GLOB '*[^0-9]*'
         AND length(token_id)=length(CAST(token_id AS BLOB)))))
    OR ?
    OR (length(token_id)>=? AND length(token_id)<=?
        AND (length(token_id)>? OR token_id>=?)
        AND (length(token_id)<? OR token_id<=?))
    ) ORDER BY token_id`

func metadataRangeArgs(collection common.Address, from, to *big.Int) []any {
	lower, upper := from.String(), to.String()
	// Current event words are unsigned; preserve the old helper semantics if
	// another internal caller supplies a signed bound in the future.
	signed := from.Sign() < 0 || to.Sign() < 0
	return []any{collection.Hex(), signed, len(lower), len(upper), len(lower), lower, len(upper), upper}
}
