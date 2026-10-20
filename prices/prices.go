// Package prices gives ZNN and QSR a dollar value, and every ZVM token with a
// pool against them one too.
//
// The dollar side comes from DexScreener, which prices the bridged wZNN and
// wQSR in Ethereum's Uniswap V2 pools. The ZVM side is arithmetic on the ZVM's
// own Uniswap reserves (derive.go). The node reads both; the page never does,
// so the browser still fetches nothing from anyone but its own node.
package prices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The bridged Zenon assets on Ethereum, as the Zenon bridge mints them.
const (
	WZNNEthereum = "0xb2e96a63479c2edd2fd62b382c89d5ca79f572d3"
	WQSREthereum = "0x96546afe4a21515a3a30cd3fd64a70eb478dc174"
)

// PairRef names one pool on one chain, the way DexScreener addresses it.
type PairRef struct {
	Chain   string
	Address string
}

// DefaultPairs are the two pools the ZVM prices from: wZNN/WETH and
// wQSR/wZNN, both Uniswap V2 on Ethereum.
var DefaultPairs = []PairRef{
	{Chain: "ethereum", Address: "0xdac866a3796f85cb84a914d98faec052e3b5596d"},
	{Chain: "ethereum", Address: "0xe6c61425d0383c1cde02a49365945f48ebf0ea0c"},
}

// DefaultAssets maps a token on the pair's chain to the Zenon asset it wraps.
var DefaultAssets = map[string]string{WZNNEthereum: "ZNN", WQSREthereum: "QSR"}

// ParsePairs reads "chain:0xpair,chain:0xpair".
func ParsePairs(s string) ([]PairRef, error) {
	var out []PairRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		chain, addr, ok := strings.Cut(part, ":")
		if !ok || chain == "" || !strings.HasPrefix(addr, "0x") {
			return nil, fmt.Errorf("price pair %q: want chain:0xaddress", part)
		}
		out = append(out, PairRef{Chain: strings.ToLower(chain), Address: strings.ToLower(addr)})
	}
	return out, nil
}

type Token struct {
	Address string `json:"address"`
	Symbol  string `json:"symbol"`
	Name    string `json:"name"`
}

// Pair is what DexScreener said about one configured pool. Found is false when
// it said nothing: its API does not return every pool its site shows.
type Pair struct {
	Chain        string   `json:"chain"`
	Address      string   `json:"address"`
	Found        bool     `json:"found"`
	DEX          string   `json:"dex,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	URL          string   `json:"url"`
	Base         *Token   `json:"base,omitempty"`
	Quote        *Token   `json:"quote,omitempty"`
	PriceUSD     float64  `json:"priceUsd,omitempty"`
	PriceNative  float64  `json:"priceNative,omitempty"`
	LiquidityUSD float64  `json:"liquidityUsd,omitempty"`
	Volume24hUSD float64  `json:"volume24hUsd,omitempty"`
	Change24h    *float64 `json:"change24h,omitempty"`
	Txns24h      int      `json:"txns24h,omitempty"`
}

// Quote is one asset's dollar price and where it came from.
type Quote struct {
	Asset string  `json:"asset"`
	USD   float64 `json:"usd"`
	// Change24h is DexScreener's own 24-hour change, and only for an asset that
	// is the base of its pair: the quote side's change is not reported.
	Change24h    *float64 `json:"change24h,omitempty"`
	Pair         string   `json:"pair"`
	Side         string   `json:"side"`
	LiquidityUSD float64  `json:"liquidityUsd"`
	URL          string   `json:"url"`
	At           int64    `json:"at"`
}

type Snapshot struct {
	Enabled   bool             `json:"enabled"`
	Source    string           `json:"source"`
	UpdatedAt int64            `json:"updatedAt,omitempty"`
	CheckedAt int64            `json:"checkedAt,omitempty"`
	Stale     bool             `json:"stale"`
	Error     string           `json:"error,omitempty"`
	Assets    map[string]Quote `json:"assets"`
	Pairs     []Pair           `json:"pairs"`
}

type Sample struct {
	At  int64   `json:"t"`
	USD float64 `json:"usd"`
}

type Config struct {
	Pairs    []PairRef
	Assets   map[string]string // lower-case token address on the pair's chain → asset
	Interval time.Duration
	BaseURL  string
	Client   *http.Client
	Log      *slog.Logger
}

// Service polls DexScreener and keeps every answer, so a chart can convert a
// past candle at the rate of its own time.
type Service struct {
	cfg Config
	db  *sql.DB

	mu   sync.RWMutex
	snap Snapshot
}

const samplesSchema = `
CREATE TABLE IF NOT EXISTS price_samples (
	asset TEXT    NOT NULL,
	ts    INTEGER NOT NULL,
	usd   REAL    NOT NULL,
	pair  TEXT    NOT NULL,
	PRIMARY KEY (asset, ts)
);`

func New(cfg Config, db *sql.DB) (*Service, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.dexscreener.com"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Assets == nil {
		cfg.Assets = DefaultAssets
	}
	if _, err := db.Exec(samplesSchema); err != nil {
		return nil, fmt.Errorf("price samples: %w", err)
	}
	s := &Service{cfg: cfg, db: db, snap: Snapshot{Enabled: true, Source: "dexscreener", Assets: map[string]Quote{}}}
	// A restart starts from the last answer rather than from nothing; its age
	// is on it, so it reads as stale until the first fetch.
	rows, err := db.Query(`SELECT asset, ts, usd, pair FROM price_samples p
		WHERE ts = (SELECT MAX(ts) FROM price_samples WHERE asset = p.asset)`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var q Quote
			if rows.Scan(&q.Asset, &q.At, &q.USD, &q.Pair) == nil {
				q.Side = "stored"
				s.snap.Assets[q.Asset] = q
				s.snap.UpdatedAt = max(s.snap.UpdatedAt, q.At)
			}
		}
	}
	return s, nil
}

func (s *Service) Run(ctx context.Context) {
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Log.Warn("refreshing prices", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Interval):
		}
	}
}

// Snapshot is the latest prices. Stale means the last good answer is older than
// five polls, or ten minutes, whichever is longer.
func (s *Service) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{Source: "dexscreener", Assets: map[string]Quote{}}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.snap
	out.Assets = make(map[string]Quote, len(s.snap.Assets))
	for k, v := range s.snap.Assets {
		out.Assets[k] = v
	}
	limit := max(5*s.cfg.Interval, 10*time.Minute)
	out.Stale = out.UpdatedAt == 0 || time.Since(time.Unix(out.UpdatedAt, 0)) > limit
	return out
}

func (s *Service) Refresh(ctx context.Context) error {
	now := time.Now().Unix()
	pairs, err := s.fetch(ctx)
	s.mu.Lock()
	s.snap.CheckedAt = now
	if err != nil {
		s.snap.Error = err.Error()
		s.mu.Unlock()
		return err
	}
	s.snap.Pairs = pairs
	assets := SelectAssets(pairs, s.cfg.Assets)
	if len(assets) == 0 {
		s.snap.Error = "no configured pair priced ZNN or QSR"
		s.mu.Unlock()
		return errors.New(s.snap.Error)
	}
	for k, q := range assets {
		q.At = now
		assets[k] = q
	}
	// An asset this answer could not price keeps its last value; its time says how old.
	for k, old := range s.snap.Assets {
		if _, ok := assets[k]; !ok {
			assets[k] = old
		}
	}
	s.snap.Assets = assets
	s.snap.UpdatedAt = now
	s.snap.Error = ""
	s.mu.Unlock()

	for _, q := range assets {
		if q.At != now {
			continue
		}
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO price_samples (asset, ts, usd, pair) VALUES (?,?,?,?)`,
			q.Asset, now, q.USD, q.Pair); err != nil {
			return fmt.Errorf("recording a price sample: %w", err)
		}
	}
	return nil
}

// History returns samples for an asset from `since`, oldest first.
func (s *Service) History(asset string, since int64, limit int) ([]Sample, error) {
	if s == nil {
		return []Sample{}, nil
	}
	rows, err := s.db.Query(`SELECT ts, usd FROM price_samples WHERE asset = ? AND ts >= ? ORDER BY ts DESC LIMIT ?`,
		asset, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.At, &x.USD); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, rows.Err()
}

// Rates returns every sample needed to convert times in [from, to]: the last
// one before `from` and everything after it.
func (s *Service) Rates(asset string, from, to int64) ([]Sample, error) {
	if s == nil {
		return nil, nil
	}
	var out []Sample
	var before Sample
	if err := s.db.QueryRow(`SELECT ts, usd FROM price_samples WHERE asset = ? AND ts < ? ORDER BY ts DESC LIMIT 1`,
		asset, from).Scan(&before.At, &before.USD); err == nil {
		out = append(out, before)
	}
	rows, err := s.db.Query(`SELECT ts, usd FROM price_samples WHERE asset = ? AND ts >= ? AND ts <= ? ORDER BY ts`, asset, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x Sample
		if err := rows.Scan(&x.At, &x.USD); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if len(out) == 0 {
		// Nothing up to `to`: fall forward to the first sample there is.
		var first Sample
		if err := s.db.QueryRow(`SELECT ts, usd FROM price_samples WHERE asset = ? ORDER BY ts LIMIT 1`, asset).
			Scan(&first.At, &first.USD); err == nil {
			out = append(out, first)
		}
	}
	return out, rows.Err()
}

// RateAt is the latest sample at or before t; before the first sample it is the
// first sample. ok is false only when there are no samples at all.
func RateAt(samples []Sample, t int64) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	i := sort.Search(len(samples), func(i int) bool { return samples[i].At > t })
	if i == 0 {
		return samples[0].USD, true
	}
	return samples[i-1].USD, true
}

// SelectAssets prices each configured asset from the deepest pool that has it
// on either side. As a pair's base, its price is DexScreener's priceUsd; as the
// quote, it is priceUsd divided by priceNative (the base priced in the quote).
func SelectAssets(pairs []Pair, assets map[string]string) map[string]Quote {
	out := map[string]Quote{}
	consider := func(q Quote) {
		if !(q.USD > 0) || math.IsInf(q.USD, 0) {
			return
		}
		if cur, ok := out[q.Asset]; !ok || q.LiquidityUSD > cur.LiquidityUSD {
			out[q.Asset] = q
		}
	}
	for _, p := range pairs {
		if !p.Found || p.Base == nil || p.Quote == nil || !(p.PriceUSD > 0) {
			continue
		}
		if a, ok := assets[strings.ToLower(p.Base.Address)]; ok {
			consider(Quote{Asset: a, USD: p.PriceUSD, Change24h: p.Change24h, Pair: p.Address, Side: "base",
				LiquidityUSD: p.LiquidityUSD, URL: p.URL})
		}
		if a, ok := assets[strings.ToLower(p.Quote.Address)]; ok && p.PriceNative > 0 {
			consider(Quote{Asset: a, USD: p.PriceUSD / p.PriceNative, Pair: p.Address, Side: "quote",
				LiquidityUSD: p.LiquidityUSD, URL: p.URL})
		}
	}
	return out
}

// --- DexScreener -----------------------------------------------------------

// flexFloat reads DexScreener's numbers, which arrive as strings for prices and
// as numbers for everything else.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

type dsToken struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Symbol  string `json:"symbol"`
}

type dsPair struct {
	ChainID     string               `json:"chainId"`
	DexID       string               `json:"dexId"`
	URL         string               `json:"url"`
	PairAddress string               `json:"pairAddress"`
	Labels      []string             `json:"labels"`
	BaseToken   dsToken              `json:"baseToken"`
	QuoteToken  dsToken              `json:"quoteToken"`
	PriceNative flexFloat            `json:"priceNative"`
	PriceUSD    flexFloat            `json:"priceUsd"`
	Volume      map[string]flexFloat `json:"volume"`
	PriceChange map[string]flexFloat `json:"priceChange"`
	Liquidity   struct {
		USD flexFloat `json:"usd"`
	} `json:"liquidity"`
	Txns map[string]struct {
		Buys  int `json:"buys"`
		Sells int `json:"sells"`
	} `json:"txns"`
}

type dsResponse struct {
	Pairs []dsPair `json:"pairs"`
}

// ParseResponse turns one pairs response into Pairs for the refs asked about,
// in the order asked, marking the ones it did not return.
func ParseResponse(raw []byte, refs []PairRef) ([]Pair, error) {
	var resp dsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("dexscreener: %w", err)
	}
	byAddr := map[string]dsPair{}
	for _, p := range resp.Pairs {
		byAddr[strings.ToLower(p.ChainID)+":"+strings.ToLower(p.PairAddress)] = p
	}
	out := make([]Pair, 0, len(refs))
	for _, ref := range refs {
		p, ok := byAddr[ref.Chain+":"+ref.Address]
		if !ok {
			out = append(out, Pair{Chain: ref.Chain, Address: ref.Address, URL: fmt.Sprintf("https://dexscreener.com/%s/%s", ref.Chain, ref.Address)})
			continue
		}
		pair := Pair{
			Chain: ref.Chain, Address: ref.Address, Found: true, DEX: p.DexID, Labels: p.Labels, URL: p.URL,
			Base:         &Token{Address: strings.ToLower(p.BaseToken.Address), Symbol: p.BaseToken.Symbol, Name: p.BaseToken.Name},
			Quote:        &Token{Address: strings.ToLower(p.QuoteToken.Address), Symbol: p.QuoteToken.Symbol, Name: p.QuoteToken.Name},
			PriceUSD:     float64(p.PriceUSD),
			PriceNative:  float64(p.PriceNative),
			LiquidityUSD: float64(p.Liquidity.USD),
			Volume24hUSD: float64(p.Volume["h24"]),
		}
		if ch, ok := p.PriceChange["h24"]; ok {
			v := float64(ch)
			pair.Change24h = &v
		}
		if t, ok := p.Txns["h24"]; ok {
			pair.Txns24h = t.Buys + t.Sells
		}
		if pair.URL == "" {
			pair.URL = fmt.Sprintf("https://dexscreener.com/%s/%s", ref.Chain, ref.Address)
		}
		out = append(out, pair)
	}
	return out, nil
}

func (s *Service) fetch(ctx context.Context) ([]Pair, error) {
	byChain := map[string][]PairRef{}
	var chains []string
	for _, ref := range s.cfg.Pairs {
		if _, ok := byChain[ref.Chain]; !ok {
			chains = append(chains, ref.Chain)
		}
		byChain[ref.Chain] = append(byChain[ref.Chain], ref)
	}
	var out []Pair
	for _, chain := range chains {
		refs := byChain[chain]
		addrs := make([]string, len(refs))
		for i, r := range refs {
			addrs[i] = r.Address
		}
		url := fmt.Sprintf("%s/latest/dex/pairs/%s/%s", strings.TrimRight(s.cfg.BaseURL, "/"), chain, strings.Join(addrs, ","))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "zvm-node")
		resp, err := s.cfg.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("dexscreener: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("dexscreener: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("dexscreener: %s", resp.Status)
		}
		pairs, err := ParseResponse(raw, refs)
		if err != nil {
			return nil, err
		}
		out = append(out, pairs...)
	}
	return out, nil
}
