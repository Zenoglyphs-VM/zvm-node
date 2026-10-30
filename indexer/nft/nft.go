package nft

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
)

// ERC-721 collections, their tokens, and the sales the exchange settled.
// Ownership comes from Transfer events, so every ERC-721 on the chain is here,
// not only the factory's. Exchange and factory events are trusted only from the
// addresses the deployment file names — anyone can emit an OrderFilled event.
//
// Unfilled orders are not here. They are signatures, not chain state, so they
// live in the order book (internal/orderbook), which this index may be rebuilt
// underneath without losing (D28).

var (
	// topicTransfer is ERC-721's, which is ERC-20's with a fourth topic. The
	// chain's own index has the same constant; duplicating one keccak here
	// keeps this plugin from needing anything unexported from it.
	topicTransfer            = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	topicApproval            = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
	topicApprovalForAll      = crypto.Keccak256Hash([]byte("ApprovalForAll(address,address,bool)"))
	topicMetadataUpdate      = crypto.Keccak256Hash([]byte("MetadataUpdate(uint256)"))
	topicBatchMetadataUpdate = crypto.Keccak256Hash([]byte("BatchMetadataUpdate(uint256,uint256)"))
	topicContractURIUpdated  = crypto.Keccak256Hash([]byte("ContractURIUpdated()"))
	topicCollectionCreated   = crypto.Keccak256Hash([]byte("CollectionCreated(address,address,string,string)"))
	topicOrderFilled         = crypto.Keccak256Hash([]byte("OrderFilled(bytes32,address,address,uint8,address,uint256,address,uint256,uint256,uint256)"))

	selTokenURI    = crypto.Keccak256([]byte("tokenURI(uint256)"))[:4]
	selContractURI = crypto.Keccak256([]byte("contractURI()"))[:4]
)

const nftSchema = `
CREATE TABLE IF NOT EXISTS nft_collections (
	address      TEXT PRIMARY KEY,
	name         TEXT,
	symbol       TEXT,
	creator      TEXT,
	factory      TEXT,
	contract_uri TEXT,
	meta         TEXT,
	first_block  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS nft_tokens (
	collection    TEXT    NOT NULL,
	token_id      TEXT    NOT NULL,
	owner         TEXT    NOT NULL,
	approved      TEXT,
	uri           TEXT,
	meta          TEXT,
	name          TEXT,
	image         TEXT,
	minted_block  INTEGER NOT NULL,
	updated_block INTEGER NOT NULL,
	PRIMARY KEY (collection, token_id)
);
CREATE INDEX IF NOT EXISTS nft_tokens_owner ON nft_tokens(owner);
-- The two NFT endpoints scanned every token on the chain to find one
-- collection's, and every sale to find one collection's last day. These were
-- in the index's own schema, where they built an index on a table this plugin
-- owns — so the index could not be created without the plugin installed.
CREATE INDEX IF NOT EXISTS nft_tokens_collection ON nft_tokens(collection, token_id);
CREATE TABLE IF NOT EXISTS nft_approvals (
	owner      TEXT    NOT NULL,
	collection TEXT    NOT NULL,
	operator   TEXT    NOT NULL,
	approved   INTEGER NOT NULL,
	block      INTEGER NOT NULL,
	PRIMARY KEY (owner, collection, operator)
);
CREATE TABLE IF NOT EXISTS nft_sales (
	tx         TEXT    NOT NULL,
	idx        INTEGER NOT NULL,
	market     TEXT    NOT NULL,
	block      INTEGER NOT NULL,
	timestamp  INTEGER NOT NULL,
	collection TEXT    NOT NULL,
	token_id   TEXT    NOT NULL,
	seller     TEXT    NOT NULL,
	buyer      TEXT    NOT NULL,
	currency   TEXT    NOT NULL,
	price      TEXT    NOT NULL,
	price_num  REAL    NOT NULL,
	royalty    TEXT    NOT NULL,
	fee        TEXT    NOT NULL,
	order_hash TEXT    NOT NULL,
	side       INTEGER NOT NULL,
	paid_in    TEXT    NOT NULL,
	PRIMARY KEY (tx, idx)
);
CREATE INDEX IF NOT EXISTS nft_sales_collection ON nft_sales(collection, block);
CREATE INDEX IF NOT EXISTS nft_sales_block ON nft_sales(block);
CREATE INDEX IF NOT EXISTS nft_sales_recent ON nft_sales(collection, timestamp DESC);
`

// Apps names the application contracts whose events the indexer trusts.
type Apps struct {
	Exchange          common.Address
	CollectionFactory common.Address
	// WrappedZNN is what bids are paid in; its sales count as ZNN.
	WrappedZNN common.Address
}

type AppsFunc func() Apps

// MetadataResolver reads a URI the node can answer without fetching anything:
// an object in its own media store.
type MetadataResolver interface {
	Resolve(uri string) ([]byte, bool)
}

// nftWhere selects the logs the NFT tables are built from.
func nftWhere() (string, []any) {
	always, a := indexer.TopicList([]common.Hash{topicApprovalForAll, topicMetadataUpdate, topicBatchMetadataUpdate, topicContractURIUpdated})
	fourTopics, b := indexer.TopicList([]common.Hash{topicTransfer, topicApproval})
	return "(l.topic0 IN (" + always + ") OR (l.topic0 IN (" + fourTopics + ") AND l.topic3 IS NOT NULL))", append(a, b...)
}

func (x *Index) indexNFTLog(ctx context.Context, dbtx *sql.Tx, r indexer.LogRow) error {
	if len(r.Topics) == 0 {
		return nil
	}
	switch r.Topics[0] {
	case topicTransfer:
		if len(r.Topics) != 4 {
			return nil
		}
		return x.nftTransfer(ctx, dbtx, r, r.Addr(1), r.Addr(2), r.Num(3))
	case topicApproval:
		if len(r.Topics) != 4 {
			return nil
		}
		_, err := dbtx.Exec(`UPDATE nft_tokens SET approved = ? WHERE collection = ? AND token_id = ?`,
			r.Addr(2).Hex(), r.Address.Hex(), r.Num(3).String())
		return err
	case topicApprovalForAll:
		if len(r.Topics) != 3 || len(r.Data) != 32 {
			return nil
		}
		approved := 0
		if r.Word(0).Sign() != 0 {
			approved = 1
		}
		_, err := dbtx.Exec(`INSERT INTO nft_approvals (owner, collection, operator, approved, block) VALUES (?,?,?,?,?)
			ON CONFLICT(owner, collection, operator) DO UPDATE SET approved = excluded.approved, block = excluded.block`,
			r.Addr(1).Hex(), r.Address.Hex(), r.Addr(2).Hex(), approved, r.Block)
		return err
	case topicMetadataUpdate:
		if len(r.Data) != 32 {
			return nil
		}
		return x.refreshToken(ctx, dbtx, r.Address, r.Word(0), r.Block)
	case topicBatchMetadataUpdate:
		if len(r.Data) != 64 {
			return nil
		}
		return x.refreshRange(ctx, dbtx, r.Address, r.Word(0), r.Word(1), r.Block)
	case topicContractURIUpdated:
		return x.refreshCollection(ctx, dbtx, r.Address, r.Block)
	}

	apps := x.apps()
	if r.Topics[0] == topicCollectionCreated {
		if apps.CollectionFactory == (common.Address{}) || r.Address != apps.CollectionFactory || len(r.Topics) != 3 {
			return nil
		}
		collection := r.Addr(1)
		if err := x.ensureCollection(ctx, dbtx, collection, r.Block); err != nil {
			return err
		}
		_, err := dbtx.Exec(`UPDATE nft_collections SET creator = ?, factory = ? WHERE address = ?`,
			r.Addr(2).Hex(), r.Address.Hex(), collection.Hex())
		return err
	}
	if apps.Exchange == (common.Address{}) || r.Address != apps.Exchange {
		return nil
	}
	return x.indexMarketLog(ctx, dbtx, r, apps)
}

func (x *Index) indexMarketLog(ctx context.Context, dbtx *sql.Tx, r indexer.LogRow, apps Apps) error {
	if r.Topics[0] != topicOrderFilled || len(r.Topics) != 4 || len(r.Data) != 7*32 {
		return nil
	}
	maker, taker := r.Addr(2), r.Addr(3)
	side := r.Word(0).Uint64()
	collection, tokenID, paidIn, price := r.WordAddr(1), r.Word(2), r.WordAddr(3), r.Word(4)
	seller, buyer := maker, taker
	if side == 1 {
		seller, buyer = taker, maker
	}
	// A bid is paid in wrapped ZNN because nothing is escrowed; its value is
	// ZNN's all the same, so it counts as ZNN wherever volume and floors are
	// summed. The token actually moved is kept beside it.
	currency := paidIn
	if apps.WrappedZNN != (common.Address{}) && paidIn == apps.WrappedZNN {
		currency = common.Address{}
	}
	if err := x.ensureCollection(ctx, dbtx, collection, r.Block); err != nil {
		return err
	}
	x.markCollection(collection.Hex())
	_, err := dbtx.Exec(`INSERT OR REPLACE INTO nft_sales
		(tx, idx, market, block, timestamp, collection, token_id, seller, buyer, currency, price, price_num, royalty, fee,
		 order_hash, side, paid_in)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Tx, r.Index, r.Address.Hex(), r.Block, r.Time, collection.Hex(), tokenID.String(), seller.Hex(), buyer.Hex(),
		currency.Hex(), price.String(), x.amountNum(dbtx, currency, price), r.Word(5).String(), r.Word(6).String(),
		r.Topics[1].Hex(), side, paidIn.Hex())
	return err
}

// amountNum is an order's price in whole units of its currency, for sorting
// and floor prices. The zero address is ZNN.
func (x *Index) amountNum(dbtx *sql.Tx, currency common.Address, amount *big.Int) float64 {
	decimals := 18
	if currency != (common.Address{}) {
		var d sql.NullInt64
		if dbtx.QueryRow("SELECT decimals FROM tokens WHERE address = ?", currency.Hex()).Scan(&d) == nil && d.Valid {
			decimals = int(d.Int64)
		}
	}
	return indexer.FloatUnits(amount, decimals)
}

func (x *Index) nftTransfer(ctx context.Context, dbtx *sql.Tx, r indexer.LogRow, from, to common.Address, id *big.Int) error {
	if err := x.ensureCollection(ctx, dbtx, r.Address, r.Block); err != nil {
		return err
	}
	x.markCollection(r.Address.Hex())
	if from != (common.Address{}) {
		res, err := dbtx.Exec(`UPDATE nft_tokens SET owner = ?, approved = NULL, updated_block = ? WHERE collection = ? AND token_id = ?`,
			to.Hex(), r.Block, r.Address.Hex(), id.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	// A mint, or a token first seen mid-life: read its metadata.
	value := x.tokenObservation(ctx, r.Block, r.Address, id)
	value, err := x.admitMetadata(ctx, dbtx, r.Block, value)
	if err != nil {
		return err
	}
	uri, m := value.uri, value.meta
	_, err = dbtx.Exec(`INSERT INTO nft_tokens (collection, token_id, owner, uri, meta, name, image, minted_block, updated_block)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(collection, token_id) DO UPDATE SET owner = excluded.owner, approved = NULL, uri = excluded.uri,
			meta = excluded.meta, name = excluded.name, image = excluded.image, updated_block = excluded.updated_block`,
		r.Address.Hex(), id.String(), to.Hex(), nullString(uri), m.raw, m.name, m.image, r.Block, r.Block)
	if err != nil {
		return err
	}
	return recordMetadata(ctx, dbtx, "token", r.Address.Hex(), id.String(), r.Block, value)
}

func (x *Index) refreshToken(ctx context.Context, dbtx *sql.Tx, collection common.Address, id *big.Int, block uint64) error {
	value := x.tokenObservation(ctx, block, collection, id)
	value, err := x.admitMetadata(ctx, dbtx, block, value)
	if err != nil {
		return err
	}
	uri, m := value.uri, value.meta
	result, err := dbtx.Exec(`UPDATE nft_tokens SET uri = ?, meta = ?, name = ?, image = ?, updated_block = ? WHERE collection = ? AND token_id = ?`,
		nullString(uri), m.raw, m.name, m.image, block, collection.Hex(), id.String())
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	return recordMetadata(ctx, dbtx, "token", collection.Hex(), id.String(), block, value)
}

// refreshRange re-reads the metadata of the tokens a BatchMetadataUpdate names
// that the index holds, at most a few thousand of them.
func (x *Index) refreshRange(ctx context.Context, dbtx *sql.Tx, collection common.Address, from, to *big.Int, block uint64) error {
	rows, err := dbtx.QueryContext(ctx, `SELECT token_id FROM nft_tokens`+metadataRangeWhere, metadataRangeArgs(collection, from, to)...)
	if err != nil {
		return err
	}
	var ids []*big.Int
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		if id, ok := new(big.Int).SetString(s, 10); ok && id.Cmp(from) >= 0 && id.Cmp(to) <= 0 {
			ids = append(ids, id)
			if len(ids) == 5_001 {
				break
			}
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for i, id := range ids {
		if i == 5_000 {
			x.log().Warn("metadata batch update truncated", "collection", collection, "atLeastTokens", len(ids))
			break
		}
		if err := x.refreshToken(ctx, dbtx, collection, id, block); err != nil {
			return err
		}
	}
	return nil
}

func (x *Index) refreshCollection(ctx context.Context, dbtx *sql.Tx, collection common.Address, block uint64) error {
	if err := x.ensureCollection(ctx, dbtx, collection, block); err != nil {
		return err
	}
	uri, proven := x.readObservedURI(ctx, block, collection, selContractURI)
	value, err := x.admitMetadata(ctx, dbtx, block, x.observedMetadata(uri, proven))
	if err != nil {
		return err
	}
	uri, m := value.uri, value.meta
	_, err = dbtx.Exec(`UPDATE nft_collections SET contract_uri = ?, meta = ? WHERE address = ?`, nullString(uri), m.raw, collection.Hex())
	if err != nil {
		return err
	}
	return recordMetadata(ctx, dbtx, "collection", collection.Hex(), "", block, value)
}

func (x *Index) ensureCollection(ctx context.Context, dbtx *sql.Tx, addr common.Address, block uint64) error {
	var known string
	err := dbtx.QueryRow("SELECT address FROM nft_collections WHERE address = ?", addr.Hex()).Scan(&known)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	uri, proven := x.readObservedURI(ctx, block, addr, selContractURI)
	value, err := x.admitMetadata(ctx, dbtx, block, x.observedMetadata(uri, proven))
	if err != nil {
		return err
	}
	uri, m := value.uri, value.meta
	_, err = dbtx.Exec(`INSERT OR IGNORE INTO nft_collections (address, name, symbol, contract_uri, meta, first_block) VALUES (?,?,?,?,?,?)`,
		addr.Hex(), nullString(x.callString(ctx, addr, "name()")), nullString(x.callString(ctx, addr, "symbol()")), nullString(uri), m.raw, block)
	x.markCollection(addr.Hex())
	if err != nil {
		return err
	}
	return recordMetadata(ctx, dbtx, "collection", addr.Hex(), "", block, value)
}

func (x *Index) tokenObservation(ctx context.Context, block uint64, collection common.Address, id *big.Int) metadataValue {
	data := append(append([]byte{}, selTokenURI...), common.LeftPadBytes(id.Bytes(), 32)...)
	uri, proven := x.readObservedURI(ctx, block, collection, data)
	return x.observedMetadata(uri, proven)
}

func (x *Index) tokenURI(ctx context.Context, block uint64, collection common.Address, id *big.Int) string {
	data := append(append([]byte{}, selTokenURI...), common.LeftPadBytes(id.Bytes(), 32)...)
	return x.readURI(ctx, block, collection, data)
}

// contractURI is the collection-level metadata URI, read like a token's.
func (x *Index) contractURI(ctx context.Context, block uint64, collection common.Address) string {
	return x.readURI(ctx, block, collection, selContractURI)
}

type tokenMeta struct {
	raw, name, image sql.NullString
}

// resolveMeta reads metadata the node holds itself — a data: URI, or an object
// in its media store — and nothing it would have to fetch.
func (x *Index) resolveMeta(uri string) tokenMeta {
	var m tokenMeta
	if uri == "" {
		return m
	}
	raw, ok := decodeDataURI(uri)
	if !ok && x.media != nil {
		raw, ok = x.media.Resolve(uri)
	}
	if !ok || len(raw) > 64<<10 {
		return m
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return m
	}
	m.raw = sql.NullString{String: string(raw), Valid: true}
	if s, ok := doc["name"].(string); ok && s != "" {
		m.name = sql.NullString{String: s, Valid: true}
	}
	for _, k := range []string{"image", "image_url"} {
		if s, ok := doc[k].(string); ok && s != "" {
			m.image = sql.NullString{String: s, Valid: true}
			break
		}
	}
	return m
}

func decodeDataURI(uri string) ([]byte, bool) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, false
	}
	header, payload, ok := strings.Cut(uri[len("data:"):], ",")
	if !ok {
		return nil, false
	}
	if strings.HasSuffix(header, ";base64") {
		b, err := base64.StdEncoding.DecodeString(payload)
		return b, err == nil
	}
	s, err := url.PathUnescape(payload)
	return []byte(s), err == nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
