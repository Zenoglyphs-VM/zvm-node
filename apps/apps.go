// Package apps is the ZVM's application layer: an NFT exchange for signed
// orders with a collection factory, and a liquidity farm. The contracts live in
// contracts/apps; `zvm apps setup` deploys them and writes the deployment file
// this package reads, which the node offers to the page and to the indexer.
package apps

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

// Compiled with solc 0.8.30 (--optimize --optimize-runs 200 --evm-version
// prague) from contracts/apps, against the OpenZeppelin v5 the benchmark uses.
//
//go:embed artifacts/apps.json
var combined []byte

// LoadArtifacts is the benchmark's contracts — Uniswap V2 and the OpenZeppelin
// tokens — with the application contracts added.
func LoadArtifacts() (*bench.Artifacts, error) {
	arts, err := bench.LoadArtifacts()
	if err != nil {
		return nil, err
	}
	if err := arts.AddCombined(combined); err != nil {
		return nil, fmt.Errorf("app artifacts: %w", err)
	}
	return arts, nil
}

// Deployment is what `zvm apps setup` leaves behind.
type Deployment struct {
	ChainID  uint64         `json:"chainId"`
	Deployer common.Address `json:"deployer"`
	// Exchange settles signed orders (D28); the order book itself is the
	// node's, not the chain's.
	Exchange common.Address `json:"exchange"`
	// WrappedZNN is what bids are paid in: nothing is escrowed, so a bid is an
	// allowance on an ERC-20, as OpenSea's offers are on WETH.
	WrappedZNN        common.Address `json:"wrappedZnn,omitempty"`
	CollectionFactory common.Address `json:"collectionFactory"`
	CollectionImpl    common.Address `json:"collectionImplementation"`
	Farm              common.Address `json:"farm"`
	RewardToken       common.Address `json:"rewardToken"`
	// FeeStrategy is the Uniswap V2 feeTo: it turns 0.05% of every swap into farm
	// budget. Absent on a chain seeded before it existed, or one with no swap.
	FeeStrategy common.Address `json:"feeStrategy,omitempty"`
	// ZNNQSRPair is the Uniswap pool of wrapped ZNN against QSR the setup opens
	// at the DexScreener ratio, so QSR trades on the ZVM against ZNN.
	ZNNQSRPair  common.Address   `json:"znnQsrPair,omitempty"`
	Collections []common.Address `json:"collections,omitempty"`
	MediaURL    string           `json:"mediaUrl,omitempty"`
	Deployed    time.Time        `json:"deployedAt"`

	// The recipe these addresses were computed from (D32). With them, anyone
	// can recompute every address above from this repository's artifacts and
	// check that this file names the contracts it claims to.
	Owner      common.Address    `json:"owner,omitempty"`
	RewardRate string            `json:"rewardRate,omitempty"`
	Salts      map[string]string `json:"salts,omitempty"`
}

func Load(path string) (*Deployment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d := new(Deployment)
	return d, json.Unmarshal(raw, d)
}

func (d *Deployment) Save(path string) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// File reads a deployment file on demand and re-reads it only when it changes,
// so running the setup against a live node needs no restart. A file for
// another chain reads as no deployment.
type File struct {
	Path    string
	ChainID uint64

	mu     sync.Mutex
	mod    time.Time
	size   int64
	cached *Deployment
}

func (f *File) Get() *Deployment {
	if f == nil || f.Path == "" {
		return nil
	}
	st, err := os.Stat(f.Path)
	if err != nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if st.ModTime().Equal(f.mod) && st.Size() == f.size {
		return f.cached
	}
	f.mod, f.size = st.ModTime(), st.Size()
	f.cached = nil
	if d, err := Load(f.Path); err == nil && d.ChainID == f.ChainID {
		f.cached = d
	}
	return f.cached
}
