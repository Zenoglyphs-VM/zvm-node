package nft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
)

// This is a logical SQLite page bound, not a hard process-RSS guarantee. The
// entire preparation must fit; exhaustion leaves the canonical job incomplete.
// A private in-memory DB avoids files, shared temp-directory policy and an
// unbounded Go response map. No canonical table is added by preparation.
const preparedURIBytes = 64 << 20
const preparedRequestPage = 128
const uriRequest = 1
const stringRequest = 2

type preparedURIKey struct{}

type uriRequestRow struct {
	seq     int64
	block   uint64
	hash    common.Hash
	address common.Address
	kind    int
	data    []byte
}

type preparedURI struct {
	db              *sql.DB
	collect         *sql.Tx
	index           *Index
	count, consumed int64
	ready           bool
	closed          bool
	err             error
}

func uriPreparation(ctx context.Context) *preparedURI {
	value, _ := ctx.Value(preparedURIKey{}).(*preparedURI)
	return value
}

func newPreparedURI(ctx context.Context, x *Index, capacity int64) (*preparedURI, error) {
	if capacity < 16*4096 || capacity%4096 != 0 {
		return nil, errors.New("invalid URI preparation page capacity")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	p := &preparedURI{db: db, index: x}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	for _, statement := range []string{
		"PRAGMA page_size=4096", "PRAGMA temp_store=MEMORY", "PRAGMA journal_mode=MEMORY", "PRAGMA cache_size=-1024",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	var pageSize, pages int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil || pageSize != 4096 {
		return nil, fmt.Errorf("URI preparation page size was not applied: %d: %v", pageSize, err)
	}
	if err := db.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", capacity/pageSize)).Scan(&pages); err != nil || pages != capacity/pageSize {
		return nil, fmt.Errorf("URI preparation page limit was not applied: %d: %v", pages, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE uri_requests (
	 seq INTEGER PRIMARY KEY, block INTEGER NOT NULL, hash BLOB NOT NULL, address BLOB NOT NULL,
	 kind INTEGER NOT NULL CHECK(kind IN (1,2)), data BLOB NOT NULL);
	 CREATE TABLE uri_values (hash TEXT PRIMARY KEY, value BLOB NOT NULL);
	 CREATE TABLE uri_results (seq INTEGER PRIMARY KEY, known INTEGER NOT NULL CHECK(known IN (0,1)), value_hash TEXT,
	 CHECK((known=0 AND value_hash IS NULL) OR (known=1 AND value_hash IS NOT NULL)));`+nftSchema); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}

// CollectLogs runs the actual NFT handler against disposable tables. Only the
// source keys a log needs are read from the canonical snapshot; prior mints in
// the same input remain in the shadow, including for a later batch refresh.
func (x *Index) CollectLogs(ctx context.Context, source indexer.PreparationSource, replay indexer.LogReplay) (indexer.PreparedLogs, error) {
	p, err := newPreparedURI(ctx, x, preparedURIBytes)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			p.Close()
		}
	}()
	p.collect, err = p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	shadow := New(Config{Apps: x.apps})
	shadow.host = x.host
	collecting := context.WithValue(ctx, preparedURIKey{}, p)
	err = replay(func(row indexer.LogRow) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.loadSourceKeys(ctx, source, row); err != nil {
			return err
		}
		return shadow.Handle(collecting, p.collect, row)
	})
	if err != nil {
		return nil, err
	}
	if p.err != nil {
		return nil, p.err
	}
	if err := p.collect.Commit(); err != nil {
		return nil, err
	}
	p.collect = nil
	// Responses reuse space from discovery. There are no source or handler
	// cursors left open when Resolve starts issuing external calls.
	for _, name := range []string{"nft_collections", "nft_tokens", "nft_approvals", "nft_sales"} {
		if _, err := p.db.ExecContext(ctx, "DROP TABLE "+name); err != nil {
			return nil, err
		}
	}
	ok = true
	return p, nil
}

func (p *preparedURI) loadCollection(ctx context.Context, source indexer.PreparationSource, address common.Address) error {
	var known int
	if err := p.collect.QueryRowContext(ctx, "SELECT COUNT(*) FROM nft_collections WHERE address=?", address.Hex()).Scan(&known); err != nil {
		return err
	}
	if known != 0 {
		return nil
	}
	var first uint64
	err := source.QueryRowContext(ctx, "SELECT first_block FROM nft_collections WHERE address=?", address.Hex()).Scan(&first)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = p.collect.ExecContext(ctx, "INSERT OR IGNORE INTO nft_collections(address,first_block) VALUES(?,?)", address.Hex(), first)
	return err
}

func (p *preparedURI) loadToken(ctx context.Context, source indexer.PreparationSource, address common.Address, id string) error {
	var known int
	if err := p.collect.QueryRowContext(ctx, "SELECT COUNT(*) FROM nft_tokens WHERE collection=? AND token_id=?", address.Hex(), id).Scan(&known); err != nil {
		return err
	}
	if known != 0 {
		return nil
	}
	var minted, updated uint64
	err := source.QueryRowContext(ctx, "SELECT minted_block,updated_block FROM nft_tokens WHERE collection=? AND token_id=?", address.Hex(), id).Scan(&minted, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = p.collect.ExecContext(ctx, "INSERT OR IGNORE INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'',?,?)", address.Hex(), id, minted, updated)
	return err
}

func (p *preparedURI) loadRange(ctx context.Context, source indexer.PreparationSource, address common.Address, from, to *big.Int) error {
	rows, err := source.QueryContext(ctx, "SELECT token_id,minted_block,updated_block FROM nft_tokens"+metadataRangeWhere, metadataRangeArgs(address, from, to)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct {
		id              string
		minted, updated uint64
	}
	var keys []key
	for rows.Next() {
		var item key
		if err := rows.Scan(&item.id, &item.minted, &item.updated); err != nil {
			return err
		}
		id, ok := new(big.Int).SetString(item.id, 10)
		if ok && id.Cmp(from) >= 0 && id.Cmp(to) <= 0 {
			keys = append(keys, item)
			if len(keys) == 5001 {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Earlier shadow keys can only displace these original first 5001 keys.
	// They cannot make a later main key enter the handler's first 5000.
	for _, item := range keys {
		if _, err := p.collect.ExecContext(ctx, "INSERT OR IGNORE INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'',?,?)", address.Hex(), item.id, item.minted, item.updated); err != nil {
			return err
		}
	}
	return nil
}

func (p *preparedURI) loadSourceKeys(ctx context.Context, source indexer.PreparationSource, row indexer.LogRow) error {
	if len(row.Topics) == 0 {
		return nil
	}
	switch row.Topics[0] {
	case topicTransfer:
		if len(row.Topics) == 4 {
			if err := p.loadCollection(ctx, source, row.Address); err != nil {
				return err
			}
			return p.loadToken(ctx, source, row.Address, row.Num(3).String())
		}
	case topicMetadataUpdate:
		if len(row.Data) == 32 {
			return p.loadToken(ctx, source, row.Address, row.Word(0).String())
		}
	case topicBatchMetadataUpdate:
		if len(row.Data) == 64 {
			return p.loadRange(ctx, source, row.Address, row.Word(0), row.Word(1))
		}
	case topicContractURIUpdated:
		return p.loadCollection(ctx, source, row.Address)
	}
	apps := p.index.apps()
	if row.Topics[0] == topicCollectionCreated && apps.CollectionFactory != (common.Address{}) && row.Address == apps.CollectionFactory && len(row.Topics) == 3 {
		return p.loadCollection(ctx, source, row.Addr(1))
	}
	if row.Topics[0] == topicOrderFilled && apps.Exchange != (common.Address{}) && row.Address == apps.Exchange && len(row.Topics) == 4 && len(row.Data) == 7*32 {
		return p.loadCollection(ctx, source, row.WordAddr(1))
	}
	return nil
}

// Handlers must replay exactly the discovered calls, in order, without retries.
// Repeating or omitting a call can mismatch the next request or make Complete
// fail. Any preparation error requires aborting the indexing batch.
func (p *preparedURI) request(ctx context.Context, block uint64, address common.Address, kind int, data []byte) (string, bool) {
	if p.err != nil {
		return "", false
	}
	if p.closed {
		p.err = errors.New("prepared URI input is already closed")
		return "", false
	}
	at, ok := metadataBlockOf(ctx, block)
	if !ok {
		p.err = errors.New("prepared URI request has no exact source block")
		return "", false
	}
	if p.collect != nil {
		p.count++
		_, p.err = p.collect.ExecContext(ctx, "INSERT INTO uri_requests(seq,block,hash,address,kind,data) VALUES(?,?,?,?,?,?)", p.count, block, at.hash[:], address[:], kind, data)
		return "", false
	}
	if !p.ready {
		p.err = errors.New("URI preparation is incomplete")
		return "", false
	}
	var number uint64
	var hash, addr, calldata, value []byte
	var storedKind, known int
	var valueHash sql.NullString
	var valueBytes sql.NullInt64
	p.err = p.db.QueryRowContext(ctx, `SELECT q.block,q.hash,q.address,q.kind,q.data,r.known,r.value_hash,length(v.value),
	 CASE WHEN length(v.value)<=? THEN v.value END
	 FROM uri_requests q JOIN uri_results r ON r.seq=q.seq LEFT JOIN uri_values v ON v.hash=r.value_hash WHERE q.seq=?`, indexer.MaxURIBytes+1, p.consumed+1).Scan(&number, &hash, &addr, &storedKind, &calldata, &known, &valueHash, &valueBytes, &value)
	if p.err != nil {
		return "", false
	}
	if number != block || !bytes.Equal(hash, at.hash[:]) || !bytes.Equal(addr, address[:]) || storedKind != kind || !bytes.Equal(calldata, data) {
		p.err = errors.New("prepared URI request identity changed")
		return "", false
	}
	if known == 0 && kind == uriRequest && !valueHash.Valid && !valueBytes.Valid && value == nil {
		p.consumed++
		return "", false
	}
	digest := sha256.Sum256(value)
	if known != 1 || !valueHash.Valid || !valueBytes.Valid || valueBytes.Int64 < 1 || valueBytes.Int64 > indexer.MaxURIBytes+1 || valueBytes.Int64 != int64(len(value)) || hex.EncodeToString(digest[:]) != valueHash.String || value[0] != 1 || !utf8.Valid(value[1:]) {
		p.err = errors.New("prepared URI response is malformed")
		return "", false
	}
	p.consumed++
	return string(value[1:]), true
}

func (p *preparedURI) Resolve(ctx context.Context) error {
	if p.closed || p.collect != nil || p.ready || p.err != nil {
		return errors.New("invalid URI preparation phase")
	}
	var cursor int64
	for cursor < p.count {
		rows, err := p.db.QueryContext(ctx, "SELECT seq,block,hash,address,kind,data FROM uri_requests WHERE seq>? ORDER BY seq LIMIT ?", cursor, preparedRequestPage)
		if err != nil {
			return err
		}
		var page []uriRequestRow
		for rows.Next() {
			var item uriRequestRow
			var hash, address []byte
			if err := rows.Scan(&item.seq, &item.block, &hash, &address, &item.kind, &item.data); err != nil {
				rows.Close()
				return err
			}
			if len(hash) != common.HashLength || len(address) != common.AddressLength {
				rows.Close()
				return errors.New("invalid prepared URI request encoding")
			}
			copy(item.hash[:], hash)
			copy(item.address[:], address)
			page = append(page, item)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(page) == 0 {
			return errors.New("prepared URI request sequence is incomplete")
		}
		for _, item := range page {
			if item.seq != cursor+1 {
				return errors.New("prepared URI request sequence has a gap")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			callctx := withMetadataBlock(ctx, item.block, item.hash)
			var value string
			known := false
			switch item.kind {
			case uriRequest:
				value, known = p.index.readObservedURI(callctx, item.block, item.address, item.data)
			case stringRequest:
				value, known = p.index.host.CallString(callctx, item.address, string(item.data)), true
			default:
				return errors.New("unknown prepared URI request kind")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			var key any
			if known {
				if len(value) > indexer.MaxURIBytes || !utf8.ValidString(value) {
					return errors.New("invalid prepared string response")
				}
				// A version byte makes a proven empty string a nonempty BLOB,
				// distinct from SQL NULL and from a missing retained value.
				raw := append([]byte{1}, []byte(value)...)
				digest := sha256.Sum256(raw)
				key = hex.EncodeToString(digest[:])
				if _, err := p.db.ExecContext(ctx, "INSERT OR IGNORE INTO uri_values(hash,value) VALUES(?,?)", key, raw); err != nil {
					return err
				}
			}
			if _, err := p.db.ExecContext(ctx, "INSERT INTO uri_results(seq,known,value_hash) VALUES(?,?,?)", item.seq, known, key); err != nil {
				return err
			}
			cursor = item.seq
		}
	}
	var count int64
	if err := p.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM uri_results").Scan(&count); err != nil {
		return err
	}
	if count != p.count {
		return errors.New("prepared URI responses are incomplete")
	}
	p.ready = true
	return nil
}

func (p *preparedURI) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, preparedURIKey{}, p)
}

func (p *preparedURI) Complete() error {
	if p.closed {
		return errors.New("prepared URI input is already closed")
	}
	if p.err != nil {
		return p.err
	}
	if !p.ready || p.consumed != p.count {
		return errors.New("prepared URI input was not consumed completely")
	}
	return nil
}

func (p *preparedURI) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	if p.collect != nil {
		p.collect.Rollback()
		p.collect = nil
	}
	if p.db != nil {
		err := p.db.Close()
		p.db = nil
		return err
	}
	return nil
}
