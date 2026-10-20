package markets

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/prices"
)

func newTestMarkets(t *testing.T) (*Markets, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	// The index's own schema, not a copy of it: these queries run against the
	// tables the indexer actually creates, so a renamed column fails here
	// rather than in production.
	for _, ddl := range indexer.Schema() {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	return New(Config{
		DB:     db,
		Rollup: &rollup.Config{L2ChainID: 7_340_470},
		// No price service: the node was started with -prices=off, or never
		// reached DexScreener. This is the case the pages have to survive.
		Prices:          nil,
		MinLiquidityUSD: 25,
		Log:             slog.New(slog.DiscardHandler),
	}), db
}

// get serves one request through a mux the extension registered itself on,
// which is the only way to prove the routes exist at the paths the pages ask
// for: a handler called directly answers a path that was never wired up.
func get(t *testing.T, m *Markets, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	m.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return w
}

func getObject(t *testing.T, m *Markets, path string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(get(t, m, path).Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return body
}

// A node with no dollar price still answers every route. The pages read
// `enabled` to decide whether to draw a dollar column at all, so answering 500
// here would take the DEX pages down with the price feed.
func TestWithoutPricesEverythingStillAnswers(t *testing.T) {
	m, _ := newTestMarkets(t)

	body := getObject(t, m, "/api/prices")
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false on a node with no price service", body["enabled"])
	}
	if got, ok := body["tokens"].(map[string]any); !ok || len(got) != 0 {
		t.Errorf("tokens = %v, want an empty object rather than null", body["tokens"])
	}

	// A list route, so it answers an array: an empty one on a chain with no
	// pools, never an error.
	if got := strings.TrimSpace(get(t, m, "/api/dex/pairs").Body.String()); got != "[]" {
		t.Errorf("/api/dex/pairs = %s, want []", got)
	}
}

// A token with no pool deep enough to price has no price. It must be absent,
// not zero: a page that prints $0.00 for a token someone holds is stating
// something false about their balance.
func TestUnpricedTokenIsAbsentNotZero(t *testing.T) {
	m, db := newTestMarkets(t)
	const (
		tokenA = "0x00000000000000000000000000000000000000aa"
		tokenB = "0x00000000000000000000000000000000000000bb"
		pair   = "0x00000000000000000000000000000000000000cc"
	)
	big18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil).String()
	if _, err := db.Exec(`INSERT INTO dex_pairs (pair, factory, token0, token1, block, timestamp, reserve0, reserve1)
		VALUES (?, '0xf', ?, ?, 1, 1, ?, ?)`, pair, tokenA, tokenB, big18, big18); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{tokenA, tokenB} {
		if _, err := db.Exec(`INSERT INTO tokens (address, kind, symbol, name, decimals, first_block)
			VALUES (?, 'erc20', 'T', 'Token', 18, 1)`, a); err != nil {
			t.Fatal(err)
		}
	}

	tokens, _ := getObject(t, m, "/api/prices")["tokens"].(map[string]any)
	if _, priced := tokens[tokenA]; priced {
		t.Errorf("a pool against an unpriced token priced it anyway: %v", tokens)
	}
}

// The Info slice is what a page reads before it decides to ask for prices at
// all. It must say so even when there is no service behind it.
func TestInfoReportsDisabled(t *testing.T) {
	m, _ := newTestMarkets(t)
	info, ok := m.Info()["prices"].(map[string]any)
	if !ok {
		t.Fatalf("Info has no prices key: %v", m.Info())
	}
	if info["enabled"] != false {
		t.Errorf("enabled = %v, want false", info["enabled"])
	}
}

// Only the deployment's own factory prices a token (D24). Anyone can create a
// pool at any price through a factory of their own, so a node that trusted
// every factory when it had no deployment file let whoever made a pool set the
// price the node reported.
func TestOnlyTheDeploymentsFactoryPrices(t *testing.T) {
	const (
		good   = "0x000000000000000000000000000000000000900d"
		bad    = "0x0000000000000000000000000000000000000bad"
		tokenX = "0x00000000000000000000000000000000000000aa"
		tokenY = "0x00000000000000000000000000000000000000bb"
	)
	qsr := strings.ToLower(rollup.QSRTokenAddr.Hex())
	setup := func(t *testing.T, dexFile string) *Markets {
		m, db := newTestMarkets(t)
		// A stored QSR quote at $1 makes QSR an anchor, as a DexScreener answer would.
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS price_samples (asset TEXT, ts INTEGER, usd REAL, pair TEXT, PRIMARY KEY (asset, ts))`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO price_samples VALUES ('QSR', ?, 1.0, 'test')`, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
		svc, err := prices.New(prices.Config{}, db)
		if err != nil {
			t.Fatal(err)
		}
		m.cfg.Prices, m.cfg.Dex = svc, dexFile
		// A thousand QSR deep on the QSR side: well past the $25 floor.
		qsrUnits := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e8)).String()
		big18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil).String()
		for _, p := range []struct{ pair, factory, token string }{
			{"0x00000000000000000000000000000000000000c1", bad, tokenX},
			{"0x00000000000000000000000000000000000000c2", good, tokenY},
		} {
			if _, err := db.Exec(`INSERT INTO dex_pairs (pair, factory, token0, token1, block, timestamp, reserve0, reserve1)
				VALUES (?, ?, ?, ?, 1, 1, ?, ?)`, p.pair, p.factory, qsr, p.token, qsrUnits, big18); err != nil {
				t.Fatal(err)
			}
		}
		for _, tok := range []struct {
			addr string
			dec  int
		}{{qsr, 8}, {tokenX, 18}, {tokenY, 18}} {
			if _, err := db.Exec(`INSERT INTO tokens (address, kind, symbol, name, decimals, first_block) VALUES (?, 'erc20', 'T', 'T', ?, 1)`,
				tok.addr, tok.dec); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	priced := func(m *Markets) map[string]any {
		tokens, _ := getObject(t, m, "/api/prices")["tokens"].(map[string]any)
		return tokens
	}

	t.Run("no deployment file trusts no factory", func(t *testing.T) {
		tokens := priced(setup(t, ""))
		for _, tok := range []string{tokenX, tokenY} {
			if _, ok := tokens[tok]; ok {
				t.Errorf("with no deployment file, a pool priced %s: %v", tok, tokens)
			}
		}
	})

	t.Run("the named factory prices and no other does", func(t *testing.T) {
		dex := filepath.Join(t.TempDir(), "dex.json")
		if err := os.WriteFile(dex, []byte(`{"chainId":7340470,"uniswapFactory":"`+good+
			`","uniswapRouter":"0x0000000000000000000000000000000000000001","weth":"0x0000000000000000000000000000000000000002"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		tokens := priced(setup(t, dex))
		if _, ok := tokens[tokenX]; ok {
			t.Errorf("a pool from another factory priced %s", tokenX)
		}
		if _, ok := tokens[tokenY]; !ok {
			t.Errorf("a pool from the deployment's own factory did not price %s: %v", tokenY, tokens)
		}
	})
}
