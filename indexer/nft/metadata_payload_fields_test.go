package nft

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

func payloadText(value string) *string { return &value }

func encodePayloadFields(t *testing.T, value metadataJSON) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Each payload has a matching digest and canonical history/block reference,
// so a refusal must reach the declared field/envelope check, not a hash guard.
// The complete table snapshot must remain identical after every read.
func assertPayloadFields(t *testing.T, raw []byte, want metadataJSON, wantError string) {
	t.Helper()
	_, db, _ := uriHistoryFixture(t, false)
	address, hash := common.Address{19: 21}, common.Hash{31: 17}
	uriBlock(t, db, 1, hash)
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	if _, err := db.Exec("INSERT INTO nft_metadata_payloads(hash,payload) VALUES(?,?)", key, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nft_metadata_history(kind,address,token_id,block,block_hash,uri_verified,payload_hash)
	 VALUES('token',?,'1',1,?,1,?)`, address.Hex(), hash.Hex(), key); err != nil {
		t.Fatal(err)
	}
	before := uriHistoryDump(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := retainedMetadata(tx, "token", address.Hex(), "1", 1)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if wantError == "" {
		if readErr != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("valid payload did not round-trip exactly: error=%v", readErr)
		}
	} else {
		if readErr == nil || !strings.Contains(readErr.Error(), wantError) {
			t.Fatalf("invalid payload returned %v, want %q", readErr, wantError)
		}
		if !reflect.DeepEqual(got, metadataJSON{Version: 1}) {
			t.Fatal("refused payload exposed partial metadata")
		}
	}
	if uriHistoryDump(t, db) != before {
		t.Fatal("payload validation changed a canonical table")
	}
}

func TestURIHistoryPayloadFields(t *testing.T) {
	base := metadataJSON{Version: 1, URI: payloadText("synthetic:uri-text"),
		Meta: payloadText(`{"label":"payload-text"}`), Name: payloadText("name-text"), Image: payloadText("synthetic:image-text")}
	withoutDocument := metadataJSON{Version: 1, URI: payloadText("synthetic:uri-text")}
	change := func(edit func(*metadataJSON)) func(*testing.T, metadataJSON) []byte {
		return func(t *testing.T, value metadataJSON) []byte {
			edit(&value)
			return encodePayloadFields(t, value)
		}
	}
	invalidText := func(marker string) func(*testing.T, metadataJSON) []byte {
		return func(t *testing.T, value metadataJSON) []byte {
			raw := encodePayloadFields(t, value)
			if bytes.Count(raw, []byte(marker)) != 1 {
				t.Fatal("invalid-text mutation is not isolated to one field")
			}
			replacement := append([]byte{0xff}, []byte(marker[1:])...)
			return bytes.Replace(raw, []byte(marker), replacement, 1)
		}
	}
	for _, test := range []struct {
		name    string
		valid   metadataJSON
		invalid func(*testing.T, metadataJSON) []byte
		want    string
	}{
		{"version", base, change(func(v *metadataJSON) { v.Version = 2 }), "unsupported metadata payload version"},
		{"trailing-value", base, func(t *testing.T, v metadataJSON) []byte { return append(encodePayloadFields(t, v), []byte(` {}`)...) }, "metadata payload has trailing data"},
		{"trailing-junk", base, func(t *testing.T, v metadataJSON) []byte { return append(encodePayloadFields(t, v), '!') }, "metadata payload has trailing data"},
		{"unknown-field", base, func(t *testing.T, v metadataJSON) []byte {
			raw := encodePayloadFields(t, v)
			return append(raw[:len(raw)-1], []byte(`,"extra":true}`)...)
		}, "unknown field"},
		{"invalid-utf8-uri", base, invalidText("uri-text"), "metadata payload contains invalid text"},
		{"invalid-utf8-document", base, invalidText("payload-text"), "metadata payload contains invalid text"},
		{"invalid-utf8-name", base, invalidText("name-text"), "metadata payload contains invalid text"},
		{"invalid-utf8-image", base, invalidText("image-text"), "metadata payload contains invalid text"},
		{"oversize-uri", base, change(func(v *metadataJSON) { v.URI = payloadText(strings.Repeat("u", indexer.MaxURIBytes+1)) }), "metadata URI exceeds its bound"},
		{"invalid-json-document", base, change(func(v *metadataJSON) { v.Meta = payloadText(`{"label":`) }), "metadata JSON is invalid"},
		{"oversize-document", base, change(func(v *metadataJSON) { v.Meta = payloadText(`"` + strings.Repeat("m", (64<<10)-1) + `"`) }), "metadata JSON is invalid"},
		{"name-without-document", withoutDocument, change(func(v *metadataJSON) { v.Name = payloadText("orphan-name") }), "metadata display fields lack their document"},
		{"image-without-document", withoutDocument, change(func(v *metadataJSON) { v.Image = payloadText("synthetic:orphan-image") }), "metadata display fields lack their document"},
		{"oversize-payload", base, func(t *testing.T, v metadataJSON) []byte {
			v.Image = payloadText("")
			v.Image = payloadText(strings.Repeat("p", metadataPayloadLimit-len(encodePayloadFields(t, v))+1))
			raw := encodePayloadFields(t, v)
			if len(raw) != metadataPayloadLimit+1 {
				t.Fatal("oversize payload is not exactly one byte over its bound")
			}
			return raw
		}, "metadata payload size is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPayloadFields(t, encodePayloadFields(t, test.valid), test.valid, "")
			assertPayloadFields(t, test.invalid(t, test.valid), metadataJSON{}, test.want)
		})
	}
}

func TestURIHistoryPayloadBoundaries(t *testing.T) {
	base := metadataJSON{Version: 1, URI: payloadText("synthetic:uri-text"), Meta: payloadText(`{}`)}
	for _, test := range []struct {
		name string
		edit func(*testing.T, *metadataJSON)
	}{
		{"uri-at-limit", func(_ *testing.T, v *metadataJSON) { v.URI = payloadText(strings.Repeat("u", indexer.MaxURIBytes)) }},
		{"document-at-limit", func(_ *testing.T, v *metadataJSON) { v.Meta = payloadText(`"` + strings.Repeat("m", (64<<10)-2) + `"`) }},
		{"literal-replacement-character", func(_ *testing.T, v *metadataJSON) { v.URI = payloadText("synthetic:\ufffd") }},
		{"payload-at-limit", func(t *testing.T, v *metadataJSON) {
			v.Image = payloadText("")
			v.Image = payloadText(strings.Repeat("p", metadataPayloadLimit-len(encodePayloadFields(t, *v))))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			test.edit(t, &value)
			raw := encodePayloadFields(t, value)
			if test.name == "payload-at-limit" && len(raw) != metadataPayloadLimit {
				t.Fatal("valid payload is not exactly at its bound")
			}
			assertPayloadFields(t, raw, value, "")
		})
	}
}
