// Package nodecmd is `zvm-node node` as a function, so that a binary which is a
// node plus something else does not have to copy three hundred lines of flags
// and dialling to add it.
//
// An Extender registers its own flags before parsing and is handed the dialled
// clients afterwards; what it returns is wired into the same indexer and the
// same HTTP server as the node's own. zvm-nft is the reason this exists.
package nodecmd

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/ethrpc"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/service"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/node"
	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

func Run(args []string, ext ...Extender) error {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	configPath := fs.String("config", "/config/rollup.json", "rollup config")
	jwtPath := fs.String("jwt", "/config/jwt.hex", "Engine API secret")
	l1URL := fs.String("l1", "http://l1-rpc:35997", "NoM node JSON-RPC")
	engineURL := fs.String("engine", "http://op-geth:8551", "op-geth Engine API")
	ethURL := fs.String("eth", "http://op-geth:8545", "op-geth JSON-RPC")
	dbPath := fs.String("db", "/data/zvm.db", "SQLite database")
	listen := fs.String("listen", ":8080", "explorer API and UI listen address ('' to disable)")
	uiDir := fs.String("ui", "", "a built static bundle to serve at / (e.g. a zvm-explorer dist); empty serves the API alone")
	faucetURL := fs.String("faucet", "", "faucet service base URL, proxied at /api/faucet")
	bridgeURL := fs.String("bridge", "", "bridge service base URL")
	relayerURL := fs.String("relayer", "", "relayer base URL")
	rpcDeny := fs.String("rpc-deny", "", "comma-separated method prefixes /rpc refuses, e.g. debug_,txpool_,admin_ (for a node on a public hostname)")
	publicEth := fs.String("public-eth", "", "op-geth URL to advertise to wallets (default: /rpc on this server)")
	confirmations := fs.Uint64("confirmations", 0, "momentums to stay behind the NoM frontier")
	stallAfter := fs.Duration("stall-after", node.DefaultStallAfter, "how long the head may stand still, while NoM has momentums this node has not derived, before /healthz reports it stalled")
	noIndex := fs.Bool("no-index", false, "derive only: no index, no API")
	dexPath := fs.String("dex", "", "Uniswap V2 deployment written by `zvm bench setup`, offered to the swap page (optional)")
	volumes := fs.String("volumes", "", "other services' data directories to size on /ops, as name=path,… (mount them read-only)")
	l1Explorer := fs.String("l1-explorer", "", "a Zenon explorer for this L1, linked from every transaction's NoM block (default: explorer.zenon.network on chain 1)")
	mainnetRPC := fs.String("mainnet-rpc", os.Getenv("NOM_MAINNET_NODE"), "a Zenon mainnet node to compare this chain's load and storage with; empty or 'off' for none (default $NOM_MAINNET_NODE)")
	mainnetDisk := fs.Int64("mainnet-disk-bytes", 0, "a mainnet node's data directory size, as measured by the operator; no RPC reports it")
	mainnetDiskSource := fs.String("mainnet-disk-source", "", "where -mainnet-disk-bytes was measured, shown beside it")
	verifyOn := fs.Bool("verify", true, "publish contract sources: recompile submissions with solc and match them against the chain")
	solcDir := fs.String("solc-dir", "", "where fetched solc releases are kept (default: a solc directory beside -db)")
	solcURL := fs.String("solc-url", "https://binaries.soliditylang.org", "where solc releases and their checksums are fetched from")
	verifierURL := fs.String("verifier", "", "a sandboxed `zvm solcd` to compile in, so this node never runs solc itself")
	verifierToken := fs.String("verifier-token-file", "", "shared secret for -verifier")
	for _, e := range ext {
		e.Flags(fs)
	}
	fs.Parse(args)
	started := time.Now()

	opsVolumes, err := api.ParseVolumes(*volumes)
	if err != nil {
		return err
	}
	// -media-dir is the market extension's flag, but /api/ops sizes it beside
	// the node's own data, as it did before the market became an extension.
	var mediaDir string
	if f := fs.Lookup("media-dir"); f != nil {
		mediaDir = f.Value.String()
	}
	var mainnet *znn.Client
	if *mainnetRPC != "" && !strings.EqualFold(*mainnetRPC, "off") {
		mainnet = znn.NewClient(*mainnetRPC)
	}

	cfg, err := rollup.Load(*configPath)
	if err != nil {
		return err
	}
	secret, err := l2.ReadJWTSecret(*jwtPath)
	if err != nil {
		return err
	}
	ctx, cancel := service.SignalContext()
	defer cancel()

	eng, err := service.WaitFor(ctx, "op-geth engine", func() (*l2.Engine, error) { return l2.DialEngine(ctx, *engineURL, secret) })
	if err != nil {
		return err
	}
	eth, err := service.WaitFor(ctx, "op-geth rpc", func() (*ethclient.Client, error) {
		c, err := ethrpc.Dial(ctx, *ethURL)
		if err != nil {
			return nil, err
		}
		if _, err := c.ChainID(ctx); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	l1 := znn.NewClient(*l1URL)
	d := &node.Deriver{
		Cfg: cfg, L1: l1, Engine: eng, Eth: eth, DB: db,
		Log: slog.Default().With("svc", "deriver"), Confirmations: *confirmations,
		StallAfter: *stallAfter,
	}

	// Extenders are built before the index, because what they return decides
	// which tables it has.
	var plugins []indexer.Plugin
	var extensions []api.Extension
	var rewinds []Result
	var startExtensions func() error
	var followIndex func()
	for _, e := range ext {
		r, err := e.Build(ctx, Env{Rollup: cfg, DB: db, Eth: eth, L1: l1, Dex: *dexPath, Log: slog.Default()})
		if err != nil {
			return err
		}
		plugins = append(plugins, r.Plugins...)
		extensions = append(extensions, r.Extensions...)
		if r.OnRewindTx != nil || r.OnRewind != nil || r.Start != nil {
			rewinds = append(rewinds, r)
		}
	}

	if !*noIndex {
		ix, err := indexer.New(db, eth, cfg, slog.Default().With("svc", "indexer"), plugins...)
		if err != nil {
			return err
		}

		// Extensions rewind with the index, not with the deriver: the index also
		// rewinds itself when it finds an orphan block, and state kept outside
		// it — an order book's fills — has to go back all the same.
		startExtensions, err = PrepareExtensions(ctx, ix, rewinds)
		if err != nil {
			return err
		}
		d.OnRewind = func(l2Number uint64) {
			// A failure here is repaired later: the index then finds the
			// orphan by its parent hash and rewinds itself.
			if err := ix.Rewind(l2Number); err != nil {
				slog.Error("rewinding the index", "err", err)
			}
		}
		// Startup recovery must finish before a follower can change the index.
		followIndex = func() { ix.Follow(ctx, func() uint64 { return d.Head().L2Number }) }

		var verifier *verify.Service
		if *verifyOn {
			dir := *solcDir
			if dir == "" {
				dir = filepath.Join(filepath.Dir(*dbPath), "solc")
			}
			var runner verify.Runner
			if *verifierURL != "" {
				token := ""
				if *verifierToken != "" {
					raw, err := os.ReadFile(*verifierToken)
					if err != nil {
						return err
					}
					token = strings.TrimSpace(string(raw))
				}
				runner = &verify.RemoteRunner{URL: *verifierURL, Token: token}
				slog.Info("compiling in the verifier service", "url", *verifierURL)
			}
			verifier, err = verify.New(verify.Config{
				DB: db.SQL(), Eth: eth, Compilers: &verify.Compilers{Dir: dir, BaseURL: *solcURL},
				Runner: runner, Log: slog.Default().With("svc", "verify"),
			})
			if err != nil {
				return err
			}
			go verifier.Run(ctx)
		}

		if *listen != "" {
			srv := api.New(api.Config{
				Rollup: cfg, DB: db, Eth: eth, L1: l1, Deriver: d,
				EthURL: *ethURL, L1URL: *l1URL, Faucet: *faucetURL, Bridge: *bridgeURL, Relayer: *relayerURL, Dex: *dexPath, RPCDeny: splitList(*rpcDeny),
				L1Explorer: l1ExplorerFor(*l1Explorer, cfg.L1ChainID),
				Verify:     verifier,
				Extensions: extensions,
				Ops: api.OpsConfig{
					DBPath: *dbPath, MediaDir: mediaDir, Volumes: opsVolumes, Mainnet: mainnet, StartedAt: started,
					MainnetDisk: api.MainnetDisk{Bytes: *mainnetDisk, Source: *mainnetDiskSource},
				},
				UI: uiHandler(*uiDir), Log: slog.Default().With("svc", "api"),
			})
			go srv.RunOps(ctx)
			// A whole request, not just its headers, has to arrive in time: a
			// body sent a byte at a time held a connection and its goroutine
			// for as long as the sender liked. Two minutes is a 16 MB
			// verification input or a 10 MB upload on a slow link.
			httpSrv := &http.Server{
				Addr: *listen, Handler: srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute,
				WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute,
			}
			go func() {
				<-ctx.Done()
				c, done := context.WithTimeout(context.Background(), 5*time.Second)
				defer done()
				httpSrv.Shutdown(c)
			}()
			go func() {
				slog.Info("explorer listening", "addr", *listen, "publicEth", *publicEth)
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("explorer http", "err", err)
					cancel()
				}
			}()
		}
	}

	if _, err := service.WaitFor(ctx, "L1 node", func() (struct{}, error) { return struct{}{}, d.Init(ctx) }); err != nil {
		return err
	}
	if startExtensions != nil {
		if err := startExtensions(); err != nil {
			return err
		}
	}
	if followIndex != nil {
		go followIndex()
	}
	return d.Run(ctx)
}

// l1ExplorerFor is the Zenon explorer this chain's NoM blocks are linked to.
// Only mainnet has a default; a devnet's blocks are nowhere public.
func l1ExplorerFor(flag string, chainID uint64) string {
	if flag != "" {
		return strings.TrimRight(flag, "/")
	}
	if chainID == 1 {
		return "https://explorer.zenon.network"
	}
	return ""
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// uiHandler serves a built static bundle — zvm-explorer's dist, or any other
// single-page app — at /. The monorepo embedded one UI into the binary; a node
// that is only a node should not have to be rebuilt to change what it serves,
// and the pages that were embedded now ship as their own repos.
func uiHandler(dir string) http.Handler {
	if dir == "" {
		return nil
	}
	files := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.FromSlash(path.Clean("/"+r.URL.Path)))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		// Any other path is a client-side route, not a 404.
		http.ServeFile(w, r, index)
	})
}
