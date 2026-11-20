package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-node/node"
)

func TestEpochReportsLoadedDeriverConfiguration(t *testing.T) {
	const chain69LoadedSHA256 = "02936999a8ad1448d00e6341dd03a375cc37f9060e73b10884024a649b114949"
	dir := t.TempDir()
	source, err := os.ReadFile("../zvm-core/chains/l2-69/rollup.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollup.json")
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := rollup.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	displayConfig := *cfg
	displayConfig.GasLimit++
	srv := New(Config{Rollup: &displayConfig, Deriver: &node.Deriver{Cfg: cfg}, DB: db})
	if err := os.WriteFile(path, []byte("changed on disk after startup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/epoch", nil))
	var got loadedEpoch
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" ||
		json.Unmarshal(recorder.Body.Bytes(), &got) != nil {
		t.Fatal("loaded epoch endpoint did not return the expected uncached observation")
	}
	want := loadedEpoch{
		Schema: 1, L1ChainID: "69", L2ChainID: "7340469", L2GenesisHash: cfg.L2GenesisHash.Hex(),
		LoadedRollupSHA256: chain69LoadedSHA256,
	}
	if got != want {
		t.Fatalf("unexpected loaded epoch: %+v", got)
	}
}

func TestEpochIsUnavailableWithoutLoadedConfiguration(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).epoch(w, httptest.NewRequest(http.MethodGet, "/api/epoch", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("empty startup state received an epoch observation")
	}
}
