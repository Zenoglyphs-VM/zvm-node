package nft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
)

const metadataHistoryVersionKey = "nft_metadata_history_v1"
const metadataPayloadLimit = 1 << 20
const metadataPayloadBlockBudget = 16 << 20
const metadataRestorePage = 128

const metadataHistorySchema = `
CREATE TABLE IF NOT EXISTS nft_metadata_payloads (
	hash TEXT PRIMARY KEY,
	payload BLOB NOT NULL
);
-- This is a canonical derived journal. Rewind removes orphan observations,
-- just as it removes orphan source logs, so cold replay has identical tables.
CREATE TABLE IF NOT EXISTS nft_metadata_history (
	kind TEXT NOT NULL CHECK (kind IN ('token','collection')),
	address TEXT NOT NULL,
	token_id TEXT NOT NULL,
	block INTEGER NOT NULL,
	block_hash TEXT NOT NULL,
	uri_verified INTEGER NOT NULL CHECK (uri_verified IN (0,1)),
	payload_hash TEXT,
	PRIMARY KEY(kind,address,token_id,block,block_hash)
);
CREATE INDEX IF NOT EXISTS nft_metadata_payload_refs ON nft_metadata_history(payload_hash);
-- A pre-upgrade cache is evidence, not a historical URI proof. Archive its
-- exact nullable fields before invalidation; never rebind it to a new header.
CREATE TABLE IF NOT EXISTS nft_metadata_legacy (
	kind TEXT NOT NULL,
	address TEXT NOT NULL,
	token_id TEXT NOT NULL,
	uri TEXT,
	meta TEXT,
	name TEXT,
	image TEXT,
	symbol TEXT,
	PRIMARY KEY(kind,address,token_id)
);
`

type metadataBlockKey struct{}
type metadataBlock struct {
	number uint64
	hash   common.Hash
}

func withMetadataBlock(ctx context.Context, number uint64, hash common.Hash) context.Context {
	return context.WithValue(ctx, metadataBlockKey{}, metadataBlock{number, hash})
}

func metadataBlockOf(ctx context.Context, number uint64) (metadataBlock, bool) {
	value, ok := ctx.Value(metadataBlockKey{}).(metadataBlock)
	return value, ok && value.number == number && value.hash != (common.Hash{})
}

// The optional hash-capable host has no latest-state fallback. A failed read or
// an exhausted budget is an unknown observation, not a proven empty URI.
func (x *Index) readObservedURI(ctx context.Context, block uint64, addr common.Address, data []byte) (string, bool) {
	if prepared := uriPreparation(ctx); prepared != nil {
		return prepared.request(ctx, block, addr, uriRequest, data)
	}
	at, ok := metadataBlockOf(ctx, block)
	reader, supported := x.host.(indexer.HashURIHost)
	if !ok || !supported {
		return "", false
	}
	x.mu.Lock()
	if x.budgetBlock != block {
		x.budgetBlock, x.spent, x.warned = block, 0, false
	}
	remaining := metadataBudget - x.spent
	if remaining <= 0 {
		warn := !x.warned
		x.warned = true
		x.mu.Unlock()
		if warn {
			x.log().Warn("metadata reads for this block took their whole budget; the rest are indexed without it", "block", block, "budget", metadataBudget)
		}
		return "", false
	}
	x.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	start := time.Now()
	uri, err := reader.CallURIAtHash(bounded, addr, data, at.hash)
	x.mu.Lock()
	x.spent += time.Since(start)
	x.mu.Unlock()
	if err != nil || len(uri) > indexer.MaxURIBytes || !utf8.ValidString(uri) {
		return "", false
	}
	return uri, true
}

type metadataValue struct {
	uri         string
	meta        tokenMeta
	verified    bool
	payloadHash string
}

type metadataJSON struct {
	Version int     `json:"version"`
	URI     *string `json:"uri"`
	Meta    *string `json:"meta"`
	Name    *string `json:"name"`
	Image   *string `json:"image"`
}

func metadataPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func metadataNull(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *value, Valid: true}
}

func (x *Index) observedMetadata(uri string, verified bool) metadataValue {
	if !verified {
		return metadataValue{}
	}
	return metadataValue{uri: uri, meta: x.resolveMeta(uri), verified: true}
}

// All payload and history writes use the caller's indexing transaction. The
// legacy per-block time budget and this unique-payload budget survive rollback.
// Indexer prepares external URI and collection-name responses before opening
// its canonical writer. Direct plugin callers must supply prepared input to use
// that boundary; the standalone host-reading helpers retain their original API.
func (x *Index) admitMetadata(ctx context.Context, tx *sql.Tx, block uint64, value metadataValue) (metadataValue, error) {
	if prepared := uriPreparation(ctx); prepared != nil && prepared.collect != nil {
		return metadataValue{}, nil
	}
	at, ok := metadataBlockOf(ctx, block)
	if !ok {
		return metadataValue{}, nil
	}
	var stored string
	if err := tx.QueryRow("SELECT hash FROM blocks WHERE number=?", block).Scan(&stored); err != nil {
		return metadataValue{}, err
	}
	if stored != at.hash.Hex() {
		return metadataValue{}, errors.New("metadata source block moved")
	}
	if !value.verified {
		return metadataValue{}, nil
	}
	if value.meta.raw.Valid && !utf8.ValidString(value.meta.raw.String) {
		// Preserve the proven URI, but do not serialize replacement characters
		// as if they were the original local media document.
		value.meta = tokenMeta{}
	}
	doc := metadataJSON{Version: 1, URI: metadataPointer(nullString(value.uri)), Meta: metadataPointer(value.meta.raw), Name: metadataPointer(value.meta.name), Image: metadataPointer(value.meta.image)}
	raw, err := json.Marshal(doc)
	if err != nil {
		return metadataValue{}, err
	}
	if len(raw) > metadataPayloadLimit {
		return metadataValue{}, nil
	}
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	var existing []byte
	err = tx.QueryRow("SELECT CASE WHEN length(payload)<=? THEN payload END FROM nft_metadata_payloads WHERE hash=?", metadataPayloadLimit, key).Scan(&existing)
	if err == nil {
		if !bytes.Equal(existing, raw) {
			return metadataValue{}, errors.New("metadata payload digest conflict")
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		x.mu.Lock()
		if x.historyBlock != block {
			x.historyBlock, x.historyBytes = block, 0
		}
		admitted := int64(len(raw)) <= metadataPayloadBlockBudget-x.historyBytes
		if admitted {
			x.historyBytes += int64(len(raw))
		}
		x.mu.Unlock()
		if !admitted {
			return metadataValue{}, nil
		}
		if _, err := tx.Exec("INSERT INTO nft_metadata_payloads(hash,payload) VALUES(?,?)", key, raw); err != nil {
			return metadataValue{}, err
		}
	} else {
		return metadataValue{}, err
	}
	value.payloadHash = key
	return value, nil
}

func recordMetadata(ctx context.Context, tx *sql.Tx, kind, address, id string, block uint64, value metadataValue) error {
	if prepared := uriPreparation(ctx); prepared != nil && prepared.collect != nil {
		return nil
	}
	at, ok := metadataBlockOf(ctx, block)
	if !ok {
		return nil
	}
	var payload any
	if value.verified {
		if len(value.payloadHash) != 64 {
			return errors.New("verified metadata has no retained payload")
		}
		payload = value.payloadHash
	}
	_, err := tx.Exec(`INSERT INTO nft_metadata_history(kind,address,token_id,block,block_hash,uri_verified,payload_hash)
	 VALUES(?,?,?,?,?,?,?) ON CONFLICT(kind,address,token_id,block,block_hash)
	 DO UPDATE SET uri_verified=excluded.uri_verified,payload_hash=excluded.payload_hash`, kind, address, id, block, at.hash.Hex(), value.verified, payload)
	return err
}

func archivedMetadata(tx *sql.Tx) error {
	var done int
	if err := tx.QueryRow("SELECT COUNT(*) FROM index_meta WHERE key=? AND value=1", metadataHistoryVersionKey).Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return nil
	}
	for _, statement := range []string{
		`INSERT OR IGNORE INTO nft_metadata_legacy(kind,address,token_id,uri,meta,name,image)
		 SELECT 'token',collection,token_id,uri,meta,name,image FROM nft_tokens
		 WHERE uri IS NOT NULL OR meta IS NOT NULL OR name IS NOT NULL OR image IS NOT NULL`,
		`INSERT OR IGNORE INTO nft_metadata_legacy(kind,address,token_id,uri,meta,name,symbol)
		 SELECT 'collection',address,'',contract_uri,meta,name,symbol FROM nft_collections
		 WHERE contract_uri IS NOT NULL OR meta IS NOT NULL`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func retainedMetadata(tx *sql.Tx, kind, address, id string, block uint64) (metadataJSON, error) {
	empty := metadataJSON{Version: 1}
	var key sql.NullString
	var verified int
	err := tx.QueryRow(`SELECT h.payload_hash,h.uri_verified FROM nft_metadata_history h
	 JOIN blocks b ON b.number=h.block AND b.hash=h.block_hash
	 WHERE h.kind=? AND h.address=? AND h.token_id=? AND h.block<=?
	 ORDER BY h.block DESC LIMIT 1`, kind, address, id, block).Scan(&key, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if verified == 0 && !key.Valid {
		return empty, nil
	}
	if verified != 1 || !key.Valid || len(key.String) != 64 {
		return empty, errors.New("metadata observation is malformed")
	}
	var size int
	if err := tx.QueryRow("SELECT length(payload) FROM nft_metadata_payloads WHERE hash=?", key.String).Scan(&size); err != nil {
		return empty, err
	}
	if size <= 0 || size > metadataPayloadLimit {
		return empty, errors.New("metadata payload size is invalid")
	}
	var raw []byte
	if err := tx.QueryRow("SELECT payload FROM nft_metadata_payloads WHERE hash=?", key.String).Scan(&raw); err != nil {
		return empty, err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != key.String {
		return empty, errors.New("metadata payload hash mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value metadataJSON
	if err := decoder.Decode(&value); err != nil {
		return empty, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return empty, errors.New("metadata payload has trailing data")
	}
	if value.Version != 1 {
		return empty, errors.New("unsupported metadata payload version")
	}
	for _, field := range []*string{value.URI, value.Meta, value.Name, value.Image} {
		if field != nil && !utf8.ValidString(*field) {
			return empty, errors.New("metadata payload contains invalid text")
		}
	}
	if value.URI != nil && len(*value.URI) > indexer.MaxURIBytes {
		return empty, errors.New("metadata URI exceeds its bound")
	}
	if value.Meta != nil && (len(*value.Meta) > 64<<10 || !json.Valid([]byte(*value.Meta))) {
		return empty, errors.New("metadata JSON is invalid")
	}
	if value.Meta == nil && (value.Name != nil || value.Image != nil) {
		return empty, errors.New("metadata display fields lack their document")
	}
	return value, nil
}

// Rebuild only the derived cache. Unknown legacy fields are archived unchanged;
// an absent proof never inherits a made-up block/hash. Canonical journal rows
// and their referenced payload set must converge exactly with cold replay.
func restoreMetadata(tx *sql.Tx, block uint64) error {
	if err := archivedMetadata(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM nft_metadata_history WHERE block>?
	 OR NOT EXISTS (SELECT 1 FROM blocks b WHERE b.number=nft_metadata_history.block AND b.hash=nft_metadata_history.block_hash)`, block); err != nil {
		return err
	}
	for _, kind := range []string{"token", "collection"} {
		var lastAddress, lastID string
		first := true
		for {
			var rows *sql.Rows
			var err error
			if kind == "token" {
				if first {
					rows, err = tx.Query("SELECT collection,token_id FROM nft_tokens ORDER BY collection,token_id LIMIT ?", metadataRestorePage)
				} else {
					rows, err = tx.Query(`SELECT collection,token_id FROM nft_tokens WHERE (collection,token_id)>(?,?)
					 ORDER BY collection,token_id LIMIT ?`, lastAddress, lastID, metadataRestorePage)
				}
			} else {
				if first {
					rows, err = tx.Query("SELECT address,'' FROM nft_collections ORDER BY address LIMIT ?", metadataRestorePage)
				} else {
					rows, err = tx.Query("SELECT address,'' FROM nft_collections WHERE address>? ORDER BY address LIMIT ?", lastAddress, metadataRestorePage)
				}
			}
			if err != nil {
				return err
			}
			var keys [][2]string
			for rows.Next() {
				var key [2]string
				if err := rows.Scan(&key[0], &key[1]); err != nil {
					rows.Close()
					return err
				}
				if len(key[0]) > 128 || len(key[1]) > 78 {
					rows.Close()
					return errors.New("metadata cache key exceeds its bound")
				}
				keys = append(keys, key)
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if len(keys) == 0 {
				break
			}
			for _, key := range keys {
				value, err := retainedMetadata(tx, kind, key[0], key[1], block)
				if err != nil {
					return fmt.Errorf("restoring %s metadata: %w", kind, err)
				}
				if kind == "token" {
					_, err = tx.Exec(`UPDATE nft_tokens SET uri=?,meta=?,name=?,image=? WHERE collection=? AND token_id=?
					 AND (uri IS NOT ? OR meta IS NOT ? OR name IS NOT ? OR image IS NOT ?)`,
						metadataNull(value.URI), metadataNull(value.Meta), metadataNull(value.Name), metadataNull(value.Image), key[0], key[1],
						metadataNull(value.URI), metadataNull(value.Meta), metadataNull(value.Name), metadataNull(value.Image))
				} else {
					_, err = tx.Exec(`UPDATE nft_collections SET contract_uri=?,meta=? WHERE address=?
					 AND (contract_uri IS NOT ? OR meta IS NOT ?)`, metadataNull(value.URI), metadataNull(value.Meta), key[0], metadataNull(value.URI), metadataNull(value.Meta))
				}
				if err != nil {
					return err
				}
			}
			lastAddress, lastID = keys[len(keys)-1][0], keys[len(keys)-1][1]
			first = false
		}
	}
	if _, err := tx.Exec(`DELETE FROM nft_metadata_payloads WHERE NOT EXISTS
	 (SELECT 1 FROM nft_metadata_history h WHERE h.payload_hash=nft_metadata_payloads.hash)`); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT OR REPLACE INTO index_meta(key,value) VALUES(?,1)", metadataHistoryVersionKey)
	return err
}
