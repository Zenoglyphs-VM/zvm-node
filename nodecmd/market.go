package nodecmd

import (
	"context"
	"flag"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/indexer/nft"
	"github.com/Zenoglyphs-VM/zvm-node/market"
	"github.com/Zenoglyphs-VM/zvm-node/media"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// Market is the NFT market, as an Extender.
//
// It is built through the same seams an out-of-tree extension would use —
// indexer.Plugin and api.Extension — even though it ships in this binary. That
// is not ceremony: it is what keeps indexer.go and api.go free of collections,
// orders and media, so the chain's own index stays readable, and what gives the
// NFT tables their own rewind and rebuild contract instead of four more lines
// in the node's.
//
// The plugin is registered unconditionally, so the tables exist on every node.
// Without -apps it simply has no exchange or factory to trust events from.
type Market struct {
	appsPath *string
	mediaDir *string
	mediaURL *string
	mediaCDN *bool
}

func (m *Market) Flags(fs *flag.FlagSet) {
	m.appsPath = fs.String("apps", "", "application deployment written by `zvm-node apps setup`: exchange, collections, farm")
	m.mediaDir = fs.String("media-dir", "", "where to store NFT images and metadata (empty: this node stores none)")
	m.mediaURL = fs.String("media-url", "", "the public base URL those objects are addressed by, e.g. https://nft.example.org")
	m.mediaCDN = fs.Bool("media-cdn", false, "media-url is live in front of this node, so pages should load objects from it")
}

func (m *Market) Build(ctx context.Context, env Env) (Result, error) {
	var zero Result
	// The deployment file is read on every use, not once here, so running
	// `apps setup` against a live node makes the market appear without a
	// restart.
	appsFile := &apps.File{Path: *m.appsPath, ChainID: env.Rollup.L2ChainID}

	var store *media.Store
	if *m.mediaDir != "" {
		s, err := media.Open(media.Config{
			Dir: *m.mediaDir, PublicURL: *m.mediaURL, CDN: *m.mediaCDN, ChainID: env.Rollup.L2ChainID,
			Log: slog.Default().With("svc", "media"),
		}, env.DB.SQL())
		if err != nil {
			return zero, err
		}
		store = s
		slog.Info("media store", "dir", *m.mediaDir, "publicUrl", store.PublicBase(), "loadFrom", store.LoadBase())
	}

	arts, err := apps.LoadArtifacts()
	if err != nil {
		return zero, err
	}
	// The order book (D28): signatures this node was handed, kept apart from
	// the index because the chain cannot give them back.
	var canonicalHead func() (uint64, common.Hash)
	if env.CanonicalHead != nil {
		canonicalHead = func() (uint64, common.Hash) {
			head := env.CanonicalHead()
			return head.L2Number, head.L2Hash
		}
	}
	book, err := orderbook.New(orderbook.Config{
		CanonicalHead: canonicalHead,
		DB:            env.DB.SQL(), Eth: env.Eth, ChainID: env.Rollup.L2ChainID, Artifact: arts.MustGet("ZVMExchange"),
		Exchange: func() common.Address {
			if d := appsFile.Get(); d != nil {
				return d.Exchange
			}
			return common.Address{}
		},
		Log: slog.Default().With("svc", "orders"),
	})
	if err != nil {
		return zero, err
	}
	plugin := nft.New(nft.Config{
		Apps: func() nft.Apps {
			d := appsFile.Get()
			if d == nil {
				return nft.Apps{}
			}
			return nft.Apps{Exchange: d.Exchange, CollectionFactory: d.CollectionFactory, WrappedZNN: d.WrappedZNN}
		},
		Media: store,
	})

	return Result{
		Start:   func(ctx context.Context) { go book.Run(ctx) },
		Plugins: []indexer.Plugin{plugin},
		Extensions: []api.Extension{market.New(market.Config{
			DB: env.DB.SQL(), Rollup: env.Rollup, Orders: book, Apps: appsFile, Media: store,
			Log: slog.Default().With("svc", "market"),
		})},
		// The book's payloads stay intact; cache markers and cursors rewind
		// in the same SQL transaction as the index, including on refusal.
		OnRewindTx: book.RewindTx,
	}, nil
}
