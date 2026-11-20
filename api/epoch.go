package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Zenoglyphs-VM/zvm-node/node"
)

type loadedEpoch struct {
	Schema             int    `json:"schema"`
	L1ChainID          string `json:"l1ChainId"`
	L2ChainID          string `json:"l2ChainId"`
	L2GenesisHash      string `json:"l2GenesisHash"`
	LoadedRollupSHA256 string `json:"loadedRollupSha256"`
}

// Capture the configuration supplied to the deriver at startup. A request
// serves this snapshot without opening files or consulting upstream clients.
func captureLoadedEpoch(deriver *node.Deriver) loadedEpoch {
	if deriver == nil || deriver.Cfg == nil {
		return loadedEpoch{}
	}
	cfg := deriver.Cfg
	data, err := json.Marshal(cfg)
	if err != nil {
		return loadedEpoch{}
	}
	sum := sha256.Sum256(data)
	return loadedEpoch{
		Schema: 1, L1ChainID: strconv.FormatUint(cfg.L1ChainID, 10), L2ChainID: strconv.FormatUint(cfg.L2ChainID, 10),
		L2GenesisHash: cfg.L2GenesisHash.Hex(), LoadedRollupSHA256: hex.EncodeToString(sum[:]),
	}
}

func (s *Server) epoch(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.loadedEpoch.Schema == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "loaded rollup configuration is unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, s.loadedEpoch)
}
