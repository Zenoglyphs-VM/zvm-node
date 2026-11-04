package orderbook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func timeBoundaries() []uint64 {
	return []uint64{0, 1, maxSafeJSONTime - 1, maxSafeJSONTime, maxSafeJSONTime + 1,
		math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64 - 1, math.MaxUint64}
}

func TestWireOrderTimesRetainOriginalInputAndHash(t *testing.T) {
	for _, value := range timeBoundaries() {
		t.Run(strconv.FormatUint(value, 10), func(t *testing.T) {
			root := common.HexToHash("0x1234")
			in := &Input{Order: Order{Trader: common.Address{19: 1}, Side: SideSell, Collection: common.Address{19: 2},
				TokenID: U(1), Amount: U(1), Price: U(17), ListingTime: value, ExpirationTime: value, Nonce: U(0), Salt: U(7)},
				Kind: KindBulk, Signature: hexutil.Bytes{1, 2, 3}, Proof: []common.Hash{{31: 4}}, Root: &root}
			original, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(ToWireInput(in))
			if err != nil {
				t.Fatal(err)
			}
			var fields struct {
				Order map[string]json.RawMessage `json:"order"`
			}
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			decimal := strconv.FormatUint(value, 10)
			if value > maxSafeJSONTime {
				decimal = "\"" + decimal + "\""
			}
			for _, name := range []string{"listingTime", "expirationTime"} {
				if string(fields.Order[name]) != decimal {
					t.Fatalf("%s lost its exact JSON representation", name)
				}
			}
			var wire WireInput
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			got := wire.Input()
			again, err := json.Marshal(got)
			if err != nil || !bytes.Equal(again, original) || got.Order.Hash() != in.Order.Hash() {
				t.Fatal("wire conversion changed original order, signature, proof, root or hash")
			}
			if value <= maxSafeJSONTime && !bytes.Equal(raw, original) {
				t.Fatal("ordinary legacy response bytes changed")
			}
		})
	}
}

func TestWireOrderTimesRefuseAmbiguousJSON(t *testing.T) {
	values := []string{"null", "true", "-1", "1.5", "1e3", "9007199254740992", "18446744073709551615",
		`""`, `"-1"`, `"1.5"`, `"1e3"`, `"18446744073709551616"`, `"000000000000000000000"`}
	for i, raw := range values {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			value := wireTime(7)
			if json.Unmarshal([]byte(raw), &value) == nil {
				t.Fatal("ambiguous or out-of-range order time was accepted")
			}
			if value != 7 {
				t.Fatal("a refused timestamp mutated its destination")
			}
		})
	}
}

func TestSQLiteOrderTimesRetainUnsignedRangeAndOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "times.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	if _, err := db.Exec("CREATE TABLE times (value INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	values := timeBoundaries()
	state := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 64; i++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values = append(values, state)
	}
	for _, value := range values {
		if _, err := db.Exec("INSERT INTO times(value) VALUES (?)", TimeValue(value)); err != nil {
			t.Fatal(err)
		}
	}
	// Reopen actual SQLite storage; this is not an in-memory conversion test.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	rows, err := db.Query("SELECT value,typeof(value) FROM times ORDER BY value")
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for rows.Next() {
		var raw any
		var kind string
		if err := rows.Scan(&raw, &kind); err != nil {
			t.Fatal(err)
		}
		got, err := TimeFromStorage(raw)
		if err != nil || i >= len(values) || got != values[i] {
			t.Fatalf("unsigned SQL ordering or persisted value differs at %d", i)
		}
		wantKind := "integer"
		if got > math.MaxInt64 {
			wantKind = "text"
		}
		if kind != wantKind {
			t.Fatalf("time was stored as %s instead of %s", kind, wantKind)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if i != len(values) {
		t.Fatal("reopened SQLite lost a timestamp")
	}
	for _, cutoff := range timeBoundaries() {
		rows, err := db.Query("SELECT value FROM times WHERE value<=? ORDER BY value", TimeValue(cutoff))
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var got StoredTime
			if err := rows.Scan(&got); err != nil {
				t.Fatal(err)
			}
			if count >= len(values) || uint64(got) != values[count] || uint64(got) > cutoff {
				t.Fatal("SQL time cutoff differs from unsigned comparison")
			}
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		want := sort.Search(len(values), func(i int) bool { return values[i] > cutoff })
		if count != want {
			t.Fatal("SQL time cutoff lost a high-range timestamp")
		}
	}
}

func TestStoredOrderTimesRefuseLossyOrNoncanonicalValues(t *testing.T) {
	for _, raw := range []any{nil, []byte("u18446744073709551615"), int64(-1), float64(9007199254740992), "18446744073709551615", "u18446744073709551616",
		"u00000000000000000001", "u9223372036854775808", "u1844674407370955161x"} {
		value := StoredTime(7)
		if value.Scan(raw) == nil || value != 7 {
			t.Fatal("a malformed stored timestamp was accepted or mutated the scanner")
		}
	}
}
