package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite"
)

const (
	alice = "0xA000000000000000000000000000000000000001"
	bob   = "0xB000000000000000000000000000000000000002"
	carol = "0xC000000000000000000000000000000000000003"
)

func TestReplayERC20(t *testing.T) {
	rows, held := replayHolders("erc20", []transferEvent{
		{from: zeroHex, to: alice, value: "1000"},
		{from: alice, to: bob, value: "300"},
		{from: bob, to: carol, value: "300"}, // bob empties out and is no holder
		{from: alice, to: zeroHex, value: "100"},
		{from: alice, to: bob, value: "not a number"},
	})
	if held.String() != "900" || len(rows) != 2 {
		t.Fatalf("held %s, rows %+v", held, rows)
	}
	if rows[0] != (holderRow{Address: alice, Quantity: "600"}) || rows[1] != (holderRow{Address: carol, Quantity: "300"}) {
		t.Fatalf("rows %+v", rows)
	}
}

func TestReplayERC721(t *testing.T) {
	rows, held := replayHolders("erc721", []transferEvent{
		{from: zeroHex, to: alice, id: "1"},
		{from: zeroHex, to: alice, id: "2"},
		{from: zeroHex, to: bob, id: "3"},
		{from: alice, to: bob, id: "1"},
		{from: bob, to: zeroHex, id: "3"},
	})
	if held.String() != "2" || len(rows) != 2 {
		t.Fatalf("held %s, rows %+v", held, rows)
	}
	// One token each: ties sort by address.
	if rows[0].Address != alice || rows[0].Tokens != 1 || rows[1].Address != bob || rows[1].Tokens != 1 {
		t.Fatalf("rows %+v", rows)
	}
}

func TestReplayERC1155(t *testing.T) {
	rows, held := replayHolders("erc1155", []transferEvent{
		{from: zeroHex, to: alice, id: "7", value: "5"},
		{from: zeroHex, to: alice, id: "8", value: "2"},
		{from: alice, to: bob, id: "7", value: "5"},
	})
	if held.String() != "7" || len(rows) != 2 {
		t.Fatalf("held %s, rows %+v", held, rows)
	}
	if rows[0] != (holderRow{Address: bob, Quantity: "5", Tokens: 1}) || rows[1] != (holderRow{Address: alice, Quantity: "2", Tokens: 1}) {
		t.Fatalf("rows %+v", rows)
	}
}

func TestOwnHoldings(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE token_transfers (tx TEXT, idx INTEGER, block INTEGER, timestamp INTEGER,
		token TEXT, from_addr TEXT, to_addr TEXT, value TEXT, token_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	add := func(block int, token, from, to, value, id string) {
		if _, err := db.Exec(`INSERT INTO token_transfers VALUES ('t', 0, ?, 0, ?, ?, ?, ?, ?)`, block, token, from, to, value, id); err != nil {
			t.Fatal(err)
		}
	}
	const nft, multi = "0xN", "0xM"
	add(1, nft, zeroHex, alice, "", "1")
	add(2, nft, zeroHex, alice, "", "2")
	add(3, nft, alice, bob, "", "1")
	add(4, nft, bob, alice, "", "1")
	add(5, nft, alice, carol, "", "2")
	add(1, multi, zeroHex, alice, "10", "7")
	add(2, multi, zeroHex, alice, "5", "8")
	add(3, multi, alice, bob, "5", "8")

	s := &Server{db: db}
	who := common.HexToAddress(alice)
	h, err := s.ownHoldings(context.Background(), "erc721", nft, who)
	if err != nil || h.quantity.String() != "1" || h.ids != 1 {
		t.Fatalf("erc721: %+v %v", h, err)
	}
	h, err = s.ownHoldings(context.Background(), "erc1155", multi, who)
	if err != nil || h.quantity.String() != "10" || h.ids != 1 {
		t.Fatalf("erc1155: %+v %v", h, err)
	}
}

func TestTxHistory(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE blocks (number INTEGER PRIMARY KEY, timestamp INTEGER, user_txs INTEGER)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	add := func(number int, ago time.Duration, txs int) {
		if _, err := db.Exec("INSERT INTO blocks VALUES (?,?,?)", number, now-int64(ago.Seconds()), txs); err != nil {
			t.Fatal(err)
		}
	}
	add(1, 48*time.Hour, 100) // older than the window: counted in the total, not the series
	add(2, 20*time.Hour, 5)
	add(3, 10*time.Hour, 7)
	add(4, 1*time.Hour, 3)

	s := &Server{db: db}
	req := httptest.NewRequest(http.MethodGet, "/api/stats/txhistory?hours=24&buckets=24", nil)
	out, err := s.txHistory(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if res["total"] != int64(115) {
		t.Fatalf("total = %v, want 115", res["total"])
	}
	if res["windowTxs"] != int64(15) {
		t.Fatalf("windowTxs = %v, want 15", res["windowTxs"])
	}
	series := res["series"].([]map[string]any)
	if len(series) == 0 {
		t.Fatal("empty series")
	}
	last := series[len(series)-1]
	if last["total"] != int64(115) {
		t.Fatalf("last cumulative point = %v, want 115 (running total should end at the grand total)", last["total"])
	}
	for i := 1; i < len(series); i++ {
		if series[i]["total"].(int64) < series[i-1]["total"].(int64) {
			t.Fatal("cumulative total went backwards")
		}
	}
}

func TestTokensByHolders(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE token_transfers (tx TEXT, idx INTEGER, block INTEGER, timestamp INTEGER,
		token TEXT, from_addr TEXT, to_addr TEXT, value TEXT, token_id TEXT);
		CREATE TABLE tokens (address TEXT PRIMARY KEY, kind TEXT, name TEXT, symbol TEXT, decimals INTEGER,
		first_block INTEGER, transfers INTEGER)`); err != nil {
		t.Fatal(err)
	}
	idx := 0
	add := func(token string, to ...string) {
		for _, a := range to {
			idx++
			if _, err := db.Exec(`INSERT INTO token_transfers VALUES ('t', ?, 1, 0, ?, ?, ?, '1', '')`, idx, token, zeroHex, a); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO tokens VALUES (?, 'erc20', ?, ?, 18, 1, ?)`, token, token, token, len(to)); err != nil {
			t.Fatal(err)
		}
	}
	add("0xBUSY", alice, alice, alice, alice, alice) // 5 transfers, 1 holder
	add("0xWIDE", alice, bob, carol, carol)          // 4 transfers, 3 holders
	add("0xPAIR", alice, bob, bob)                   // 3 transfers, 2 holders
	add("0xTINY", alice, bob)                        // 2 transfers: cannot beat PAIR, never replayed

	s := &Server{db: db}
	req := httptest.NewRequest(http.MethodGet, "/api/tokens?sort=holders&limit=2", nil)
	out, err := s.tokens(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	v := reflect.ValueOf(out)
	for i := 0; i < v.Len(); i++ {
		got += fmt.Sprintf("%s:%d ", v.Index(i).FieldByName("Address"), v.Index(i).FieldByName("Holders").Int())
	}
	if got != "0xWIDE:3 0xPAIR:2 " {
		t.Fatalf("got %q", got)
	}
	if _, replayed := s.holders.Load("0xTINY"); replayed {
		t.Fatal("0xTINY was replayed though its transfers could not reach the cut")
	}
}
