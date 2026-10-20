// Package markets prices the ZVM: what its own Uniswap pools are worth, what
// they traded, and what any of it is in dollars.
//
// Dollars come from DexScreener's price for the bridged wZNN and wQSR on
// Ethereum (package prices). Everything else is derived from the ZVM's own
// reserves, so a token with a pool against ZNN or QSR has a price and one
// without has none — shown as nothing rather than as zero.
//
// It is an api.Extension for the reason the NFT market is one: it answers from
// the node's own index, and a node that derives and indexes but sells nothing
// should not carry a price feed, a DexScreener dependency or the outbound
// request that comes with it. The browser still talks to nobody but this node.
package markets

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"

	nodeapi "github.com/Zenoglyphs-VM/zvm-node/api"
	"github.com/Zenoglyphs-VM/zvm-node/prices"
)

// Config is what the node's main hands the markets extension.
type Config struct {
	// DB is the node's index: the uniswap_* and token tables it fills.
	DB     *sql.DB
	Rollup *rollup.Config
	// Prices is the dollar side, or nil on a node started with -prices=off.
	// Without it pools still have prices in ZNN; they just have none in
	// dollars, and every USD field is omitted rather than zero.
	Prices *prices.Service
	// Dex is the Uniswap V2 deployment file, the same one the node's /api/info
	// offers the swap page. It names the wrapped ZNN every pool quotes against.
	Dex string
	// MinLiquidityUSD is how deep a pool must be before it may price a token.
	MinLiquidityUSD float64
	Log             *slog.Logger
}

// Markets implements api.Extension.
type Markets struct {
	cfg Config
	db  *sql.DB

	mu           sync.Mutex
	cachedMarket marketState
}

func New(cfg Config) *Markets {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Markets{cfg: cfg, db: cfg.DB}
}

func (s *Markets) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prices", nodeapi.Handle(s.pricesHandler))
	mux.HandleFunc("GET /api/prices/history", nodeapi.Handle(s.priceHistory))
	mux.HandleFunc("GET /api/dex/pairs", nodeapi.Handle(s.dexPairs))
	mux.HandleFunc("GET /api/dex/pairs/{pair}", nodeapi.Handle(s.dexPair))
	mux.HandleFunc("GET /api/dex/pairs/{pair}/candles", nodeapi.Handle(s.dexCandles))
	mux.HandleFunc("GET /api/dex/pairs/{pair}/trades", nodeapi.Handle(s.dexTrades))
}

// Info tells the page whether dollars are available at all, and how fresh they
// are. The numbers themselves are a request away; this is what a page needs to
// decide whether to ask.
func (s *Markets) Info() map[string]any {
	if s.cfg.Prices == nil {
		return map[string]any{"prices": map[string]any{"enabled": false}}
	}
	snap := s.cfg.Prices.Snapshot()
	return map[string]any{"prices": map[string]any{
		"enabled": true, "source": snap.Source, "updatedAt": snap.UpdatedAt, "stale": snap.Stale,
	}}
}

// Wrap is inert: the markets extension serves nothing outside /api.
func (s *Markets) Wrap(h http.Handler) http.Handler { return h }

// dex names the live Uniswap V2 deployment, re-read per request for the reason
// api.ReadDex is: a `zvm-node dex` run against a live node should show up
// without a restart.
func (s *Markets) dex() *nodeapi.Dex { return nodeapi.ReadDex(s.cfg.Dex, s.cfg.Rollup.L2ChainID) }

// zeroAddress is how a currency says "ZNN itself".
var zeroAddress = common.Address{}

var candleIntervals = map[int64]bool{60: true, 300: true, 900: true, 3600: true, 14400: true, 86400: true}

type tokenInfo struct {
	Address  string  `json:"address"`
	Symbol   string  `json:"symbol"`
	Name     string  `json:"name"`
	Decimals int     `json:"decimals"`
	USD      float64 `json:"usd,omitempty"`
	Anchor   string  `json:"anchor,omitempty"`
}

type poolRow struct {
	prices.Pool
	Factory   string
	Block     int64
	Timestamp int64
	Symbol0   string
	Symbol1   string
	Name0     string
	Name1     string
}

// marketState is the pools and the prices derived from them, recomputed at most
// once a second: reserves move only when a block does.
type marketState struct {
	at     time.Time
	snap   prices.Snapshot
	pools  []poolRow
	tokens map[string]prices.TokenPrice
}

func (s *Markets) loadPools() ([]poolRow, error) {
	rows, err := s.db.Query(`SELECT p.pair, p.factory, p.token0, p.token1, p.reserve0, p.reserve1, p.block, p.timestamp,
		COALESCE(a.symbol,''), COALESCE(a.name,''), COALESCE(a.decimals,18),
		COALESCE(b.symbol,''), COALESCE(b.name,''), COALESCE(b.decimals,18)
		FROM dex_pairs p LEFT JOIN tokens a ON a.address = p.token0 LEFT JOIN tokens b ON b.address = p.token1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []poolRow
	for rows.Next() {
		var p poolRow
		var r0, r1 string
		if err := rows.Scan(&p.Pair, &p.Factory, &p.Token0, &p.Token1, &r0, &r1, &p.Block, &p.Timestamp,
			&p.Symbol0, &p.Name0, &p.Decimals0, &p.Symbol1, &p.Name1, &p.Decimals1); err != nil {
			return nil, err
		}
		p.Reserve0, _ = new(big.Int).SetString(r0, 10)
		p.Reserve1, _ = new(big.Int).SetString(r1, 10)
		out = append(out, p)
	}
	return out, rows.Err()
}

// anchors are the ZVM tokens a dollar price is known for directly: wrapped ZNN,
// which the pools hold instead of ZNN, and the bridged QSR predeploy.
func (s *Markets) anchors(snap prices.Snapshot) map[string]prices.TokenPrice {
	out := map[string]prices.TokenPrice{}
	if q, ok := snap.Assets["ZNN"]; ok && q.USD > 0 {
		if d := s.dex(); d != nil && d.WETH != zeroAddress {
			out[strings.ToLower(d.WETH.Hex())] = prices.TokenPrice{USD: q.USD, Anchor: "ZNN"}
		}
	}
	if q, ok := snap.Assets["QSR"]; ok && q.USD > 0 {
		out[strings.ToLower(rollup.QSRTokenAddr.Hex())] = prices.TokenPrice{USD: q.USD, Anchor: "QSR"}
	}
	return out
}

func (s *Markets) market() marketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.cachedMarket.at) < time.Second {
		return s.cachedMarket
	}
	st := marketState{at: time.Now(), snap: s.cfg.Prices.Snapshot(), tokens: map[string]prices.TokenPrice{}}
	pools, err := s.loadPools()
	if err != nil {
		s.cfg.Log.Warn("reading pools", "err", err)
	}
	st.pools = pools
	// Only the deployment's own factory prices a token: a pool from any other
	// factory can be created by anyone, at any price.
	trusted := make([]prices.Pool, 0, len(pools))
	factory := ""
	if d := s.dex(); d != nil {
		factory = strings.ToLower(d.Factory.Hex())
	}
	// No deployment file means no factory to trust, and then no pool prices
	// anything: trusting every factory instead let whoever made a pool set
	// the price it reported (D24).
	for _, p := range pools {
		if factory != "" && strings.EqualFold(p.Factory, factory) {
			trusted = append(trusted, p.Pool)
		}
	}
	st.tokens = prices.Derive(trusted, s.anchors(st.snap), s.minLiquidity(), 3)
	s.cachedMarket = st
	return st
}

func (s *Markets) minLiquidity() float64 {
	if s.cfg.MinLiquidityUSD > 0 {
		return s.cfg.MinLiquidityUSD
	}
	return 25
}

func (s *Markets) tokenInfoFor(address, symbol, name string, decimals int, tokens map[string]prices.TokenPrice) tokenInfo {
	t := tokenInfo{Address: address, Symbol: symbol, Name: name, Decimals: decimals}
	// Chains seeded before WZNN existed hold an unmodified WETH9 here, which
	// calls itself Wrapped Ether on a chain with no ether on it. Those get
	// relabelled; a wrapper that already names itself correctly is left alone,
	// because the contract is the better authority than this table.
	if strings.EqualFold(symbol, "WETH") {
		if d := s.dex(); d != nil && strings.EqualFold(address, d.WETH.Hex()) {
			t.Symbol, t.Name = "WZNN", "Wrapped ZNN"
		}
	}
	if p, ok := tokens[strings.ToLower(address)]; ok {
		t.USD, t.Anchor = p.USD, p.Anchor
	}
	return t
}

// --- prices ---------------------------------------------------------------

func (s *Markets) pricesHandler(_ context.Context, _ *http.Request) (any, error) {
	st := s.market()
	tokens := map[string]any{}
	for addr, p := range st.tokens {
		tokens[addr] = p
	}
	out := map[string]any{
		"enabled":         s.cfg.Prices != nil,
		"source":          st.snap.Source,
		"updatedAt":       st.snap.UpdatedAt,
		"checkedAt":       st.snap.CheckedAt,
		"stale":           st.snap.Stale,
		"assets":          st.snap.Assets,
		"pairs":           st.snap.Pairs,
		"tokens":          tokens,
		"minLiquidityUsd": s.minLiquidity(),
	}
	if st.snap.Error != "" {
		out["error"] = st.snap.Error
	}
	return out, nil
}

func (s *Markets) priceHistory(_ context.Context, r *http.Request) (any, error) {
	asset := strings.ToUpper(r.URL.Query().Get("asset"))
	if asset == "" {
		asset = "ZNN"
	}
	hours := nodeapi.IntParam(r, "hours", 24, 24*365)
	samples, err := s.cfg.Prices.History(asset, time.Now().Add(-time.Duration(hours)*time.Hour).Unix(), nodeapi.IntParam(r, "limit", 2000, 20000))
	if err != nil {
		return nil, err
	}
	return map[string]any{"asset": asset, "samples": samples}, nil
}

// --- pools ----------------------------------------------------------------

// quoteSide picks which way round to read a pool: against wrapped ZNN if it
// holds it, then QSR, so a price reads as "one BNA is x ZNN".
func (s *Markets) quoteSide(p poolRow) (base, quote string) {
	weth := ""
	if d := s.dex(); d != nil {
		weth = strings.ToLower(d.WETH.Hex())
	}
	qsr := strings.ToLower(rollup.QSRTokenAddr.Hex())
	t0, t1 := strings.ToLower(p.Token0), strings.ToLower(p.Token1)
	switch {
	case t0 == weth && weth != "":
		return p.Token1, p.Token0
	case t1 == weth && weth != "":
		return p.Token0, p.Token1
	case t0 == qsr:
		return p.Token1, p.Token0
	default:
		return p.Token0, p.Token1
	}
}

type pairStats struct {
	trades  int
	volume0 float64
	volume1 float64
	last    int64
}

func (s *Markets) statsSince(since int64) (map[string]pairStats, error) {
	rows, err := s.db.Query(`SELECT pair,
		SUM(CASE WHEN kind IN ('buy','sell') THEN 1 ELSE 0 END),
		COALESCE(SUM(CASE WHEN kind IN ('buy','sell') THEN CAST(amount0 AS REAL) ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN kind IN ('buy','sell') THEN CAST(amount1 AS REAL) ELSE 0 END), 0),
		MAX(timestamp)
		FROM dex_events WHERE timestamp >= ? GROUP BY pair`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]pairStats{}
	for rows.Next() {
		var pair string
		var st pairStats
		var raw0, raw1 float64
		if err := rows.Scan(&pair, &st.trades, &raw0, &raw1, &st.last); err != nil {
			return nil, err
		}
		st.volume0, st.volume1 = raw0, raw1
		out[pair] = st
	}
	return out, rows.Err()
}

func (s *Markets) priceAt(pair string, ts int64) (float64, bool) {
	var p sql.NullFloat64
	err := s.db.QueryRow(`SELECT price FROM dex_events WHERE pair = ? AND timestamp <= ? AND price IS NOT NULL
		ORDER BY block DESC, idx DESC LIMIT 1`, pair, ts).Scan(&p)
	if err != nil || !p.Valid {
		return 0, false
	}
	return p.Float64, true
}

func (s *Markets) pairJSON(p poolRow, st marketState, day map[string]pairStats) map[string]any {
	t0 := s.tokenInfoFor(p.Token0, p.Symbol0, p.Name0, p.Decimals0, st.tokens)
	t1 := s.tokenInfoFor(p.Token1, p.Symbol1, p.Name1, p.Decimals1, st.tokens)
	base, quote := s.quoteSide(p)
	r0 := prices.Units(p.Reserve0, p.Decimals0)
	r1 := prices.Units(p.Reserve1, p.Decimals1)
	var price float64
	if r0 > 0 && r1 > 0 {
		price = r1 / r0
		if strings.EqualFold(base, p.Token1) {
			price = r0 / r1
		}
	}
	out := map[string]any{
		"pair": p.Pair, "factory": p.Factory, "token0": t0, "token1": t1,
		"reserve0": big0(p.Reserve0), "reserve1": big0(p.Reserve1),
		"base": base, "quote": quote, "price": price,
		"liquidityUsd": prices.PoolLiquidityUSD(p.Pool, st.tokens),
		"createdAt":    p.Timestamp,
	}
	if bp, ok := st.tokens[strings.ToLower(base)]; ok {
		out["priceUsd"] = bp.USD
	}
	stats := day[p.Pair]
	volBase, volQuote := stats.volume0/math.Pow10(p.Decimals0), stats.volume1/math.Pow10(p.Decimals1)
	if strings.EqualFold(base, p.Token1) {
		volBase, volQuote = stats.volume1/math.Pow10(p.Decimals1), stats.volume0/math.Pow10(p.Decimals0)
	}
	volume := map[string]any{"base": volBase, "quote": volQuote}
	if qp, ok := st.tokens[strings.ToLower(quote)]; ok {
		volume["usd"] = volQuote * qp.USD
	}
	out["volume24h"] = volume
	out["trades24h"] = stats.trades
	if stats.last > 0 {
		out["lastTradeAt"] = stats.last
	}
	if before, ok := s.priceAt(p.Pair, time.Now().Add(-24*time.Hour).Unix()); ok && before > 0 && price > 0 {
		if strings.EqualFold(base, p.Token1) {
			before = 1 / before
		}
		out["change24h"] = price/before - 1
	}
	return out
}

func big0(x *big.Int) string {
	if x == nil {
		return "0"
	}
	return x.String()
}

func (s *Markets) dexPairs(_ context.Context, r *http.Request) (any, error) {
	st := s.market()
	day, err := s.statsSince(time.Now().Add(-24 * time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(st.pools))
	for _, p := range st.pools {
		out = append(out, s.pairJSON(p, st, day))
	}
	// Deepest first: a list of markets is a list of where the liquidity is.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["liquidityUsd"].(float64) > out[j]["liquidityUsd"].(float64)
	})
	return out, nil
}

func (s *Markets) findPool(pair string) (poolRow, marketState, error) {
	st := s.market()
	for _, p := range st.pools {
		if strings.EqualFold(p.Pair, pair) {
			return p, st, nil
		}
	}
	return poolRow{}, st, nodeapi.ErrNotFound
}

func (s *Markets) dexPair(_ context.Context, r *http.Request) (any, error) {
	p, st, err := s.findPool(r.PathValue("pair"))
	if err != nil {
		return nil, err
	}
	day, err := s.statsSince(time.Now().Add(-24 * time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	return s.pairJSON(p, st, day), nil
}

// --- candles and trades ---------------------------------------------------

type candle struct {
	Time   int64   `json:"t"`
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume float64 `json:"v"`
	Trades int     `json:"n"`
}

// orientation says which side of a pool is being priced, and how to read an
// event's price and amounts for it.
type orientation struct {
	base, quote       string
	baseDec, quoteDec int
	inverted          bool
	quoteAsset        string // ZNN or QSR, when the quote side is one of them
}

func (s *Markets) orient(p poolRow, want string) orientation {
	base, quote := s.quoteSide(p)
	if want != "" {
		if strings.EqualFold(want, p.Token0) {
			base, quote = p.Token1, p.Token0
		} else if strings.EqualFold(want, p.Token1) {
			base, quote = p.Token0, p.Token1
		}
	}
	o := orientation{base: base, quote: quote}
	if strings.EqualFold(quote, p.Token1) {
		o.baseDec, o.quoteDec = p.Decimals0, p.Decimals1
	} else {
		o.baseDec, o.quoteDec, o.inverted = p.Decimals1, p.Decimals0, true
	}
	o.quoteAsset = s.assetOf(quote)
	return o
}

// assetOf names the Zenon asset a ZVM token is, for converting to dollars at
// the time of a past candle rather than at today's price.
func (s *Markets) assetOf(token string) string {
	if d := s.dex(); d != nil && strings.EqualFold(token, d.WETH.Hex()) {
		return "ZNN"
	}
	if strings.EqualFold(token, rollup.QSRTokenAddr.Hex()) {
		return "QSR"
	}
	return ""
}

func (o orientation) price(p float64) float64 {
	if o.inverted && p != 0 {
		return 1 / p
	}
	return p
}

func (s *Markets) dexCandles(_ context.Context, r *http.Request) (any, error) {
	p, _, err := s.findPool(r.PathValue("pair"))
	if err != nil {
		return nil, err
	}
	interval := int64(nodeapi.IntParam(r, "interval", 900, 86400))
	if !candleIntervals[interval] {
		return nil, fmt.Errorf("%w: interval must be one of 60, 300, 900, 3600, 14400, 86400", nodeapi.ErrBadRequest)
	}
	limit := nodeapi.IntParam(r, "limit", 200, 1000)
	o := s.orient(p, r.URL.Query().Get("quote"))
	now := time.Now().Unix()
	since := (now/interval-int64(limit))*interval + interval

	// Read, not aggregate: the indexer folds every pool event into its buckets
	// as it records it (D21), in the pool's own orientation — token1 per
	// token0 — so all that is left here is to turn it the way the reader asked
	// for. Inverting a candle swaps its high and its low.
	rows, err := s.db.Query(`SELECT bucket, open, high, low, close, vol0, vol1, trades FROM dex_candles
		WHERE pair = ? AND interval = ? AND bucket >= ? ORDER BY bucket`, p.Pair, interval, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candles := []candle{}
	for rows.Next() {
		var c candle
		var open, high, low, close_, vol0, vol1 float64
		if err := rows.Scan(&c.Time, &open, &high, &low, &close_, &vol0, &vol1, &c.Trades); err != nil {
			return nil, err
		}
		c.Open, c.Close = o.price(open), o.price(close_)
		c.High, c.Low = o.price(high), o.price(low)
		if o.inverted {
			c.High, c.Low = c.Low, c.High
		}
		if o.inverted {
			c.Volume = vol0
		} else {
			c.Volume = vol1
		}
		candles = append(candles, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string]any{
		"pair": p.Pair, "interval": interval, "base": o.base, "quote": o.quote,
		"candles": candles, "usd": false,
	}
	if r.URL.Query().Get("usd") == "1" && o.quoteAsset != "" && s.cfg.Prices != nil && len(candles) > 0 {
		rates, err := s.cfg.Prices.Rates(o.quoteAsset, since, now)
		if err != nil {
			return nil, err
		}
		if len(rates) > 0 {
			for i := range candles {
				rate, _ := prices.RateAt(rates, candles[i].Time)
				candles[i].Open *= rate
				candles[i].High *= rate
				candles[i].Low *= rate
				candles[i].Close *= rate
				candles[i].Volume *= rate
			}
			out["usd"] = true
			out["usdAsset"] = o.quoteAsset
			// Candles before the first price this node recorded are converted at
			// that first price; the page says so rather than implying it knew.
			out["usdSince"] = rates[0].At
			out["candles"] = candles
		}
	}
	return out, nil
}

func (s *Markets) dexTrades(_ context.Context, r *http.Request) (any, error) {
	p, st, err := s.findPool(r.PathValue("pair"))
	if err != nil {
		return nil, err
	}
	o := s.orient(p, r.URL.Query().Get("quote"))
	limit := nodeapi.IntParam(r, "limit", 50, 500)
	kinds := "('buy','sell')"
	if r.URL.Query().Get("all") == "1" {
		kinds = "('buy','sell','add','remove')"
	}
	rows, err := s.db.Query(`SELECT block, idx, timestamp, tx, kind, account, amount0, amount1, price
		FROM dex_events WHERE pair = ? AND kind IN `+kinds+` ORDER BY block DESC, idx DESC LIMIT ?`, p.Pair, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	quoteUSD := 0.0
	if q, ok := st.tokens[strings.ToLower(o.quote)]; ok {
		quoteUSD = q.USD
	}
	out := []any{}
	for rows.Next() {
		var block, idx, ts int64
		var tx, kind, account, a0, a1 string
		var raw sql.NullFloat64
		if err := rows.Scan(&block, &idx, &ts, &tx, &kind, &account, &a0, &a1, &raw); err != nil {
			return nil, err
		}
		baseAmount, quoteAmount := floatAmount(a0, o.baseDec), floatAmount(a1, o.quoteDec)
		if o.inverted {
			baseAmount, quoteAmount = floatAmount(a1, o.baseDec), floatAmount(a0, o.quoteDec)
			// A buy takes token0 out of the pool; read the other way round it is a sell.
			if kind == "buy" {
				kind = "sell"
			} else if kind == "sell" {
				kind = "buy"
			}
		}
		row := map[string]any{
			"block": block, "index": idx, "timestamp": ts, "tx": tx, "kind": kind, "account": account,
			"base": baseAmount, "quote": quoteAmount, "price": o.price(raw.Float64),
		}
		if quoteUSD > 0 {
			row["usd"] = quoteAmount * quoteUSD
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func floatAmount(raw string, decimals int) float64 {
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return 0
	}
	return prices.Units(v, decimals)
}

// --- helpers --------------------------------------------------------------

func parseUint(s string) (uint64, bool) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v, err == nil
}
