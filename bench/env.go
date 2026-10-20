package bench

import (
	"context"
	"flag"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/cli"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// Env is what every command that drives load against a running chain needs:
// both layers, somewhere to submit through, and the node's API for per-block
// NoM cost. It began as an unexported struct in cmd/zvm, shared by the
// benchmark and the application setup; those are separate programs now, and a
// second copy of these flag names and defaults is a second chain to point them
// at by accident.
type Env struct {
	Cfg       *rollup.Config
	Eth       *ethclient.Client
	Submit    *ethclient.Client
	SubmitURL string
	L1        *znn.Client
	API       string
}

// Flags registers the five endpoints on fs and returns a func that dials them
// once the flags are parsed. Each defaults to its environment variable (see
// zvm-core/cli). Submit defaults to the relayer; an empty -submit posts to the
// read endpoint instead, which is what a native-lane run wants.
func Flags(fs *flag.FlagSet) func() (*Env, error) {
	configPath := fs.String("config", "l2/rollup.json", "rollup config")
	ethURL := cli.Endpoint(fs, "eth", cli.EnvZVM, "ZVM JSON-RPC to read from")
	submitURL := cli.Endpoint(fs, "submit", cli.EnvRelayer, "where signed transactions go (the relayer)")
	l1URL := cli.Endpoint(fs, "l1", cli.EnvNoM, "NoM node JSON-RPC")
	apiBase := cli.Endpoint(fs, "api", cli.EnvAPI, "ZVM node read API, for per-block NoM cost")
	return func() (*Env, error) {
		if err := cli.NeedEndpoints(fs, "eth", "l1", "api"); err != nil {
			return nil, err
		}
		cfg, err := rollup.Load(*configPath)
		if err != nil {
			return nil, err
		}
		eth, err := DialPooled(context.Background(), *ethURL)
		if err != nil {
			return nil, err
		}
		submit := eth
		if *submitURL != "" {
			if submit, err = DialPooled(context.Background(), *submitURL); err != nil {
				return nil, err
			}
		}
		return &Env{Cfg: cfg, Eth: eth, Submit: submit, SubmitURL: *submitURL, L1: znn.NewClient(*l1URL), API: *apiBase}, nil
	}
}
