package api

import (
	"encoding/json"
	"os"

	"github.com/ethereum/go-ethereum/common"
)

// Dex is the part of a `zvm bench setup` deployment the swap page needs: a
// Uniswap V2 factory and router, the wrapped-ZNN contract they route through,
// and the tokens worth listing first.
type Dex struct {
	Factory common.Address   `json:"factory"`
	Router  common.Address   `json:"router"`
	WETH    common.Address   `json:"weth"`
	Tokens  []common.Address `json:"tokens"`
}

func (s *Server) dex() *Dex { return ReadDex(s.cfg.Dex, s.cfg.Rollup.L2ChainID) }

// ReadDex reads the deployment on every request rather than once at startup, so
// running the setup against a live node makes the swap page appear without a
// restart. A file for another chain, or one naming no router, is ignored. A
// file left behind by a chain that has since been reset names the right chain
// and the wrong contracts; the page catches that by checking the router has
// code before offering it.
//
// It is exported because the markets extension prices the same pools this
// names, and two readers of one file is two chances to disagree about which
// router is live.
func ReadDex(path string, chainID uint64) *Dex {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		ChainID uint64         `json:"chainId"`
		Factory common.Address `json:"uniswapFactory"`
		Router  common.Address `json:"uniswapRouter"`
		WETH    common.Address `json:"weth"`
		TokenA  common.Address `json:"erc20"`
		TokenB  common.Address `json:"tokenB"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.ChainID != chainID || f.Router == (common.Address{}) {
		return nil
	}
	d := &Dex{Factory: f.Factory, Router: f.Router, WETH: f.WETH, Tokens: []common.Address{}}
	for _, t := range []common.Address{f.TokenA, f.TokenB} {
		if t != (common.Address{}) {
			d.Tokens = append(d.Tokens, t)
		}
	}
	return d
}
