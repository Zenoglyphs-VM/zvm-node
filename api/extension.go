package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-core/cli"
)

// What an extension needs to serve routes in the same shape as the node's own:
// the same JSON envelope, the same timeout, and the same mapping from an error
// to a status code. Without these an extension re-implements them slightly
// differently and its 404 arrives as a 500.

// HandlerFunc is a route: it returns the value to encode, or an error wrapping
// ErrNotFound or ErrBadRequest.
type HandlerFunc = handlerFunc

var (
	// ErrNotFound and ErrBadRequest are what a handler wraps to choose a
	// status: `fmt.Errorf("%w: no such collection", api.ErrNotFound)`.
	ErrNotFound   = errNotFound
	ErrBadRequest = errBadRequest
)

// Handle wraps a route the way the node wraps its own: a 20-second deadline,
// JSON out, and 404 or 400 for the two sentinel errors.
func Handle(fn HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		v, err := fn(ctx, r)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, errBadRequest) {
				status = http.StatusBadRequest
			}
			WriteJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, v)
	}
}

// WriteJSON is the node's reply shape.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// ParseAddress accepts either spelling an address has on this chain — a 0x EVM
// address or the z1 Zenon address whose 20 bytes it is — and reports a bad one
// as ErrBadRequest rather than a 500.
func ParseAddress(s string) (common.Address, error) {
	a, err := cli.ParseEVMOrNoM(s)
	if err != nil {
		return common.Address{}, fmt.Errorf("%w: %s", errBadRequest, err)
	}
	return a, nil
}

// IntParam reads a positive integer query parameter, falling back to def and
// clamping to maxV, so a page cannot ask for a million rows.
func IntParam(r *http.Request, name string, def, maxV int) int {
	return intParam(r, name, def, maxV)
}

// NullStr encodes a nullable column as null rather than "".
func NullStr(s sql.NullString) any { return nullStr(s) }

// ScanTransferRow scans the token-transfer projection the node's own address
// and token pages use, so an extension showing the same rows shows the same
// fields under the same names.
func ScanTransferRow(rows *sql.Rows) (any, error) { return scanTransferRow(rows) }

// QueryList runs a query and scans every row, returning an empty slice rather
// than nil so that an empty result encodes as [] and not null.
func QueryList(db *sql.DB, q string, args []any, scan func(*sql.Rows) (any, error)) ([]any, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
