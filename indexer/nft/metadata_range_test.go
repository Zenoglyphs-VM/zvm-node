package nft

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

// This deliberately retains the old full-scan/parser selection as the oracle,
// independently of the new SQL predicate and its argument construction.
func oldMetadataRangeSelection(t *testing.T, db *sql.DB, address common.Address, from, to *big.Int) []string {
	t.Helper()
	rows, err := db.Query("SELECT token_id FROM nft_tokens WHERE collection=? ORDER BY token_id", address.Hex())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var selected []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if id, ok := new(big.Int).SetString(raw, 10); ok && id.Cmp(from) >= 0 && id.Cmp(to) <= 0 {
			selected = append(selected, id.String())
			if len(selected) == 5000 {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestPreparedURIRangeFilter(t *testing.T) {
	maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	for _, test := range []struct {
		name, from, to string
		ids            []string
	}{
		{"sparse", "7000", "7002", nil},
		{"cross-width", "9", "100", []string{"0", "8", "9", "10", "11", "99", "100", "101", "1000"}},
		{"uint256", maximum, maximum, []string{"1", "999999999999999999999999999999999999999999999999999999999999999999999999999999", maximum, new(big.Int).Lsh(big.NewInt(1), 256).String()}},
		{"whole-uint256", "0", maximum, []string{"0", "+0", "-0", "00", "1", "+1", maximum, "+" + maximum, new(big.Int).Lsh(big.NewInt(1), 256).String(), "1\x009", "x"}},
		{"legacy", "0", "11", []string{"0", "-0", "+0", "00", "01", "+1", "-1", "1", "2", "9", "10", "11", "100", "0010", "+10", "1_0", "x", "１２", "1\x009"}},
		{"zero", "0", "0", []string{"0", "-0", "+0", "00", "1"}},
		{"non-text", "0", "11", []string{"0", "+2", "7", "10", "100"}},
		{"reversed", "20", "10", []string{"0", "10", "15", "20", "100", "0015"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			from, _ := new(big.Int).SetString(test.from, 10)
			to, _ := new(big.Int).SetString(test.to, 10)
			address, hash := common.Address{19: 21}, common.Hash{31: 11}
			seed := func() (*Index, *sql.DB, *preparedRangeHost) {
				x, db, host := uriHistoryFixture(t, false)
				uriBlock(t, db, 1, hash)
				host.values[hash] = uriDocument("filtered")
				recorder := &preparedRangeHost{uriHistoryHost: host}
				if err := x.Attach(recorder); err != nil {
					t.Fatal(err)
				}
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				ids := test.ids
				if test.name == "sparse" {
					for i := 1; i <= 8192; i++ {
						ids = append(ids, fmt.Sprint(i))
					}
				}
				for _, id := range ids {
					var stored any = id
					if test.name == "non-text" {
						stored = []byte(id)
					}
					if _, err := tx.Exec("INSERT INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'existing',1,1)", address.Hex(), stored); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.Exec("INSERT INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'other',1,1)", common.Address{19: 99}.Hex(), test.from); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				return x, db, recorder
			}
			direct, db, recorder := seed()
			want := oldMetadataRangeSelection(t, db, address, from, to)
			statement, args := metadataRangeQuery(address, from, to, false)
			rows, err := db.QueryContext(context.Background(), statement, args...)
			if err != nil {
				t.Fatal(err)
			}
			candidates := 0
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				candidates++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if test.name == "sparse" {
				if candidates != 3 {
					t.Fatalf("narrow query returned %d candidates, want 3 of 8192", candidates)
				}
				t.Log("canonical sparse fixture: 8192 collection rows; 3 driver candidates; no elapsed-time or RSS claim")
			}
			row := indexer.LogRow{Address: address, Block: 1, BlockHash: hash, Topics: []common.Hash{topicBatchMetadataUpdate}, Data: append(common.LeftPadBytes(from.Bytes(), 32), common.LeftPadBytes(to.Bytes(), 32)...)}
			if err := applyPreparedURITest(direct, db, nil, []indexer.LogRow{row}); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(recorder.ids) != fmt.Sprint(want) {
				t.Fatalf("direct calls %v differ from old scan %v", recorder.ids, want)
			}
			prepared, preparedDB, preparedHost := seed()
			p := collectPreparedURITest(t, prepared, preparedDB, []indexer.LogRow{row})
			if err := p.Resolve(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(preparedHost.ids) != fmt.Sprint(want) {
				t.Fatalf("discovery calls %v differ from old scan %v", preparedHost.ids, want)
			}
			if err := applyPreparedURITest(prepared, preparedDB, p, []indexer.LogRow{row}); err != nil {
				t.Fatal(err)
			}
			if got, want := uriHistoryDump(t, preparedDB), uriHistoryDump(t, db); got != want {
				t.Fatalf("filtered prepared/direct all-table replay differs\ngot %s\nwant %s", got, want)
			}
		})
	}
	for _, discovery := range []bool{false, true} {
		name := "cancelled-live"
		if discovery {
			name = "cancelled-discovery"
		}
		t.Run(name, func(t *testing.T) {
			x, db, host := uriHistoryFixture(t, false)
			before := uriHistoryDump(t, db)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if discovery {
				p, openErr := newPreparedURI(context.Background(), x, preparedURIBytes)
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer p.Close()
				p.collect, openErr = p.db.Begin()
				if openErr != nil {
					t.Fatal(openErr)
				}
				err = p.loadRange(ctx, tx, common.Address{19: 21}, big.NewInt(0), big.NewInt(1))
			} else {
				err = x.refreshRange(ctx, tx, common.Address{19: 21}, big.NewInt(0), big.NewInt(1), 1)
			}
			tx.Rollback()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled range returned %v", err)
			}
			if host.latest != 0 || uriHistoryDump(t, db) != before {
				t.Fatal("cancelled query changed the canonical store or called latest URI")
			}
		})
	}
}
