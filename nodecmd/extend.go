package nodecmd

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"sync"

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
	// CanonicalHead is the initialized local derivation anchor. A retained
	// execution suffix may be ahead of it after an interrupted rewind.
	CanonicalHead func() store.Head
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
	// Start launches a worker and returns immediately. Fallible local setup
	// belongs in Build or the shared transaction, before any worker starts.
	// The worker must honor ctx. PrepareExtensions provides the start barrier.
	Start func(ctx context.Context)
	// OnRewindTx rewinds extension state in the shared Env.DB transaction.
	// It uses only the provided transaction for local SQL and must not commit
	// it. Returning an error prevents the index and extension from diverging.
	OnRewindTx func(tx *sql.Tx, l2Number uint64) error
	// OnRewind is a post-commit notification for state outside the shared DB.
	OnRewind func(l2Number uint64)
}

// BindRewind installs both transaction participants and post-commit notices.
func (r Result) BindRewind(ix *indexer.Indexer) {
	if r.OnRewindTx != nil {
		ix.BeforeRewindCommit(r.OnRewindTx)
	}
	if r.OnRewind != nil {
		ix.AfterRewind(r.OnRewind)
	}
}

// PrepareExtensions binds shared-database participants and reconciles them with
// the persisted index in one transaction. A failure returns no start function.
// The caller keeps the index follower idle, prepares extensions before exposing
// their routes, and calls start only after node initialization is accepted.
// The returned function launches each worker at most once.
func PrepareExtensions(ctx context.Context, ix *indexer.Indexer, results []Result, canonical ...func() store.Head) (start func() error, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ix == nil {
		return nil, errors.New("extension recovery requires an initialized index")
	}
	if len(canonical) > 1 || (len(canonical) == 1 && canonical[0] == nil) {
		return nil, errors.New("extension recovery requires one canonical head source")
	}
	prepared := append([]Result(nil), results...)
	for _, result := range prepared {
		result.BindRewind(ix)
	}
	if err := ix.ReconcileExtensionState(ctx); err != nil {
		return nil, err
	}
	var startMu sync.Mutex
	started := false
	return func() error {
		startMu.Lock()
		defer startMu.Unlock()
		if started {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(canonical) == 1 {
			if err := ix.ReconcileCanonical(ctx, canonical[0]()); err != nil {
				return err
			}
		}
		for _, result := range prepared {
			if result.Start != nil {
				result.Start(ctx)
			}
		}
		started = true
		return nil
	}, nil
}
