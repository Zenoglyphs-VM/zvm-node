package nft

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Keep the original lexicographic selection. Canonical unsigned decimals can
// be rejected by exact length/text comparisons before crossing the SQL driver.
// Legacy spellings and non-text storage still reach the existing big.Int parser;
// filtering them with an integer/REAL cast would change historical behavior.
// This original predicate remains the fallback for signed, reversed or
// oversized internal bounds. Unsigned event words use metadataRangeQuery below.
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

// Use the same exact expression for the index and its query constraints.
const metadataCanonicalLength = `CASE WHEN typeof(token_id)='text' AND (token_id='0' OR
    (token_id GLOB '[1-9]*' AND token_id NOT GLOB '*[^0-9]*'
     AND length(token_id)=length(CAST(token_id AS BLOB))))
    THEN length(token_id) ELSE -1 END`

const metadataRangeIndexSchema = "CREATE INDEX IF NOT EXISTS nft_tokens_metadata_range ON nft_tokens(collection,(" + metadataCanonicalLength + "),token_id);"

// metadataRangeQuery merges one sorted seek per decimal width with the legacy
// stream. INDEXED BY is an index requirement, not a planner hint: an absent or
// unusable index refuses the operation rather than silently scanning a width.
// No SQL LIMIT is applied before the unchanged Go parser counts matching IDs.
// Only the declared unsigned 256-bit event bounds use this route. Other internal
// bounds retain the original predicate and signed/parser behavior.
func metadataRangeQuery(collection common.Address, from, to *big.Int, includeBlocks bool) (string, []any) {
	columns := "token_id"
	if includeBlocks {
		columns = "token_id,minted_block,updated_block"
	}
	if from.Sign() < 0 || to.Sign() < 0 || from.Cmp(to) > 0 || from.BitLen() > 256 || to.BitLen() > 256 {
		return "SELECT " + columns + " FROM nft_tokens" + metadataRangeWhere, metadataRangeArgs(collection, from, to)
	}
	lower, upper := from.String(), to.String()
	parts := make([]string, 0, len(upper)-len(lower)+2)
	args := make([]any, 0, 4*(len(upper)-len(lower)+1)+1)
	prefix := "SELECT " + columns + " FROM nft_tokens INDEXED BY nft_tokens_metadata_range WHERE collection=? AND " + metadataCanonicalLength
	for width := len(lower); width <= len(upper); width++ {
		left := lower
		if width > len(lower) {
			left = "1" + strings.Repeat("0", width-1)
		}
		right := strings.Repeat("9", width)
		if width == len(upper) {
			right = upper
		}
		parts = append(parts, prefix+"=? AND token_id>=? AND token_id<=?")
		args = append(args, collection.Hex(), width, left, right)
	}
	parts = append(parts, prefix+"=-1")
	args = append(args, collection.Hex())
	return strings.Join(parts, " UNION ALL ") + " ORDER BY token_id", args
}
