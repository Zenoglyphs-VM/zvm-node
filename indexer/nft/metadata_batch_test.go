package nft

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// This cursor exposes how many database rows the caller consumes. It also
// refuses writes before Close, matching the plugin's one-connection use case.
type metadataBatchDB struct {
	ids      []string
	read     int
	active   bool
	closed   bool
	failAt   int
	readErr  error
	closeErr error
	updated  []string
}

type metadataBatchConnector struct{ db *metadataBatchDB }
type metadataBatchDriver struct{ db *metadataBatchDB }
type metadataBatchConn struct{ db *metadataBatchDB }
type metadataBatchTx struct{}
type metadataBatchRows struct{ db *metadataBatchDB }

func (c metadataBatchConnector) Connect(context.Context) (driver.Conn, error) {
	return &metadataBatchConn{c.db}, nil
}
func (c metadataBatchConnector) Driver() driver.Driver { return metadataBatchDriver{c.db} }
func (d metadataBatchDriver) Open(string) (driver.Conn, error) {
	return &metadataBatchConn{d.db}, nil
}
func (c *metadataBatchConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared statement")
}
func (c *metadataBatchConn) Close() error              { return nil }
func (c *metadataBatchConn) Begin() (driver.Tx, error) { return metadataBatchTx{}, nil }
func (metadataBatchTx) Commit() error                  { return nil }
func (metadataBatchTx) Rollback() error                { return nil }
func (c *metadataBatchConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	if c.db.active {
		return nil, errors.New("metadata cursor is already active")
	}
	c.db.active = true
	return &metadataBatchRows{c.db}, nil
}
func (c *metadataBatchConn) ExecContext(_ context.Context, _ string, args []driver.NamedValue) (driver.Result, error) {
	if c.db.active || !c.db.closed {
		return nil, errors.New("metadata write started before the cursor closed")
	}
	if len(args) != 7 {
		return nil, errors.New("unexpected token update arguments")
	}
	id, ok := args[6].Value.(string)
	if !ok {
		return nil, errors.New("token ID is not a string")
	}
	c.db.updated = append(c.db.updated, id)
	return driver.RowsAffected(1), nil
}
func (r *metadataBatchRows) Columns() []string { return []string{"token_id"} }
func (r *metadataBatchRows) Close() error {
	r.db.active, r.db.closed = false, true
	return r.db.closeErr
}
func (r *metadataBatchRows) Next(dst []driver.Value) error {
	if r.db.readErr != nil && r.db.read == r.db.failAt {
		return r.db.readErr
	}
	if r.db.read == len(r.db.ids) {
		return io.EOF
	}
	dst[0] = r.db.ids[r.db.read]
	r.db.read++
	return nil
}

type metadataBatchHost struct{}

func (metadataBatchHost) Eth() *ethclient.Client { return nil }
func (metadataBatchHost) Log() *slog.Logger      { return slog.New(slog.DiscardHandler) }
func (metadataBatchHost) CallString(context.Context, common.Address, string) string {
	return ""
}
func (metadataBatchHost) CallURI(context.Context, common.Address, []byte) string {
	return `data:application/json,{"name":"stored metadata"}`
}

func metadataBatchIndex(t *testing.T, state *metadataBatchDB) (*Index, *sql.Tx) {
	t.Helper()
	db := sql.OpenDB(metadataBatchConnector{state})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	x := New(Config{})
	if err := x.Attach(metadataBatchHost{}); err != nil {
		t.Fatal(err)
	}
	return x, tx
}

func TestMetadataBatchStopsReadingAfterItsSelectionBound(t *testing.T) {
	// Preserve the real lexicographic SQL order, numeric filtering and first
	// 5,000 updates, including nonmatching and malformed rows in the cursor.
	state := &metadataBatchDB{ids: []string{"-1", "invalid", "9000"}}
	for id := 0; id < 8_000; id++ {
		state.ids = append(state.ids, strconv.Itoa(id))
	}
	sort.Strings(state.ids)
	from, to := big.NewInt(200), big.NewInt(7_000)
	var expected []string
	readBound := 0
	for i, raw := range state.ids {
		if id, ok := new(big.Int).SetString(raw, 10); ok && id.Cmp(from) >= 0 && id.Cmp(to) <= 0 {
			expected = append(expected, raw)
			if len(expected) == 5_001 {
				readBound = i + 1
				break
			}
		}
	}
	if readBound == 0 || readBound >= len(state.ids) {
		t.Fatal("fixture does not leave an unread cursor suffix")
	}
	x, tx := metadataBatchIndex(t, state)
	defer func(old time.Duration) { metadataBudget = old }(metadataBudget)
	metadataBudget = time.Hour
	if err := x.refreshRange(context.Background(), tx, common.Address{19: 1}, from, to, 7); err != nil {
		t.Fatal(err)
	}
	if state.read != readBound || !state.closed || state.active {
		t.Fatalf("consumed %d rows, want %d and a closed cursor", state.read, readBound)
	}
	if len(state.updated) != 5_000 {
		t.Fatalf("updated %d tokens, want 5000", len(state.updated))
	}
	for i, id := range state.updated {
		if id != expected[i] {
			t.Fatalf("update %d selected %s, want %s", i, id, expected[i])
		}
	}
}

func TestMetadataBatchRefusesIncompleteCursorBeforeAnyUpdate(t *testing.T) {
	for _, phase := range []string{"read", "close"} {
		t.Run(phase, func(t *testing.T) {
			want := errors.New("declared metadata cursor failure")
			state := &metadataBatchDB{ids: []string{"1", "2", "3"}, failAt: 2}
			if phase == "read" {
				state.readErr = want
			} else {
				state.closeErr = want
			}
			x, tx := metadataBatchIndex(t, state)
			err := x.refreshRange(context.Background(), tx, common.Address{19: 1}, big.NewInt(0), big.NewInt(10), 7)
			if !errors.Is(err, want) {
				t.Fatalf("returned %v, want the original cursor error", err)
			}
			if !state.closed || state.active || len(state.updated) != 0 {
				t.Fatal("an incomplete metadata cursor produced updates or remained open")
			}
		})
	}
}
