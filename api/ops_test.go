package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

func TestParseVolumes(t *testing.T) {
	got, err := ParseVolumes(" op-geth=/volumes/op-geth, nom-l1=/volumes/nom-l1 ,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Volume{"op-geth", "/volumes/op-geth"}) || got[1] != (Volume{"nom-l1", "/volumes/nom-l1"}) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"op-geth", "=/x", "x=", "index=/data"} {
		if _, err := ParseVolumes(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestWindowOf(t *testing.T) {
	m := func(height, ts uint64, data ...[]byte) *znn.DetailedMomentum {
		dm := &znn.DetailedMomentum{Momentum: &znn.Momentum{Height: height, Timestamp: ts}}
		for _, d := range data {
			dm.Blocks = append(dm.Blocks, &znn.AccountBlock{Data: d})
		}
		return dm
	}
	w := windowOf([]*znn.DetailedMomentum{
		m(10, 1000),
		m(11, 1010, []byte("abc"), nil),
		m(12, 1020, make([]byte, 7)),
	})
	want := nomWindow{Momentums: 3, From: 10, To: 12, Seconds: 20, Blocks: 3, DataBytes: 10, EmptyMomentums: 1}
	if *w != want {
		t.Fatalf("got %+v, want %+v", *w, want)
	}
	if w := windowOf(nil); w.Momentums != 0 || w.Seconds != 0 {
		t.Fatalf("empty: %+v", *w)
	}
}

func TestMeasureDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 50), 0o644)
	u := measureDir("x", dir)
	if u.Error != "" || u.Files != 2 || u.Bytes != 150 {
		t.Fatalf("got %+v", u)
	}
	if u := measureDir("missing", filepath.Join(dir, "nope")); u.Error == "" {
		t.Fatal("a missing directory should say so")
	}
}

func TestHostOnly(t *testing.T) {
	if got := hostOnly("https://node.example:35997/secret-key"); got != "https://node.example:35997" {
		t.Fatalf("got %q", got)
	}
}
