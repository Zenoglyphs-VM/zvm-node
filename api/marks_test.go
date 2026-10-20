package api

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// The marks cache is keyed by any address anyone asks about. Expired entries
// have to go, or a script asking about fresh addresses grows it forever.
func TestExpiredMarksAreDropped(t *testing.T) {
	s := &Server{}
	now := time.Now()
	for i := 0; i < 1000; i++ {
		s.marks.Store(common.BigToAddress(new(big.Int).Lsh(big.NewInt(1), uint(i%150))), markEntry{at: now.Add(-2 * markTTL)})
	}
	fresh := common.HexToAddress("0xf")
	s.marks.Store(fresh, markEntry{at: now})
	s.sweepMarks(now)
	n := 0
	s.marks.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Errorf("after a sweep %d marks are left, want only the fresh one", n)
	}
	// At most once a TTL: a sweep right after does nothing, which keeps a busy
	// page from walking the map on every request.
	s.marks.Store(common.HexToAddress("0xe"), markEntry{at: now.Add(-2 * markTTL)})
	s.sweepMarks(now.Add(time.Second))
	if _, ok := s.marks.Load(common.HexToAddress("0xe")); !ok {
		t.Error("a second sweep within the TTL ran")
	}
}
