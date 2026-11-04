package orderbook

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// A retained execution suffix is not an accepted local derivation anchor.
// These read-only controls exercise the bound, refusals and moving anchor;
// the actual CLI/index/book rewind schedule remains a separate native gate.
func TestBookAcceptedHeadObservations(t *testing.T) {
	for _, name := range []string{"accepted", "legacy-default", "hash-mismatch", "unavailable", "uninitialized", "unsupported-height", "moved"} {
		t.Run(name, func(t *testing.T) {
			b, p, db, _ := recoveryBook(t)
			accepted := types.CopyHeader(p.header)
			accepted.Number, accepted.Time = big.NewInt(1), 1010
			p.headers = map[uint64]*types.Header{1: accepted}
			number, hash := uint64(1), accepted.Hash()
			wantError := false
			switch name {
			case "hash-mismatch":
				hash, wantError = common.Hash{31: 9}, true
			case "unavailable":
				delete(p.headers, 1)
				wantError = true
			case "uninitialized":
				number, hash, wantError = 0, common.Hash{}, true
			case "unsupported-height":
				number, wantError = uint64(1)<<63, true
			}
			if name != "legacy-default" {
				b.cfg.CanonicalHead = func() (uint64, common.Hash) { return number, hash }
			}
			before := recoveryBookState(t, db)
			at, err := b.snapshot(context.Background())
			if wantError {
				if err == nil {
					t.Fatal("invalid accepted anchor produced an observation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want := accepted
				if name == "legacy-default" {
					want = p.header
				}
				if at.head.Number.Cmp(want.Number) != 0 || at.head.Hash() != want.Hash() {
					t.Fatal("observation consumed a header outside its accepted context")
				}
				if name == "moved" {
					number, hash = p.header.Number.Uint64(), p.header.Hash()
				}
				err = b.verifyHeader(context.Background(), at.head)
				if name == "moved" && err == nil {
					t.Fatal("a moved accepted anchor retained an in-flight observation")
				}
				if name != "moved" && err != nil {
					t.Fatal(err)
				}
			}
			if after := recoveryBookState(t, db); after != before {
				t.Fatal("read-only accepted-anchor controls changed original order state")
			}
		})
	}
}
