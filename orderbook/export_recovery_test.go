package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// RecoveryCLIFixture exports only test fixtures, so the external test package
// can import the real nodecmd entrypoint without an orderbook import cycle.
// The execution client is still the declared custom archive-backed adapter.
// SQL observations never call Deriver.Init, IndexBlock, Book.step or recovery.
type RecoveryCLIFixture struct {
	r    *executionRun
	read *sql.DB
}

func NewRecoveryCLIFixture(t *testing.T) *RecoveryCLIFixture {
	t.Helper()
	scenario := newExecutionScenarioProfile(t, true, false)
	r := &executionRun{t: t, scenario: scenario, path: t.TempDir(), limit: 7}
	r.backend = openExecutionEngine(t, filepath.Join(r.path, "engine"), scenario.genesis, false)
	srv := rpc.NewServer()
	if err := srv.RegisterName("engine", r.backend); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterName("eth", executionEth{r.backend}); err != nil {
		t.Fatal(err)
	}
	r.web = httptest.NewServer(srv)
	r.nom = httptest.NewServer(http.HandlerFunc(r.nomReply))
	var err error
	r.eth, err = ethclient.Dial(r.web.URL)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, value []byte) {
		if err := os.WriteFile(filepath.Join(r.path, name), value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(scenario.cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("rollup.json", raw)
	write("jwt.hex", []byte(strings.Repeat("0", 64)))
	raw, err = json.Marshal(map[string]any{"chainId": scenario.cfg.L2ChainID, "exchange": scenario.exchange, "collectionFactory": scenario.factory})
	if err != nil {
		t.Fatal(err)
	}
	write("apps.json", raw)
	uri := url.URL{Scheme: "file", Path: filepath.Join(r.path, "node.db")}
	q := uri.Query()
	q.Set("mode", "ro")
	uri.RawQuery = q.Encode()
	read, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	read.SetMaxOpenConns(1)
	f := &RecoveryCLIFixture{r: r, read: read}
	t.Cleanup(func() { read.Close(); r.eth.Close(); r.web.Close(); r.nom.Close(); r.backend.close() })
	return f
}

func (f *RecoveryCLIFixture) Directory() string { return f.r.path }

func (f *RecoveryCLIFixture) Args() []string {
	path := func(name string) string { return filepath.Join(f.r.path, name) }
	return []string{"-config", path("rollup.json"), "-jwt", path("jwt.hex"), "-db", path("node.db"),
		"-l1", f.r.nom.URL, "-engine", f.r.web.URL, "-eth", f.r.web.URL, "-apps", path("apps.json"),
		"-listen=", "-verify=false", "-prices=off", "-mainnet-rpc=off"}
}

func (f *RecoveryCLIFixture) Horizon(height uint64) {
	f.r.nomMu.Lock()
	defer f.r.nomMu.Unlock()
	f.r.limit = height
}

// Ready reads the CLI's committed rows. The final horizon waits for the real
// default 30-second sweep to refresh open orders; its timing is not overridden.
func (f *RecoveryCLIFixture) Ready(ctx context.Context, height uint64) (bool, string) {
	var derived, index, cursor uint64
	var hash, sourceHash string
	err := f.read.QueryRowContext(ctx, "SELECT height,l2_hash,hash FROM momentums ORDER BY height DESC LIMIT 1").Scan(&derived, &hash, &sourceHash)
	if err != nil {
		return false, err.Error()
	}
	if err = f.read.QueryRowContext(ctx, "SELECT coalesce(max(number),0) FROM blocks").Scan(&index); err != nil {
		return false, err.Error()
	}
	if err = f.read.QueryRowContext(ctx, "SELECT value FROM nft_order_meta WHERE key=?", "scanned:"+f.r.scenario.exchange.Hex()).Scan(&cursor); err != nil {
		return false, err.Error()
	}
	f.r.nomMu.RLock()
	branch := f.r.branch
	expected := f.r.scenario.branches[branch][height]
	f.r.nomMu.RUnlock()
	if expected == nil {
		return false, "undeclared canonical source horizon"
	}
	ready := derived == height && index == height-1 && cursor == height-1 && sourceHash == expected.Momentum.Hash.String()
	var states []string
	for _, order := range f.r.scenario.orders {
		var status, checked string
		var filled, code int
		err = f.read.QueryRowContext(ctx, "SELECT status,filled,code,checked_hash FROM nft_orders WHERE hash=?", order.Hash().Hex()).Scan(&status, &filled, &code, &checked)
		if err != nil {
			return false, err.Error()
		}
		states = append(states, fmt.Sprintf("%s/%d/%d/checked=%t", status, filled, code, checked == hash))
		// The declared branch purchases this exact input at momentum 8.
		if height >= 8 && order.Hash() == f.r.scenario.inputs[branch].Order.Hash() {
			ready = ready && status == StatusFilled && filled == 1 && code == int(CodeFilled)
		} else {
			ready = ready && status == StatusOpen && filled == 0 && code == int(CodeOK)
			if height == 9 {
				ready = ready && checked == hash
			}
		}
	}
	return ready, fmt.Sprintf("derived=%d index=%d cursor=%d source_match=%t branch=%d orders=%s", derived, index, cursor, sourceHash == expected.Momentum.Hash.String(), branch, strings.Join(states, ";"))
}

func (f *RecoveryCLIFixture) Trigger(enabled bool) {
	f.r.t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.Join(f.r.path, "node.db")}
	q := uri.Query()
	q.Set("mode", "rw")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		f.r.t.Fatal(err)
	}
	defer db.Close()
	statement := "DROP TRIGGER execution_cli_exit"
	if enabled {
		statement = `CREATE TRIGGER execution_cli_exit AFTER UPDATE ON chunks WHEN NEW.consumed_at=8 BEGIN SELECT zvm_owned_cli_exit(); END`
	}
	if _, err := db.Exec(statement); err != nil {
		f.r.t.Fatal(err)
	}
}

// Snapshot is called only after the exact child has joined. It asserts all
// original balance/owner/order/chunk/deferred/receipt expectations and reads
// every canonical SQL table through a mode=ro connection, without repair.
func (f *RecoveryCLIFixture) Snapshot() map[string][]string {
	f.r.t.Helper()
	// Join any remaining owned RPC handlers without stopping/flushing the
	// engine. No further child may use this terminal fixture snapshot.
	f.r.web.Close()
	f.r.assertExpectedFrom(f.read)
	return f.r.stateFrom(f.read)
}

func AssertRecoveryCLIState(t *testing.T, got, want map[string][]string) {
	assertExecutionState(t, got, want)
}
