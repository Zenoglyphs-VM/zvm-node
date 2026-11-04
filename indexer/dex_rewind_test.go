package indexer

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestDexRewindMatchesSurvivingCandleHistory(t *testing.T) {
	for _, cut := range []uint64{0, 1, 2} {
		t.Run(string(rune('0'+cut)), func(t *testing.T) {
			f := n04Dex()
			f.block(1, 2, true)
			f.block(2, 5, false)
			f.block(3, 7, false)
			ix, db, _ := n04Index(t, f.chain, &n04Plugin{})
			n04DexIndex(t, ix, 1, 3)
			if err := ix.Rewind(cut); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := n04Index(t, f.chain, &n04Plugin{})
			if cut > 0 {
				n04DexIndex(t, reference, 1, cut)
			}
			n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
		})
	}
}

func TestDexRewindPreservesOtherPairs(t *testing.T) {
	f := n04Dex()
	f.block(1, 2, true)
	other := f
	other.pair = common.Address{19: 15}
	f.chain.answer(other.pair, "factory()", common.LeftPadBytes(f.factory[:], 32))
	other.block(2, 3, true)
	f.block(3, 7, false)
	ix, db, _ := n04Index(t, f.chain, &n04Plugin{})
	n04DexIndex(t, ix, 1, 3)
	if err := ix.Rewind(2); err != nil {
		t.Fatal(err)
	}
	reference, referenceDB, _ := n04Index(t, f.chain, &n04Plugin{})
	n04DexIndex(t, reference, 1, 2)
	n04IndexSame(t, n04IndexState(t, db), n04IndexState(t, referenceDB))
}
