package nodecmd

import (
	"context"
	"flag"
	"log/slog"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"

	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
)

// An Extender is something the node carries that is not the chain: a
// marketplace, an application's own index. It gets its flags onto the node's
// flag set, and is built once the clients are dialled and before the index is
// opened — because what it returns decides which tables that index has.
type Extender interface {
	// Flags is called before parsing, on the node's own flag set. Names must
	// not collide with the node's.
	Flags(fs *flag.FlagSet)
	// Build is called once, after the flags are parsed and the clients are up.
	// Returning an error stops the node from starting, which is the right
	// outcome for a misconfigured extension: half a market is worse than none.
	Build(ctx context.Context, env Env) (Result, error)
}

// Env is what the node has already done by the time an Extender is built.
type Env struct {
	Rollup *rollup.Config
	DB     *store.DB
	Eth    *ethclient.Client
	L1     *znn.Client
	// Dex is the node's -dex deployment file, or empty. It is handed over
	// rather than re-declared as an extension's own flag because the node
	// already offers it on /api/info: two flags naming the same file is two
	// answers to which router is live.
	Dex string
	Log *slog.Logger
}

// Result is what an Extender adds to the node.
type Result struct {
	// Plugins index extra tables from the same logs, in the same transaction.
	Plugins []indexer.Plugin
	// Extensions add routes and a slice of /api/info.
	Extensions []api.Extension
	// OnRewind is called after the index has rewound, for state an extension
	// keeps outside it — an order book, a cache.
	OnRewind func(l2Number uint64)
}
