package node

import (
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
)

// A node reports itself stalled only when it is *both* behind NoM and no longer
// catching up. The two halves matter separately: a node syncing a long history
// is behind and perfectly healthy, and a node level with the frontier has
// nothing to derive, which is an idle chain's normal state. Only the
// combination means derivation has stopped while there was work to do
// (docs/HALT.md).
func TestStalledNeedsBothBehindAndNotMoving(t *testing.T) {
	d := &Deriver{
		Cfg:        &rollup.Config{L1StartHeight: 2, FinalityDepth: 6},
		StallAfter: 30 * time.Second,
	}
	d.setHead(store.Head{Height: 100, L2Number: 99})

	long := func() { d.lastProgress.Store(time.Now().Add(-time.Hour).UnixNano()) }
	just := func() { d.lastProgress.Store(time.Now().UnixNano()) }

	t.Run("level with the frontier is never stalled", func(t *testing.T) {
		d.l1Frontier.Store(100)
		long()
		if s := d.Status(); s.Stalled {
			t.Errorf("stalled with lag %d: an idle chain produces no momentums to derive", s.LagMomentums)
		}
	})

	t.Run("behind but still moving is not stalled", func(t *testing.T) {
		d.l1Frontier.Store(6000)
		just()
		if s := d.Status(); s.Stalled {
			t.Errorf("stalled while catching up: lag %d, last progress %d ms ago", s.LagMomentums, s.SinceProgressMS)
		}
	})

	t.Run("behind and not moving is stalled", func(t *testing.T) {
		d.l1Frontier.Store(6000)
		long()
		s := d.Status()
		if !s.Stalled {
			t.Errorf("not stalled: lag %d, last progress %d ms ago, threshold %s", s.LagMomentums, s.SinceProgressMS, d.stallAfter())
		}
		if s.SinceProgressMS < time.Minute.Milliseconds() {
			t.Errorf("sinceProgressMs is %d, want at least a minute", s.SinceProgressMS)
		}
	})

	t.Run("deriving a block clears it", func(t *testing.T) {
		d.l1Frontier.Store(6000)
		long()
		if !d.Status().Stalled {
			t.Fatal("expected a stalled node to start from")
		}
		d.setHead(store.Head{Height: 101, L2Number: 100})
		if s := d.Status(); s.Stalled {
			t.Error("still stalled after the head moved")
		}
	})
}

// An unstarted deriver has never moved its head, and must not read as having
// been stalled since the zero time.
func TestUnstartedDeriverIsNotInstantlyStalled(t *testing.T) {
	d := &Deriver{Cfg: &rollup.Config{L1StartHeight: 2, FinalityDepth: 6}}
	d.l1Frontier.Store(6000)
	s := d.Status()
	if s.Stalled {
		t.Error("a deriver that has not started yet reports itself stalled")
	}
	if s.SinceProgressMS > time.Minute.Milliseconds() {
		t.Errorf("sinceProgressMs is %d, want the clock to start at first observation", s.SinceProgressMS)
	}
}

func TestStallAfterDefaults(t *testing.T) {
	if got := (&Deriver{}).stallAfter(); got != DefaultStallAfter {
		t.Errorf("stallAfter() = %s, want %s", got, DefaultStallAfter)
	}
	if got := (&Deriver{StallAfter: time.Second}).stallAfter(); got != time.Second {
		t.Errorf("stallAfter() = %s, want the configured 1s", got)
	}
}
