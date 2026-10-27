package market

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	nodeapi "github.com/Zenoglyphs-VM/zvm-node/api"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-node/orderbook"
)

// The NFT market: collections and sales from the index, orders from the order
// book (D28).
//
// An order is shown only while it can be filled. The book re-checks every one
// against the exchange on a timer and whenever the exchange names it; between
// those checks a listing can still go stale by its token moving, so the queries
// below also require that the index still has the seller holding the token and
// approving the exchange — which is what the exchange itself checks at fill.

const zeroAddr = "0x0000000000000000000000000000000000000000"

// listingValid is a fillable listing, aliased o. Its arguments are
// listingArgs().
const listingValid = `o.side = 0 AND o.status = 'open' AND o.code = 0 AND o.listing_time <= ? AND o.expiration_time > ?
	AND EXISTS (SELECT 1 FROM nft_tokens t WHERE t.collection = o.collection AND t.token_id = o.token_id
		AND t.owner = o.trader AND (t.approved = o.exchange
			OR EXISTS (SELECT 1 FROM nft_approvals a WHERE a.owner = o.trader AND a.collection = o.collection
				AND a.operator = o.exchange AND a.approved = 1)))`

// bidValid is a fillable bid, aliased b. Its arguments are listingArgs().
const bidValid = `b.side = 1 AND b.status = 'open' AND b.code = 0 AND b.listing_time <= ? AND b.expiration_time > ?
	AND b.filled < b.amount`

func listingArgs() []any {
	now := time.Now().Unix()
	return []any{now, now}
}

func rawJSON(s string) any {
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

func nullFloat(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

// --- the exchange and its book ------------------------------------------------

// nftExchange tells a wallet what to sign: the exchange's EIP-712 domain and
// types, as eth_signTypedData_v4 takes them, and what bids are paid in.
func (s *Market) nftExchange(_ context.Context, _ *http.Request) (any, error) {
	d := s.cfg.Apps.Get()
	if d == nil || d.Exchange == (common.Address{}) {
		return nil, fmt.Errorf("%w: no exchange is deployed on this chain", nodeapi.ErrNotFound)
	}
	out := map[string]any{
		"exchange":   d.Exchange,
		"wrappedZnn": d.WrappedZNN,
		"domain": map[string]any{
			"name": orderbook.DomainName, "version": orderbook.DomainVersion,
			"chainId": s.cfg.Rollup.L2ChainID, "verifyingContract": d.Exchange,
		},
		"domainSeparator": orderbook.Domain(s.cfg.Rollup.L2ChainID, d.Exchange),
		"types": map[string]any{
			"Order": []map[string]string{
				{"name": "trader", "type": "address"}, {"name": "side", "type": "uint8"},
				{"name": "collection", "type": "address"}, {"name": "tokenId", "type": "uint256"},
				{"name": "anyToken", "type": "bool"}, {"name": "amount", "type": "uint256"},
				{"name": "paymentToken", "type": "address"}, {"name": "price", "type": "uint256"},
				{"name": "listingTime", "type": "uint64"}, {"name": "expirationTime", "type": "uint64"},
				{"name": "nonce", "type": "uint256"}, {"name": "salt", "type": "uint256"},
			},
			"OrderRoot": []map[string]string{{"name": "root", "type": "bytes32"}},
		},
		"kinds": map[string]uint8{"single": orderbook.KindSingle, "bulk": orderbook.KindBulk, "onChain": orderbook.KindOnChain},
	}
	if s.cfg.Orders != nil {
		out["book"] = s.cfg.Orders.Stats()
	}
	return out, nil
}

// nftSubmitOrders takes signed orders — one, or a bulk listing's many — and
// keeps the ones the exchange says can be filled.
func (s *Market) nftSubmitOrders(ctx context.Context, r *http.Request) (any, error) {
	if s.cfg.Orders == nil {
		return nil, fmt.Errorf("%w: this node keeps no order book", nodeapi.ErrNotFound)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var body struct {
		Orders []*orderbook.WireInput `json:"orders"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%w: %v", nodeapi.ErrBadRequest, err)
	}
	inputs := make([]*orderbook.Input, len(body.Orders))
	for i, in := range body.Orders {
		if in == nil {
			return nil, fmt.Errorf("%w: order %d is empty", nodeapi.ErrBadRequest, i)
		}
		inputs[i] = in.Input()
	}
	results, err := s.cfg.Orders.Submit(ctx, inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", nodeapi.ErrBadRequest, err)
	}
	return map[string]any{"results": results}, nil
}

// nftOrder is one order with everything a fill sends to the exchange.
func (s *Market) nftOrder(_ context.Context, r *http.Request) (any, error) {
	if s.cfg.Orders == nil {
		return nil, nodeapi.ErrNotFound
	}
	h := r.PathValue("hash")
	if !strings.HasPrefix(h, "0x") || len(h) != 66 {
		return nil, fmt.Errorf("%w: an order hash is 32 bytes", nodeapi.ErrBadRequest)
	}
	in, status, code, err := s.cfg.Orders.Get(common.HexToHash(h))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nodeapi.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"hash": in.Order.Hash(), "input": orderbook.ToWireInput(in), "status": status, "code": code, "reason": orderbook.CodeText(code),
		"fillable": status == orderbook.StatusOpen && code == orderbook.CodeOK,
	}, nil
}

// nftOrders lists orders by trader, collection or token. By default only the
// fillable ones; ?all=1 includes the rest, with the reason each is not.
func (s *Market) nftOrders(_ context.Context, r *http.Request) (any, error) {
	q := r.URL.Query()
	where := []string{"1=1"}
	var args []any
	for _, f := range []struct{ param, col string }{{"trader", "o.trader"}, {"collection", "o.collection"}} {
		if v := q.Get(f.param); v != "" {
			addr, err := nodeapi.ParseAddress(v)
			if err != nil {
				return nil, err
			}
			where, args = append(where, f.col+" = ?"), append(args, addr.Hex())
		}
	}
	if v := q.Get("tokenId"); v != "" {
		where, args = append(where, "(o.token_id = ? OR o.token_id IS NULL)"), append(args, v)
	}
	switch q.Get("side") {
	case "sell", "0":
		where = append(where, "o.side = 0")
	case "buy", "1":
		where = append(where, "o.side = 1")
	}
	if q.Get("all") != "1" {
		where, args = append(where, "o.status = 'open' AND o.code = 0 AND o.listing_time <= ? AND o.expiration_time > ?"),
			append(args, listingArgs()...)
	}
	return s.orderRows(strings.Join(where, " AND "), args, "o.block DESC, o.hash", nodeapi.IntParam(r, "limit", 50, 500))
}

// orderRows is the book's rows with the names a page shows beside them.
func (s *Market) orderRows(where string, args []any, order string, limit int) ([]any, error) {
	q := `SELECT o.hash, o.exchange, o.trader, o.side, o.collection, o.token_id, o.amount, o.filled, o.payment_token,
		o.price, o.price_num, o.listing_time, o.expiration_time, o.kind, o.status, o.code, o.block, o.timestamp, o.source,
		COALESCE(t.name,''), COALESCE(t.image,''), COALESCE(c.name,'')
		FROM nft_orders o
		LEFT JOIN nft_tokens t ON t.collection = o.collection AND t.token_id = o.token_id
		LEFT JOIN nft_collections c ON c.address = o.collection
		WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?`
	return nodeapi.QueryList(s.db, q, append(args, limit), scanOrderRow)
}

func scanOrderRow(rows *sql.Rows) (any, error) {
	var hash, exchange, trader, collection, payment, price, status, source, name, image, collectionName string
	var tokenID sql.NullString
	var side, amount, filled, kind, code, block, ts int64
	var opens, expires orderbook.StoredTime
	var priceNum float64
	if err := rows.Scan(&hash, &exchange, &trader, &side, &collection, &tokenID, &amount, &filled, &payment,
		&price, &priceNum, &opens, &expires, &kind, &status, &code, &block, &ts, &source,
		&name, &image, &collectionName); err != nil {
		return nil, err
	}
	out := map[string]any{
		"hash": hash, "exchange": exchange, "trader": trader, "side": map[int64]string{0: "sell", 1: "buy"}[side],
		"collection": collection, "collectionName": collectionName, "tokenId": nodeapi.NullStr(tokenID),
		"isCollection": !tokenID.Valid, "amount": amount, "filled": filled, "paymentToken": payment,
		"price": price, "priceNum": priceNum, "listingTime": orderbook.TimeJSON(uint64(opens)), "expiry": orderbook.TimeJSON(uint64(expires)), "kind": kind,
		"status": status, "code": code, "block": block, "timestamp": ts, "source": source,
		"name": name, "image": image,
	}
	if code != 0 {
		out["reason"] = orderbook.CodeText(uint8(code))
	}
	// The names the pages read listings and offers by before orders were
	// signed: who is selling or bidding, and in what.
	out["currency"] = payment
	if side == 0 {
		out["seller"] = trader
	} else {
		out["bidder"] = trader
		out["quantity"] = amount
	}
	return out, nil
}

// --- collections ----------------------------------------------------------

type collectionRow struct {
	Address     string  `json:"address"`
	Name        string  `json:"name"`
	Symbol      string  `json:"symbol"`
	Creator     string  `json:"creator,omitempty"`
	Factory     string  `json:"factory,omitempty"`
	ContractURI string  `json:"contractUri,omitempty"`
	Meta        any     `json:"meta,omitempty"`
	FirstBlock  int64   `json:"firstBlock"`
	Items       int64   `json:"items"`
	Owners      int64   `json:"owners"`
	Sales       int64   `json:"sales"`
	Sales24h    int64   `json:"sales24h"`
	Volume      float64 `json:"volume"`
	Volume24h   float64 `json:"volume24h"`
	Floor       any     `json:"floor"`
	Listed      int64   `json:"listed"`
	TopBid      any     `json:"topBid"`
}

func (s *Market) collectionRows(where string, args []any) ([]*collectionRow, error) {
	now := time.Now().Unix()
	day := now - 86400
	// items, owners, sales and volume come from nft_collection_stats, which
	// the indexer recomputes for a collection when a block touches it (D21).
	// The last day's figures stay a query, because a moving window cannot be a
	// running total — but it is one indexed scan of a day of sales, not of
	// every sale a collection ever had.
	q := `SELECT c.address, COALESCE(c.name,''), COALESCE(c.symbol,''), COALESCE(c.creator,''), COALESCE(c.factory,''),
		COALESCE(c.contract_uri,''), COALESCE(c.meta,''), c.first_block,
		COALESCE(g.items,0), COALESCE(g.owners,0), COALESCE(g.sales,0),
		(SELECT COUNT(*) FROM nft_sales s WHERE s.collection = c.address AND s.timestamp >= ?),
		COALESCE(g.volume,0),
		(SELECT COALESCE(SUM(s.price_num),0) FROM nft_sales s WHERE s.collection = c.address AND s.currency = ? AND s.timestamp >= ?)
		FROM nft_collections c LEFT JOIN nft_collection_stats g ON g.collection = c.address`
	full := append([]any{day, zeroAddr, day}, args...)
	if where != "" {
		q += " WHERE " + where
	}
	rows, err := s.db.Query(q, full...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*collectionRow
	byAddress := map[string]*collectionRow{}
	for rows.Next() {
		c := new(collectionRow)
		var meta string
		if err := rows.Scan(&c.Address, &c.Name, &c.Symbol, &c.Creator, &c.Factory, &c.ContractURI, &meta, &c.FirstBlock,
			&c.Items, &c.Owners, &c.Sales, &c.Sales24h, &c.Volume, &c.Volume24h); err != nil {
			return nil, err
		}
		c.Meta = rawJSON(meta)
		out = append(out, c)
		byAddress[c.Address] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Floors: the cheapest listing anyone could actually fill right now.
	floors, err := s.db.Query(`SELECT o.collection, MIN(o.price_num), COUNT(*) FROM nft_orders o
		WHERE o.payment_token = ? AND `+listingValid+` GROUP BY o.collection`, append([]any{zeroAddr}, listingArgs()...)...)
	if err != nil {
		return nil, err
	}
	for floors.Next() {
		var addr string
		var floor float64
		var listed int64
		if err := floors.Scan(&addr, &floor, &listed); err != nil {
			floors.Close()
			return nil, err
		}
		if c, ok := byAddress[addr]; ok {
			c.Floor, c.Listed = floor, listed
		}
	}
	floors.Close()
	// Top bid: the most a collection bid would pay for any one item.
	bids, err := s.db.Query(`SELECT b.collection, MAX(b.price_num) FROM nft_orders b WHERE b.token_id IS NULL AND `+bidValid+
		` GROUP BY b.collection`, listingArgs()...)
	if err != nil {
		return nil, err
	}
	defer bids.Close()
	for bids.Next() {
		var addr string
		var top float64
		if err := bids.Scan(&addr, &top); err != nil {
			return nil, err
		}
		if c, ok := byAddress[addr]; ok {
			c.TopBid = top
		}
	}
	return out, bids.Err()
}

func (s *Market) nftCollections(_ context.Context, r *http.Request) (any, error) {
	rows, err := s.collectionRows("", nil)
	if err != nil {
		return nil, err
	}
	sortCollections(rows, r.URL.Query().Get("sort"))
	limit := nodeapi.IntParam(r, "limit", 50, 200)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func sortCollections(rows []*collectionRow, key string) {
	less := func(i, j int) bool {
		switch key {
		case "recent":
			return rows[i].FirstBlock > rows[j].FirstBlock
		case "items":
			return rows[i].Items > rows[j].Items
		case "floor":
			fi, iok := rows[i].Floor.(float64)
			fj, jok := rows[j].Floor.(float64)
			if iok != jok {
				return iok
			}
			return fi < fj
		default: // volume, then newest — and a collection with nothing in it last
			if (rows[i].Items == 0) != (rows[j].Items == 0) {
				return rows[j].Items == 0
			}
			if rows[i].Volume != rows[j].Volume {
				return rows[i].Volume > rows[j].Volume
			}
			return rows[i].FirstBlock > rows[j].FirstBlock
		}
	}
	sortSlice(rows, less)
}

func (s *Market) nftCollection(_ context.Context, r *http.Request) (any, error) {
	addr, err := nodeapi.ParseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	rows, err := s.collectionRows("c.address = ?", []any{addr.Hex()})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nodeapi.ErrNotFound
	}
	return rows[0], nil
}

// itemSelect is a token and its cheapest fillable listing, if any.
const itemSelect = `SELECT t.token_id, t.owner, COALESCE(t.uri,''), COALESCE(t.name,''), COALESCE(t.image,''),
	COALESCE(t.meta,''), t.minted_block,
	(SELECT o.hash FROM nft_orders o WHERE o.collection = t.collection AND o.token_id = t.token_id AND ` + listingValid + `
		ORDER BY o.price_num ASC LIMIT 1) AS best
	FROM nft_tokens t`

func (s *Market) nftItems(_ context.Context, r *http.Request) (any, error) {
	addr, err := nodeapi.ParseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	order := map[string]string{
		"price_asc":  "p.price_num IS NULL, p.price_num ASC",
		"price_desc": "p.price_num IS NULL, p.price_num DESC",
		"recent":     "i.minted_block DESC",
		"id":         "CAST(i.token_id AS INTEGER)",
	}[r.URL.Query().Get("sort")]
	if order == "" {
		order = "CAST(i.token_id AS INTEGER)"
	}
	filter := ""
	if r.URL.Query().Get("listed") == "1" {
		filter = " WHERE i.best IS NOT NULL"
	}
	limit := nodeapi.IntParam(r, "limit", 48, 200)
	offset := nodeapi.IntParam(r, "offset", 0, 1_000_000)
	args := append(listingArgs(), addr.Hex(), zeroAddr, limit, offset)
	rows, err := s.db.Query(`SELECT i.*, p.price, p.price_num, p.payment_token, p.exchange, p.expiration_time, p.trader, p.kind
		FROM (`+itemSelect+` WHERE t.collection = ? AND t.owner != ?) i
		LEFT JOIN nft_orders p ON p.hash = i.best`+filter+`
		ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanItem(rows *sql.Rows) (map[string]any, error) {
	var tokenID, owner, uri, name, image, meta string
	var mintedBlock int64
	var best, price, currency, exchange, seller sql.NullString
	var priceNum sql.NullFloat64
	var expiry any
	var kind sql.NullInt64
	if err := rows.Scan(&tokenID, &owner, &uri, &name, &image, &meta, &mintedBlock, &best,
		&price, &priceNum, &currency, &exchange, &expiry, &seller, &kind); err != nil {
		return nil, err
	}
	item := map[string]any{
		"tokenId": tokenID, "owner": owner, "uri": uri, "name": name, "image": image,
		"meta": rawJSON(meta), "mintedBlock": mintedBlock,
	}
	if best.Valid {
		exactExpiry, err := orderbook.TimeFromStorage(expiry)
		if err != nil {
			return nil, err
		}
		item["listing"] = map[string]any{
			"hash": best.String, "exchange": exchange.String, "price": price.String, "priceNum": priceNum.Float64,
			"currency": currency.String, "expiry": orderbook.TimeJSON(exactExpiry), "seller": seller.String, "kind": kind.Int64,
		}
	}
	return item, nil
}

func (s *Market) nftItem(_ context.Context, r *http.Request) (any, error) {
	addr, err := nodeapi.ParseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	args := append(listingArgs(), addr.Hex(), id)
	rows, err := s.db.Query(`SELECT i.*, p.price, p.price_num, p.payment_token, p.exchange, p.expiration_time, p.trader, p.kind
		FROM (`+itemSelect+` WHERE t.collection = ? AND t.token_id = ?) i
		LEFT JOIN nft_orders p ON p.hash = i.best`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nodeapi.ErrNotFound
	}
	item, err := scanItem(rows)
	if err != nil {
		return nil, err
	}
	rows.Close()

	item["collection"] = addr.Hex()
	if collections, err := s.collectionRows("c.address = ?", []any{addr.Hex()}); err == nil && len(collections) > 0 {
		item["collectionInfo"] = collections[0]
	}
	// Bids that could take this token: its own, and any collection bid.
	bids, err := s.bidRows(`b.collection = ? AND (b.token_id = ? OR b.token_id IS NULL) AND `+bidValid,
		append([]any{addr.Hex(), id}, listingArgs()...), "b.price_num DESC", 50)
	if err != nil {
		return nil, err
	}
	item["offers"] = bids
	if item["listings"], err = s.orderRows(`o.collection = ? AND o.token_id = ? AND `+listingValid,
		append([]any{addr.Hex(), id}, listingArgs()...), "o.price_num ASC", 20); err != nil {
		return nil, err
	}
	sales, err := s.saleRows("s.collection = ? AND s.token_id = ?", []any{addr.Hex(), id}, 50)
	if err != nil {
		return nil, err
	}
	item["sales"] = sales
	transfers, err := nodeapi.QueryList(s.db, `SELECT t.idx, t.token, t.from_addr, t.to_addr, t.value, t.token_id,
		COALESCE(k.symbol,''), COALESCE(k.decimals,0), COALESCE(k.kind,'')
		FROM token_transfers t LEFT JOIN tokens k ON k.address = t.token
		WHERE t.token = ? AND t.token_id = ? ORDER BY t.block DESC, t.idx DESC LIMIT 50`,
		[]any{addr.Hex(), id}, nodeapi.ScanTransferRow)
	if err != nil {
		return nil, err
	}
	item["transfers"] = transfers
	return item, nil
}

// --- orders ---------------------------------------------------------------

// bidRows is bids in the shape the pages have always read offers in.
func (s *Market) bidRows(where string, args []any, order string, limit int) ([]any, error) {
	q := `SELECT b.hash, b.exchange, b.collection, b.token_id, b.trader, b.payment_token, b.price, b.price_num,
		b.amount, b.filled, b.expiration_time, b.status, b.block, b.timestamp FROM nft_orders b
		WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?`
	return nodeapi.QueryList(s.db, q, append(args, limit), func(rows *sql.Rows) (any, error) {
		var hash, exchange, collection, bidder, payment, price, status string
		var tokenID sql.NullString
		var quantity, filled, block, ts int64
		var expiry orderbook.StoredTime
		var priceNum float64
		if err := rows.Scan(&hash, &exchange, &collection, &tokenID, &bidder, &payment, &price, &priceNum, &quantity, &filled,
			&expiry, &status, &block, &ts); err != nil {
			return nil, err
		}
		return map[string]any{
			"hash": hash, "exchange": exchange, "collection": collection, "tokenId": nodeapi.NullStr(tokenID), "bidder": bidder,
			"paymentToken": payment, "price": price, "priceNum": priceNum, "quantity": quantity, "filled": filled,
			"expiry": orderbook.TimeJSON(uint64(expiry)), "status": status, "block": block, "timestamp": ts, "isCollection": !tokenID.Valid,
		}, nil
	})
}

func (s *Market) listingRows(where string, args []any, order string, limit int) ([]any, error) {
	return s.orderRows(where, args, order, limit)
}

func (s *Market) saleRows(where string, args []any, limit int) ([]any, error) {
	q := `SELECT s.tx, s.idx, s.block, s.timestamp, s.collection, s.token_id, s.seller, s.buyer, s.currency,
		s.price, s.price_num, s.royalty, s.fee, s.order_hash, s.side, s.paid_in, COALESCE(t.name,''), COALESCE(t.image,''),
		COALESCE(c.name,'')
		FROM nft_sales s
		LEFT JOIN nft_tokens t ON t.collection = s.collection AND t.token_id = s.token_id
		LEFT JOIN nft_collections c ON c.address = s.collection
		WHERE ` + where + ` ORDER BY s.block DESC, s.idx DESC LIMIT ?`
	return nodeapi.QueryList(s.db, q, append(args, limit), func(rows *sql.Rows) (any, error) {
		var tx, collection, tokenID, seller, buyer, currency, price, royalty, fee, orderHash, paidIn, name, image, collectionName string
		var idx, block, ts, side int64
		var priceNum float64
		if err := rows.Scan(&tx, &idx, &block, &ts, &collection, &tokenID, &seller, &buyer, &currency, &price,
			&priceNum, &royalty, &fee, &orderHash, &side, &paidIn, &name, &image, &collectionName); err != nil {
			return nil, err
		}
		return map[string]any{
			"tx": tx, "index": idx, "block": block, "timestamp": ts, "collection": collection,
			"collectionName": collectionName, "tokenId": tokenID, "seller": seller, "buyer": buyer,
			"currency": currency, "paidIn": paidIn, "price": price, "priceNum": priceNum, "royalty": royalty, "fee": fee,
			"name": name, "image": image, "order": orderHash, "acceptedBid": side == 1,
		}, nil
	})
}

func (s *Market) nftSales(_ context.Context, r *http.Request) (any, error) {
	if c := r.URL.Query().Get("collection"); c != "" {
		addr, err := nodeapi.ParseAddress(c)
		if err != nil {
			return nil, err
		}
		return s.saleRows("s.collection = ?", []any{addr.Hex()}, nodeapi.IntParam(r, "limit", 25, 200))
	}
	return s.saleRows("1=1", nil, nodeapi.IntParam(r, "limit", 25, 200))
}

func (s *Market) nftListings(_ context.Context, r *http.Request) (any, error) {
	where, args := listingValid, listingArgs()
	if c := r.URL.Query().Get("collection"); c != "" {
		addr, err := nodeapi.ParseAddress(c)
		if err != nil {
			return nil, err
		}
		where += " AND o.collection = ?"
		args = append(args, addr.Hex())
	}
	order := "o.block DESC, o.hash"
	if r.URL.Query().Get("sort") == "price" {
		order = "o.price_num ASC"
	}
	return s.listingRows(where, args, order, nodeapi.IntParam(r, "limit", 25, 200))
}

func (s *Market) nftOffers(_ context.Context, r *http.Request) (any, error) {
	where, args := bidValid, listingArgs()
	if c := r.URL.Query().Get("collection"); c != "" {
		addr, err := nodeapi.ParseAddress(c)
		if err != nil {
			return nil, err
		}
		where += " AND b.collection = ?"
		args = append(args, addr.Hex())
	}
	if r.URL.Query().Get("collectionOnly") == "1" {
		where += " AND b.token_id IS NULL"
	}
	return s.bidRows(where, args, "b.price_num DESC", nodeapi.IntParam(r, "limit", 25, 200))
}

// --- activity -------------------------------------------------------------

// activity mixes sales and transfers from the index with the book's orders,
// which have no transaction — a listing is a signature — so their "tx" is the
// order's hash and their block the one the node received them at.
func (s *Market) activity(where map[string]string, args []any, limit int) ([]any, error) {
	q := `SELECT kind, block, timestamp, tx, collection, token_id, a, b, price, currency FROM (
		SELECT CASE WHEN side = 1 THEN 'bid-accepted' ELSE 'sale' END AS kind, block, timestamp, tx, collection, token_id,
			seller AS a, buyer AS b, price, currency FROM nft_sales WHERE ` + where["sale"] + `
		UNION ALL
		SELECT CASE WHEN side = 0 THEN 'list' ELSE 'offer' END, block, timestamp, hash, collection, COALESCE(token_id,''),
			trader, '', price, payment_token FROM nft_orders WHERE ` + where["order"] + `
		UNION ALL
		SELECT 'transfer', block, timestamp, tx, token, COALESCE(token_id,''), from_addr, to_addr, '', '' FROM token_transfers WHERE ` + where["transfer"] + `
	) ORDER BY block DESC, timestamp DESC LIMIT ?`
	return nodeapi.QueryList(s.db, q, append(args, limit), func(rows *sql.Rows) (any, error) {
		var kind, tx, collection, tokenID, a, b, price, currency string
		var block, ts int64
		if err := rows.Scan(&kind, &block, &ts, &tx, &collection, &tokenID, &a, &b, &price, &currency); err != nil {
			return nil, err
		}
		out := map[string]any{
			"kind": kind, "block": block, "timestamp": ts, "collection": collection, "tokenId": tokenID,
			"from": a, "to": b, "price": price, "currency": currency,
		}
		if kind == "list" || kind == "offer" {
			out["order"] = tx
		} else {
			out["tx"] = tx
		}
		return out, nil
	})
}

func (s *Market) nftActivity(_ context.Context, r *http.Request) (any, error) {
	addr, err := nodeapi.ParseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	a := addr.Hex()
	return s.activity(map[string]string{
		"sale": "collection = ?", "order": "collection = ? AND status != 'invalid'",
		"transfer": "token = ? AND token_id IS NOT NULL",
	}, []any{a, a, a}, nodeapi.IntParam(r, "limit", 50, 200))
}

// --- accounts -------------------------------------------------------------

func (s *Market) nftAccount(_ context.Context, r *http.Request) (any, error) {
	addr, err := nodeapi.ParseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	a := addr.Hex()
	out := map[string]any{"address": a}

	owned, err := nodeapi.QueryList(s.db, `SELECT t.collection, t.token_id, COALESCE(t.name,''), COALESCE(t.image,''), COALESCE(c.name,''),
		p.hash, p.price, p.price_num, p.payment_token, p.exchange
		FROM nft_tokens t
		LEFT JOIN nft_collections c ON c.address = t.collection
		LEFT JOIN nft_orders p ON p.hash = (SELECT o.hash FROM nft_orders o WHERE o.collection = t.collection
			AND o.token_id = t.token_id AND `+listingValid+` ORDER BY o.price_num LIMIT 1)
		WHERE t.owner = ? ORDER BY t.updated_block DESC LIMIT 200`,
		append(listingArgs(), a), func(rows *sql.Rows) (any, error) {
			var collection, tokenID, name, image, collectionName string
			var hash, price, currency, exchange sql.NullString
			var priceNum sql.NullFloat64
			if err := rows.Scan(&collection, &tokenID, &name, &image, &collectionName, &hash, &price, &priceNum, &currency, &exchange); err != nil {
				return nil, err
			}
			item := map[string]any{"collection": collection, "collectionName": collectionName, "tokenId": tokenID, "name": name, "image": image}
			if hash.Valid {
				item["listing"] = map[string]any{"hash": hash.String, "exchange": exchange.String, "price": price.String,
					"priceNum": priceNum.Float64, "currency": currency.String}
			}
			return item, nil
		})
	if err != nil {
		return nil, err
	}
	out["owned"] = owned

	created, err := s.collectionRows("c.creator = ?", []any{a})
	if err != nil {
		return nil, err
	}
	sortCollections(created, "recent")
	out["created"] = created

	if out["listings"], err = s.listingRows(listingValid+" AND o.trader = ?", append(listingArgs(), a), "o.block DESC", 100); err != nil {
		return nil, err
	}
	if out["offersMade"], err = s.bidRows(bidValid+" AND b.trader = ?", append(listingArgs(), a), "b.block DESC", 100); err != nil {
		return nil, err
	}
	// Bids this account could accept: on a token it holds, or on a collection
	// it holds something in.
	received, err := s.bidRows(bidValid+` AND b.trader != ? AND ((b.token_id IS NOT NULL AND EXISTS (
			SELECT 1 FROM nft_tokens t WHERE t.collection = b.collection AND t.token_id = b.token_id AND t.owner = ?))
		OR (b.token_id IS NULL AND EXISTS (
			SELECT 1 FROM nft_tokens t WHERE t.collection = b.collection AND t.owner = ?)))`,
		append(listingArgs(), a, a, a), "b.price_num DESC", 100)
	if err != nil {
		return nil, err
	}
	out["offersReceived"] = received
	// Everything this account has signed that is still open, fillable or not —
	// what a "cancel all" would cancel.
	if out["openOrders"], err = s.orderRows("o.trader = ? AND o.status = 'open'", []any{a}, "o.block DESC", 200); err != nil {
		return nil, err
	}

	if out["activity"], err = s.activity(map[string]string{
		"sale": "(seller = ? OR buyer = ?)", "order": "trader = ? AND status != 'invalid'",
		"transfer": "(from_addr = ? OR to_addr = ?) AND token_id IS NOT NULL",
	}, []any{a, a, a, a, a}, 100); err != nil {
		return nil, err
	}
	return out, nil
}

// --- overview -------------------------------------------------------------

func (s *Market) nftOverview(_ context.Context, r *http.Request) (any, error) {
	now := time.Now().Unix()
	totals := map[string]any{}
	var collections, tokens, sales int64
	var volume, volume24h sql.NullFloat64
	s.db.QueryRow("SELECT COUNT(*) FROM nft_collections").Scan(&collections)
	s.db.QueryRow("SELECT COUNT(*) FROM nft_tokens WHERE owner != ?", zeroAddr).Scan(&tokens)
	s.db.QueryRow("SELECT COUNT(*), SUM(price_num) FROM nft_sales WHERE currency = ?", zeroAddr).Scan(&sales, &volume)
	s.db.QueryRow("SELECT SUM(price_num) FROM nft_sales WHERE currency = ? AND timestamp >= ?", zeroAddr, now-86400).Scan(&volume24h)
	totals["collections"], totals["items"], totals["sales"] = collections, tokens, sales
	totals["volume"], totals["volume24h"] = nullFloat(volume), nullFloat(volume24h)
	var listed int64
	s.db.QueryRow(`SELECT COUNT(*) FROM nft_orders o WHERE `+listingValid, listingArgs()...).Scan(&listed)
	totals["listed"] = listed

	top, err := s.collectionRows("", nil)
	if err != nil {
		return nil, err
	}
	sortCollections(top, "")
	if len(top) > 12 {
		top = top[:12]
	}
	recentSales, err := s.saleRows("1=1", nil, 12)
	if err != nil {
		return nil, err
	}
	recentListings, err := s.listingRows(listingValid, listingArgs(), "o.block DESC, o.hash", 12)
	if err != nil {
		return nil, err
	}
	return map[string]any{"totals": totals, "collections": top, "sales": recentSales, "listings": recentListings}, nil
}

// sortSlice is an insertion sort: stable, and the lists it orders are the tens
// of collections a chain has, not thousands.
func sortSlice[T any](rows []T, less func(i, j int) bool) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
