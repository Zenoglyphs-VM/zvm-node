package api

// Token pages: holders and transfers, from the indexed Transfer events.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

type holderRow struct {
	Address  string `json:"address"`
	Quantity string `json:"quantity"`
	// Tokens is how many distinct ids an ERC-721 or ERC-1155 holder has.
	Tokens int `json:"tokens,omitempty"`
}

type transferEvent struct {
	from, to, value, id string
}

type holderSet struct {
	lastBlock int64
	transfers int64
	rows      []holderRow
	held      *big.Int
}

var zeroHex = common.Address{}.Hex()

// replayHolders turns a token's transfers, oldest first, into what each
// address holds now. Mints come from the zero address and burns go to it, so
// neither is a holder. It is only as right as the events: a token that moves
// balances without a Transfer event reads wrong here, as on any explorer that
// indexes events.
func replayHolders(kind string, events []transferEvent) ([]holderRow, *big.Int) {
	held := new(big.Int)
	var rows []holderRow
	switch kind {
	case "erc721":
		owners := map[string]string{}
		for _, e := range events {
			if e.to == zeroHex {
				delete(owners, e.id)
			} else {
				owners[e.id] = e.to
			}
		}
		counts := map[string]int{}
		for _, owner := range owners {
			counts[owner]++
		}
		for addr, n := range counts {
			rows = append(rows, holderRow{Address: addr, Quantity: strconv.Itoa(n), Tokens: n})
		}
		held.SetInt64(int64(len(owners)))

	case "erc1155":
		balances := map[string]map[string]*big.Int{}
		move := func(addr, id string, v *big.Int, sign int) {
			if addr == zeroHex {
				return
			}
			if balances[addr] == nil {
				balances[addr] = map[string]*big.Int{}
			}
			b := balances[addr][id]
			if b == nil {
				b = new(big.Int)
				balances[addr][id] = b
			}
			if sign < 0 {
				b.Sub(b, v)
			} else {
				b.Add(b, v)
			}
		}
		for _, e := range events {
			v, ok := new(big.Int).SetString(e.value, 10)
			if !ok {
				continue
			}
			move(e.from, e.id, v, -1)
			move(e.to, e.id, v, 1)
		}
		for addr, ids := range balances {
			total, n := new(big.Int), 0
			for _, b := range ids {
				if b.Sign() > 0 {
					total.Add(total, b)
					n++
				}
			}
			if n > 0 {
				rows = append(rows, holderRow{Address: addr, Quantity: total.String(), Tokens: n})
				held.Add(held, total)
			}
		}

	default:
		balances := map[string]*big.Int{}
		for _, e := range events {
			v, ok := new(big.Int).SetString(e.value, 10)
			if !ok {
				continue
			}
			if e.from != zeroHex {
				if balances[e.from] == nil {
					balances[e.from] = new(big.Int)
				}
				balances[e.from].Sub(balances[e.from], v)
			}
			if e.to != zeroHex {
				if balances[e.to] == nil {
					balances[e.to] = new(big.Int)
				}
				balances[e.to].Add(balances[e.to], v)
			}
		}
		for addr, b := range balances {
			if b.Sign() > 0 {
				rows = append(rows, holderRow{Address: addr, Quantity: b.String()})
				held.Add(held, b)
			}
		}
	}

	quantities := make(map[string]*big.Int, len(rows))
	for _, r := range rows {
		quantities[r.Address], _ = new(big.Int).SetString(r.Quantity, 10)
	}
	sort.Slice(rows, func(i, j int) bool {
		if c := quantities[rows[i].Address].Cmp(quantities[rows[j].Address]); c != 0 {
			return c > 0
		}
		return rows[i].Address < rows[j].Address
	})
	if rows == nil {
		rows = []holderRow{}
	}
	return rows, held
}

// holdersOf replays a token's transfers, reusing the last answer until a
// transfer is added or rewound away.
func (s *Server) holdersOf(ctx context.Context, token, kind string) (*holderSet, error) {
	var last sql.NullInt64
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT MAX(block), COUNT(*) FROM token_transfers WHERE token = ?", token).Scan(&last, &count); err != nil {
		return nil, err
	}
	if v, ok := s.holders.Load(token); ok {
		if h := v.(*holderSet); h.lastBlock == last.Int64 && h.transfers == count {
			return h, nil
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT from_addr, to_addr, COALESCE(value, ''), COALESCE(token_id, '')
		FROM token_transfers WHERE token = ? ORDER BY block, idx`, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]transferEvent, 0, count)
	for rows.Next() {
		var e transferEvent
		if err := rows.Scan(&e.from, &e.to, &e.value, &e.id); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list, held := replayHolders(kind, events)
	h := &holderSet{lastBlock: last.Int64, transfers: count, rows: list, held: held}
	s.holders.Store(token, h)
	return h, nil
}

// tokenNames answers, for up to 100 addresses, the name and symbol of each
// that the indexer already knows as a token. Only those are in the answer —
// mirrors verifiedMarks (contracts.go), which the explorer's AddressLink
// batches alongside this to label a page's addresses in one round trip each.
func (s *Server) tokenNames(_ context.Context, r *http.Request) (any, error) {
	type tokenName struct {
		Name   string `json:"name"`
		Symbol string `json:"symbol"`
	}
	out := map[string]tokenName{}
	seen := map[common.Address]bool{}
	for _, raw := range strings.Split(r.URL.Query().Get("addresses"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := parseAddress(raw)
		if err != nil {
			return nil, err
		}
		if seen[addr] {
			continue
		}
		if seen[addr] = true; len(seen) > 100 {
			return nil, fmt.Errorf("%w: at most 100 addresses", errBadRequest)
		}
		var name, symbol string
		if err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(symbol,'') FROM tokens WHERE address = ?`, addr.Hex()).
			Scan(&name, &symbol); err == nil && (name != "" || symbol != "") {
			out[addr.Hex()] = tokenName{Name: name, Symbol: symbol}
		}
	}
	return out, nil
}

func (s *Server) tokenHolders(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	var kind string
	err = s.db.QueryRowContext(ctx, "SELECT kind FROM tokens WHERE address = ?", addr.Hex()).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	h, err := s.holdersOf(ctx, addr.Hex(), kind)
	if err != nil {
		return nil, err
	}
	list := h.rows
	if limit := intParam(r, "limit", 50, 1000); len(list) > limit {
		list = list[:limit]
	}
	return map[string]any{"kind": kind, "holders": len(h.rows), "held": h.held.String(), "rows": list}, nil
}

// tokenTransfers pages through a token's transfers, newest first, with the
// transaction each came from: its lane, and the contract and selector it called.
func (s *Server) tokenTransfers(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	q := `SELECT t.tx, t.idx, t.block, t.timestamp, t.from_addr, t.to_addr, t.value, t.token_id, x.selector, x.to_addr, x.lane
		FROM token_transfers t LEFT JOIN txs x ON x.hash = t.tx WHERE t.token = ?`
	args := []any{addr.Hex()}
	if before := r.URL.Query().Get("before"); before != "" {
		b, i, ok := strings.Cut(before, ":")
		block, err1 := strconv.ParseInt(b, 10, 64)
		idx, err2 := strconv.ParseInt(i, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%w: before is block:index", errBadRequest)
		}
		q += " AND (t.block < ? OR (t.block = ? AND t.idx < ?))"
		args = append(args, block, block, idx)
	}
	q += " ORDER BY t.block DESC, t.idx DESC LIMIT ?"
	args = append(args, intParam(r, "limit", 50, 200))
	return s.queryList(q, args, func(rows *sql.Rows) (any, error) {
		var tx, from, to string
		var idx, block, ts int64
		var value, tokenID, selector, txTo, lane sql.NullString
		if err := rows.Scan(&tx, &idx, &block, &ts, &from, &to, &value, &tokenID, &selector, &txTo, &lane); err != nil {
			return nil, err
		}
		return map[string]any{
			"tx": tx, "index": idx, "block": block, "timestamp": ts, "from": from, "to": to,
			"value": nullStr(value), "tokenId": nullStr(tokenID),
			"selector": nullStr(selector), "txTo": nullStr(txTo), "lane": nullStr(lane),
		}, nil
	})
}
