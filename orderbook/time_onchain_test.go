package orderbook

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// Exercise the other book inlet: a real approveOrders transaction and its
// Solidity log, rather than an HTTP submission or a direct SQL order insert.
func TestBookImportsFullWidthOnChainOrderTimes(t *testing.T) {
	c := newSimChain(t)
	b, db := c.book()
	order := &Order{Trader: crypto.PubkeyToAddress(c.seller.PublicKey), Side: SideSell, Collection: c.nft, TokenID: U(1), Amount: U(1),
		Price: U(17), ListingTime: math.MaxInt64 + 1, ExpirationTime: math.MaxUint64, Nonce: U(0), Salt: U(704)}
	c.must(c.call(c.seller, c.exchange, "ZVMExchange", "approveOrders", nil, ABIOrders([]*Order{order})), "approve full-width order")
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	in, status, code, err := b.Get(order.Hash())
	if err != nil || in.Kind != KindOnChain || in.Order.ListingTime != order.ListingTime ||
		in.Order.ExpirationTime != order.ExpirationTime || status != StatusOpen || code != CodeNotStarted {
		t.Fatal("the real approval log lost its uint64 times or skipped the executed not-started check")
	}
	var opens, expires StoredTime
	var openKind, expiryKind, original string
	if err := db.QueryRow("SELECT listing_time,expiration_time,typeof(listing_time),typeof(expiration_time),input FROM nft_orders WHERE hash=?",
		order.Hash().Hex()).Scan(&opens, &expires, &openKind, &expiryKind, &original); err != nil {
		t.Fatal(err)
	}
	if uint64(opens) != order.ListingTime || uint64(expires) != order.ExpirationTime || openKind != "text" || expiryKind != "text" {
		t.Fatal("on-chain order time storage became signed or floating point")
	}
	if err := b.Rewind(0); err != nil {
		t.Fatal(err)
	}
	if err := b.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after string
	var count int
	if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", order.Hash().Hex()).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM nft_orders").Scan(&count); err != nil || count != 1 || after != original {
		t.Fatal("a follower re-read replaced or duplicated the original full-width input")
	}
	raw, err := json.Marshal(map[string]any{"pass": true, "actualSolidityApprovalLog": true, "listingTime": TimeJSON(order.ListingTime),
		"expirationTime": TimeJSON(order.ExpirationTime), "code": CodeNotStarted, "originalInputPreserved": true,
		"manualSQLOrderInserts": 0, "isolatedSimulatedEVM": true, "publicNetwork": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("MARKET-TIME-ONCHAIN-QUALIFICATION " + string(raw))
}
