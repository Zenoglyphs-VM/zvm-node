package prices

import (
	"math"
	"math/big"
	"strings"
)

// Pool is one Uniswap V2 pair as derivation needs it. Addresses are lower case.
type Pool struct {
	Pair      string
	Token0    string
	Token1    string
	Reserve0  *big.Int
	Reserve1  *big.Int
	Decimals0 int
	Decimals1 int
}

// TokenPrice is a ZVM token's dollar price and the route to it.
type TokenPrice struct {
	USD float64 `json:"usd"`
	// Anchor names the Zenon asset a token is priced as directly (ZNN, QSR);
	// empty for a price derived through a pool.
	Anchor string `json:"anchor,omitempty"`
	// Pair is the pool the price came through, and LiquidityUSD that pool's
	// depth counted from its priced side, doubled.
	Pair         string  `json:"pair,omitempty"`
	LiquidityUSD float64 `json:"liquidityUsd,omitempty"`
	Hops         int     `json:"hops"`
}

// Derive prices every token reachable from the anchors through pools, the way
// Uniswap's v2 subgraph derives a token's ETH price: from a pool against a token
// already priced, and only when that side of the pool holds at least
// minLiquidityUSD — below that, anyone can set the price for the cost of the
// pool. Unlike the subgraph, a token priced this way can price the next one,
// up to maxHops from an anchor; among candidates the deepest pool wins, and a
// token keeps the price from the fewest hops.
func Derive(pools []Pool, anchors map[string]TokenPrice, minLiquidityUSD float64, maxHops int) map[string]TokenPrice {
	out := make(map[string]TokenPrice, len(anchors))
	for k, v := range anchors {
		out[strings.ToLower(k)] = v
	}
	for hop := 1; hop <= maxHops; hop++ {
		best := map[string]TokenPrice{}
		for _, p := range pools {
			r0, r1 := Units(p.Reserve0, p.Decimals0), Units(p.Reserve1, p.Decimals1)
			if !(r0 > 0) || !(r1 > 0) {
				continue
			}
			sides := [2]struct {
				known, unknown string
				kr, ur         float64
			}{
				{strings.ToLower(p.Token0), strings.ToLower(p.Token1), r0, r1},
				{strings.ToLower(p.Token1), strings.ToLower(p.Token0), r1, r0},
			}
			for _, s := range sides {
				kp, ok := out[s.known]
				if !ok {
					continue
				}
				if _, done := out[s.unknown]; done {
					continue
				}
				depth := s.kr * kp.USD
				if depth < minLiquidityUSD {
					continue
				}
				cand := TokenPrice{USD: depth / s.ur, Pair: strings.ToLower(p.Pair), LiquidityUSD: 2 * depth, Hops: hop}
				if cur, ok := best[s.unknown]; !ok || cand.LiquidityUSD > cur.LiquidityUSD {
					best[s.unknown] = cand
				}
			}
		}
		if len(best) == 0 {
			break
		}
		for k, v := range best {
			out[k] = v
		}
	}
	return out
}

// PoolLiquidityUSD values a pool from whichever sides are priced: both when
// both are, one side doubled when only one is, zero when neither is.
func PoolLiquidityUSD(p Pool, priced map[string]TokenPrice) float64 {
	a, aok := priced[strings.ToLower(p.Token0)]
	b, bok := priced[strings.ToLower(p.Token1)]
	v0 := Units(p.Reserve0, p.Decimals0) * a.USD
	v1 := Units(p.Reserve1, p.Decimals1) * b.USD
	switch {
	case aok && bok:
		return v0 + v1
	case aok:
		return 2 * v0
	case bok:
		return 2 * v1
	}
	return 0
}

// Units is a raw token amount as a float, decimals applied.
func Units(x *big.Int, decimals int) float64 {
	if x == nil || x.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Float).SetInt(x).Float64()
	return f / math.Pow10(decimals)
}
