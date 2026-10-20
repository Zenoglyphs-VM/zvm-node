// Package api serves the NFT market: collections and their items as the index
// sees them, and the signed orders the node was handed.
//
// It is an api.Extension, so it shares the node's HTTP server, its JSON
// envelope and its database handle rather than standing up a second service
// beside them. A market on its own port would need its own copy of the index
// to answer "who owns this", and two copies is how the two stop agreeing.
package market

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	nodeapi "github.com/Zenoglyphs-VM/zvm-node/api"

	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/media"
	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// Config is what the node's main hands the market.
type Config struct {
	// DB is the node's index. The market reads the nft_* tables the index
	// plugin fills and writes none of them.
	DB     *sql.DB
	Rollup *rollup.Config
	// Orders is the order book (D28): signatures this node was handed, kept
	// apart from the index because the chain cannot give them back.
	Orders *orderbook.Book
	// Apps is the exchange, the collection factory and the farm, re-read
	// whenever the deployment file changes.
	Apps *apps.File
	// Media is where this node stores what NFTs point at, or nil if it stores
	// none.
	Media *media.Store
	Log   *slog.Logger
}

// Market implements api.Extension.
type Market struct {
	cfg Config
	db  *sql.DB
}

func New(cfg Config) *Market {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Market{cfg: cfg, db: cfg.DB}
}

func (s *Market) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/nft", nodeapi.Handle(s.nftOverview))
	mux.HandleFunc("GET /api/nft/collections", nodeapi.Handle(s.nftCollections))
	mux.HandleFunc("GET /api/nft/collections/{addr}", nodeapi.Handle(s.nftCollection))
	mux.HandleFunc("GET /api/nft/collections/{addr}/items", nodeapi.Handle(s.nftItems))
	mux.HandleFunc("GET /api/nft/collections/{addr}/activity", nodeapi.Handle(s.nftActivity))
	mux.HandleFunc("GET /api/nft/items/{addr}/{id}", nodeapi.Handle(s.nftItem))
	mux.HandleFunc("GET /api/nft/accounts/{addr}", nodeapi.Handle(s.nftAccount))
	mux.HandleFunc("GET /api/nft/sales", nodeapi.Handle(s.nftSales))
	mux.HandleFunc("GET /api/nft/listings", nodeapi.Handle(s.nftListings))
	mux.HandleFunc("GET /api/nft/offers", nodeapi.Handle(s.nftOffers))
	// Signed orders (D28): the exchange's domain for a wallet to sign against,
	// the book, and one order with what a fill needs.
	mux.HandleFunc("GET /api/nft/exchange", nodeapi.Handle(s.nftExchange))
	mux.HandleFunc("GET /api/nft/orders", nodeapi.Handle(s.nftOrders))
	mux.HandleFunc("POST /api/nft/orders", nodeapi.Handle(s.nftSubmitOrders))
	mux.HandleFunc("GET /api/nft/orders/{hash}", nodeapi.Handle(s.nftOrder))
	if s.cfg.Media != nil {
		s.cfg.Media.Register(mux)
	}
}

// Info adds this extension's slice of /api/info, and names it on /api/ops.
func (s *Market) Info() map[string]any {
	out := map[string]any{}
	if s.cfg.Apps != nil {
		if a := s.cfg.Apps.Get(); a != nil {
			out["apps"] = a
		}
	}
	if s.cfg.Media != nil {
		out["media"] = s.cfg.Media.Info()
	}
	return out
}

// Wrap puts the media hostname outside everything, so it answers with stored
// objects and never with the explorer, the API or an upload.
func (s *Market) Wrap(h http.Handler) http.Handler {
	if s.cfg.Media == nil {
		return h
	}
	return s.cfg.Media.Wrap(h)
}
