package nodecmd

import (
	"context"
	"flag"
	"log/slog"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/markets"
	"github.com/Zenoglyphs-VM/zvm-node/prices"
)

// Markets is the ZVM's own Uniswap pools — pairs, candles, trades — and the
// dollar price laid over them, as an Extender.
//
// Dollars are one request a minute to DexScreener, made by the node. The page
// never makes it: a browser asking a price API directly would leak every
// visitor to a third party, and would be blocked as mixed content on any node
// served over plain HTTP. -prices=off turns the outbound request off entirely,
// and the pools keep their prices in ZNN.
type Markets struct {
	from         *string
	pairs        *string
	interval     *time.Duration
	minLiquidity *float64
}

func (m *Markets) Flags(fs *flag.FlagSet) {
	m.from = fs.String("prices", "dexscreener", "where dollar prices come from, or 'off'")
	m.pairs = fs.String("price-pairs", "", "pools to price ZNN and QSR from, as chain:0xpair,… (default: the wZNN/WETH and wQSR/wZNN pools on Ethereum)")
	m.interval = fs.Duration("price-interval", time.Minute, "how often to ask for prices")
	m.minLiquidity = fs.Float64("min-liquidity-usd", 25, "how deep a ZVM pool must be before it may price a token")
}

func (m *Markets) Build(ctx context.Context, env Env) (Result, error) {
	var zero Result

	var svc *prices.Service
	if !strings.EqualFold(*m.from, "off") {
		pairs := prices.DefaultPairs
		if *m.pairs != "" {
			parsed, err := prices.ParsePairs(*m.pairs)
			if err != nil {
				return zero, err
			}
			pairs = parsed
		}
		s, err := prices.New(prices.Config{
			Pairs: pairs, Interval: *m.interval, Log: slog.Default().With("svc", "prices"),
		}, env.DB.SQL())
		if err != nil {
			return zero, err
		}
		svc = s
		go svc.Run(ctx)
	}

	return Result{
		Extensions: []api.Extension{markets.New(markets.Config{
			DB: env.DB.SQL(), Rollup: env.Rollup, Prices: svc, Dex: env.Dex,
			MinLiquidityUSD: *m.minLiquidity,
			Log:             slog.Default().With("svc", "markets"),
		})},
	}, nil
}
