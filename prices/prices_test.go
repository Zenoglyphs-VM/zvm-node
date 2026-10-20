package prices

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
)

// DexScreener's answer on 2026-09-15 to a request naming both default pairs:
// the wQSR/wZNN pool came back, the wZNN/WETH pool did not.
const recordedResponse = `{"schemaVersion":"1.0.0","pairs":[{"chainId":"ethereum","dexId":"uniswap","url":"https://dexscreener.com/ethereum/0xe6c61425d0383c1cde02a49365945f48ebf0ea0c","pairAddress":"0xe6c61425D0383c1cde02A49365945f48Ebf0eA0C","labels":["v2"],"baseToken":{"address":"0x96546AFE4a21515A3a30CD3fd64A70eB478DC174","name":"Wrapped QSR","symbol":"wQSR"},"quoteToken":{"address":"0xb2e96a63479C2Edd2FD62b382c89D5CA79f572d3","name":"Wrapped ZNN","symbol":"wZNN"},"priceNative":"0.1383","priceUsd":"0.02093","txns":{"m5":{"buys":0,"sells":0},"h1":{"buys":0,"sells":0},"h6":{"buys":0,"sells":0},"h24":{"buys":1,"sells":1}},"volume":{"h24":134.98,"h6":0,"h1":0,"m5":0},"priceChange":{"h24":18.49},"liquidity":{"usd":1672.7,"base":39949,"quote":5525.2591},"fdv":30237,"marketCap":30237,"pairCreatedAt":1687429943000}]}`

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestParseMarksPairsItDidNotReturn(t *testing.T) {
	pairs, err := ParseResponse([]byte(recordedResponse), DefaultPairs)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("got %d pairs, want one per ref", len(pairs))
	}
	if pairs[0].Found {
		t.Errorf("wZNN/WETH was not in the answer but reads as found")
	}
	if pairs[0].URL != "https://dexscreener.com/ethereum/0xdac866a3796f85cb84a914d98faec052e3b5596d" {
		t.Errorf("missing pair URL %q", pairs[0].URL)
	}
	p := pairs[1]
	if !p.Found || p.PriceUSD != 0.02093 || p.PriceNative != 0.1383 || p.LiquidityUSD != 1672.7 {
		t.Errorf("wQSR/wZNN parsed as %+v", p)
	}
	if p.Base.Address != WQSREthereum || p.Quote.Address != WZNNEthereum {
		t.Errorf("token addresses not lower-cased: %s %s", p.Base.Address, p.Quote.Address)
	}
	if p.Change24h == nil || *p.Change24h != 18.49 || p.Txns24h != 2 || p.Volume24hUSD != 134.98 {
		t.Errorf("24h fields: change %v txns %d volume %v", p.Change24h, p.Txns24h, p.Volume24hUSD)
	}
}

func TestZNNFallsBackToTheQuoteSide(t *testing.T) {
	pairs, _ := ParseResponse([]byte(recordedResponse), DefaultPairs)
	got := SelectAssets(pairs, DefaultAssets)
	if q := got["QSR"]; q.USD != 0.02093 || q.Side != "base" || q.Change24h == nil {
		t.Errorf("QSR = %+v", q)
	}
	z := got["ZNN"]
	if !near(z.USD, 0.02093/0.1383) || z.Side != "quote" {
		t.Errorf("ZNN = %+v, want priceUsd/priceNative from the quote side", z)
	}
	if z.Change24h != nil {
		t.Errorf("the quote side has no reported 24h change, got %v", *z.Change24h)
	}
}

func TestTheDeeperPoolWins(t *testing.T) {
	pairs, _ := ParseResponse([]byte(recordedResponse), DefaultPairs)
	pairs[0] = Pair{
		Chain: "ethereum", Address: DefaultPairs[0].Address, Found: true,
		Base:     &Token{Address: WZNNEthereum, Symbol: "wZNN"},
		Quote:    &Token{Address: "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2", Symbol: "WETH"},
		PriceUSD: 0.152, PriceNative: 0.0000603, LiquidityUSD: 80_000,
	}
	z := SelectAssets(pairs, DefaultAssets)["ZNN"]
	if z.USD != 0.152 || z.Pair != DefaultPairs[0].Address || z.Side != "base" {
		t.Errorf("ZNN = %+v, want the $80k wZNN/WETH pool over the $1.7k one", z)
	}
}

func TestNumbersArriveEitherWay(t *testing.T) {
	var v struct {
		A flexFloat `json:"a"`
		B flexFloat `json:"b"`
		C flexFloat `json:"c"`
		D flexFloat `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"a":"1.5","b":2.25,"c":null,"d":"n/a"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != 1.5 || v.B != 2.25 || v.C != 0 || v.D != 0 {
		t.Errorf("got %+v", v)
	}
}

func TestRateAt(t *testing.T) {
	s := []Sample{{100, 1}, {200, 2}, {300, 3}}
	for _, c := range []struct {
		t    int64
		want float64
	}{{50, 1}, {100, 1}, {199, 1}, {250, 2}, {300, 3}, {999, 3}} {
		if got, ok := RateAt(s, c.t); !ok || got != c.want {
			t.Errorf("RateAt(%d) = %v, want %v", c.t, got, c.want)
		}
	}
	if _, ok := RateAt(nil, 1); ok {
		t.Error("no samples should report no rate")
	}
}

func e(n int64, decimals int) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
}

func TestDeriveThroughPools(t *testing.T) {
	const wznn, qsr, bna, bnb, thin = "0x00000000000000000000000000000000000000aa", "0x00000000000000000000000000000000000000bb",
		"0x00000000000000000000000000000000000000cc", "0x00000000000000000000000000000000000000dd", "0x00000000000000000000000000000000000000ee"
	pools := []Pool{
		// 1,000,000 BNA against 10,000 ZNN: a BNA is 0.01 ZNN.
		{Pair: "0x01", Token0: bna, Token1: wznn, Reserve0: e(1_000_000, 18), Reserve1: e(10_000, 18), Decimals0: 18, Decimals1: 18},
		// BNA/BNB at 1:2, no ZNN in it: BNB is two hops out.
		{Pair: "0x02", Token0: bna, Token1: bnb, Reserve0: e(1_000_000, 18), Reserve1: e(2_000_000, 18), Decimals0: 18, Decimals1: 18},
		// One ZNN is not enough depth to price anything.
		{Pair: "0x03", Token0: thin, Token1: wznn, Reserve0: e(1, 18), Reserve1: e(1, 18), Decimals0: 18, Decimals1: 18},
		// QSR has 8 decimals; an anchor keeps its own price whatever a pool says.
		{Pair: "0x04", Token0: wznn, Token1: qsr, Reserve0: e(1_000, 18), Reserve1: e(9_000, 8), Decimals0: 18, Decimals1: 8},
	}
	anchors := map[string]TokenPrice{wznn: {USD: 0.15, Anchor: "ZNN"}, qsr: {USD: 0.021, Anchor: "QSR"}}
	got := Derive(pools, anchors, 25, 3)

	if p := got[bna]; !near(p.USD, 0.0015) || p.Hops != 1 || p.Pair != "0x01" || !near(p.LiquidityUSD, 3_000) {
		t.Errorf("BNA = %+v", p)
	}
	if p := got[bnb]; !near(p.USD, 0.00075) || p.Hops != 2 || p.Pair != "0x02" {
		t.Errorf("BNB = %+v", p)
	}
	if p, ok := got[thin]; ok {
		t.Errorf("a pool holding $0.15 of ZNN priced its token: %+v", p)
	}
	if p := got[qsr]; p.USD != 0.021 || p.Anchor != "QSR" {
		t.Errorf("QSR anchor overwritten: %+v", p)
	}
	if got := PoolLiquidityUSD(pools[3], got); !near(got, 1_000*0.15+9_000*0.021) {
		t.Errorf("ZNN/QSR pool liquidity = %v", got)
	}
	if got := PoolLiquidityUSD(pools[0], map[string]TokenPrice{wznn: {USD: 0.15}}); !near(got, 3_000) {
		t.Errorf("one priced side should count double, got %v", got)
	}
}
