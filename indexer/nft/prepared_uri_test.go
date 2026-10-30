package nft

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/ethereum/go-ethereum/common"
)

func preparedURIRows(block uint64, hash common.Hash) []indexer.LogRow {
	address := common.Address{19: 21}
	mint := func(id byte) indexer.LogRow {
		return indexer.LogRow{Address: address, Block: block, BlockHash: hash,
			Topics: []common.Hash{topicTransfer, {}, {31: 22}, {31: id}}}
	}
	first, second := mint(1), mint(2)
	transfer := mint(1)
	transfer.Topics[1], transfer.Topics[2] = common.Hash{31: 22}, common.Hash{31: 23}
	update := indexer.LogRow{Address: address, Block: block, BlockHash: hash,
		Topics: []common.Hash{topicMetadataUpdate}, Data: common.LeftPadBytes([]byte{1}, 32)}
	batch := indexer.LogRow{Address: address, Block: block, BlockHash: hash,
		Topics: []common.Hash{topicBatchMetadataUpdate}, Data: append(make([]byte, 32), common.LeftPadBytes([]byte{2}, 32)...)}
	collection := indexer.LogRow{Address: address, Block: block, BlockHash: hash, Topics: []common.Hash{topicContractURIUpdated}}
	return []indexer.LogRow{first, second, transfer, update, batch, collection}
}

func collectPreparedURITest(t *testing.T, x *Index, db *sql.DB, rows []indexer.LogRow) *preparedURI {
	t.Helper()
	source, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Rollback()
	item, err := x.CollectLogs(context.Background(), source, func(visit func(indexer.LogRow) error) error {
		for _, row := range rows {
			if err := visit(row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := item.(*preparedURI)
	t.Cleanup(func() { p.Close() })
	if err := source.Commit(); err != nil {
		t.Fatal(err)
	}
	return p
}

func applyPreparedURITest(x *Index, db *sql.DB, p *preparedURI, rows []indexer.LogRow) error {
	ctx := context.Background()
	if p != nil {
		ctx = p.Context(ctx)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if err := x.Handle(ctx, tx, row); err != nil {
			x.AfterRollback()
			return err
		}
	}
	if p != nil {
		if err := p.Complete(); err != nil {
			x.AfterRollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		x.AfterRollback()
		return err
	}
	return nil
}

func TestPreparedURI(t *testing.T) {
	for _, phase := range []string{"complete", "unknown", "empty", "existing", "missing-response", "corrupt-response", "missing-empty-value", "request-mismatch", "unused-response", "cancel", "discovery-refusal", "page-capacity"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			x, db, host := uriHistoryFixture(t, false)
			hash := common.Hash{31: 11}
			uriBlock(t, db, 1, hash)
			host.values[hash] = uriDocument("prepared")
			if phase == "unknown" {
				host.refused[hash] = true
			}
			if phase == "empty" || phase == "missing-empty-value" {
				host.values[hash] = ""
			}
			rows := preparedURIRows(1, hash)
			if phase == "existing" {
				if err := applyPreparedURITest(x, db, nil, rows[:1]); err != nil {
					t.Fatal(err)
				}
			}
			before := uriHistoryDump(t, db)
			if phase == "page-capacity" {
				p, err := newPreparedURI(ctx, x, 32*4096)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				refused := false
				for i := 0; i < 64; i++ {
					_, err := p.db.ExecContext(ctx, "INSERT INTO uri_values(hash,value) VALUES(?,?)", fmt.Sprint(i), make([]byte, 4096))
					if err != nil {
						refused = true
						break
					}
				}
				var pages int
				if err := p.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
					t.Fatal(err)
				}
				if !refused || pages > 32 {
					t.Fatalf("page admission: refused=%v pages=%d", refused, pages)
				}
				if got := uriHistoryDump(t, db); got != before {
					t.Fatal("capacity refusal changed a main table")
				}
				return
			}
			if phase == "discovery-refusal" {
				source, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				item, err := x.CollectLogs(ctx, source, func(visit func(indexer.LogRow) error) error {
					if err := visit(rows[0]); err != nil {
						return err
					}
					return errors.New("declared discovery iterator refusal")
				})
				source.Rollback()
				if err == nil || item != nil {
					t.Fatal("partial discovery returned a usable input")
				}
				if got := uriHistoryDump(t, db); got != before {
					t.Fatal("discovery changed a main table")
				}
				return
			}
			p := collectPreparedURITest(t, x, db, rows)
			if phase == "cancel" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				if err := p.Resolve(cancelled); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
				if p.ready || p.Complete() == nil {
					t.Fatal("cancelled preparation became complete")
				}
				if got := uriHistoryDump(t, db); got != before {
					t.Fatal("cancellation changed a main table")
				}
				return
			}
			if err := p.Resolve(ctx); err != nil {
				t.Fatal(err)
			}
			if phase == "complete" || phase == "empty" {
				var values int
				if err := p.db.QueryRow("SELECT COUNT(*) FROM uri_values").Scan(&values); err != nil {
					t.Fatal(err)
				}
				if p.count != 9 || values != 2 {
					t.Fatalf("exact request/dedup counts: %d/%d", p.count, values)
				}
			}
			switch phase {
			case "missing-response":
				if _, err := p.db.Exec("DELETE FROM uri_results WHERE seq=?", p.count); err != nil {
					t.Fatal(err)
				}
			case "corrupt-response":
				if _, err := p.db.Exec("UPDATE uri_values SET value=?", []byte("declared-corrupt-response")); err != nil {
					t.Fatal(err)
				}
			case "missing-empty-value":
				digest := sha256.Sum256([]byte{1})
				if _, err := p.db.Exec("DELETE FROM uri_values WHERE hash=?", hex.EncodeToString(digest[:])); err != nil {
					t.Fatal(err)
				}
			case "request-mismatch":
				rows[0].Topics = append([]common.Hash{}, rows[0].Topics...)
				rows[0].Topics[3] = common.Hash{31: 99}
			case "unused-response":
				rows = rows[:1]
			}
			err := applyPreparedURITest(x, db, p, rows)
			success := phase == "complete" || phase == "unknown" || phase == "empty" || phase == "existing"
			if !success {
				if err == nil {
					t.Fatal("invalid or incomplete prepared input committed")
				}
				if got := uriHistoryDump(t, db); got != before {
					t.Fatal("refused prepared input changed a main table")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, referenceHost := uriHistoryFixture(t, false)
			uriBlock(t, referenceDB, 1, hash)
			referenceHost.values[hash] = host.values[hash]
			referenceHost.refused[hash] = host.refused[hash]
			if phase == "existing" {
				if err := applyPreparedURITest(reference, referenceDB, nil, rows[:1]); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyPreparedURITest(reference, referenceDB, nil, rows); err != nil {
				t.Fatal(err)
			}
			if got, want := uriHistoryDump(t, db), uriHistoryDump(t, referenceDB); got != want {
				t.Fatalf("prepared apply differs from direct actual-handler replay\ngot %s\nwant %s", got, want)
			}
			if host.latest != 0 || referenceHost.latest != 0 {
				t.Fatal("historical preparation used latest-state URI fallback")
			}
		})
	}
}

func TestPreparedURIRollbackBudget(t *testing.T) {
	defer func(old time.Duration) { metadataBudget = old }(metadataBudget)
	metadataBudget = time.Millisecond
	x, db, _ := uriHistoryFixture(t, false)
	host := &slowHost{delay: 5 * time.Millisecond}
	if err := x.Attach(host); err != nil {
		t.Fatal(err)
	}
	for _, block := range []uint64{1, 1, 2} {
		hash := common.Hash{31: byte(block)}
		uriBlock(t, db, block, hash)
		p := collectPreparedURITest(t, x, db, preparedURIRows(block, hash))
		if err := p.Resolve(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.Close()
		x.AfterRollback()
		want := int(block)
		if host.calls != want {
			t.Fatalf("block %d renewed or lost its consumed budget: calls=%d want=%d", block, host.calls, want)
		}
	}
}

type preparedRangeHost struct {
	*uriHistoryHost
	ids []string
}

func (h *preparedRangeHost) CallURIAtHash(ctx context.Context, address common.Address, data []byte, hash common.Hash) (string, error) {
	if len(data) == 68 {
		h.ids = append(h.ids, new(big.Int).SetBytes(data[4:]).String())
	}
	return h.uriHistoryHost.CallURIAtHash(ctx, address, data, hash)
}

func TestPreparedURIRangeKeys(t *testing.T) {
	x, db, host := uriHistoryFixture(t, false)
	hash := common.Hash{31: 11}
	uriBlock(t, db, 1, hash)
	host.values[hash] = uriDocument("range")
	address := common.Address{19: 21}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5002; i++ {
		if _, err := tx.Exec("INSERT INTO nft_tokens(collection,token_id,owner,minted_block,updated_block) VALUES(?,?,'existing',1,1)", address.Hex(), fmt.Sprint(i)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO nft_collections(address,first_block) VALUES(?,1)", address.Hex()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows := preparedURIRows(1, hash)
	rows[0].Topics[3] = common.Hash{}
	rows[4].Data = append(make([]byte, 32), common.LeftPadBytes(big.NewInt(5002).Bytes(), 32)...)
	rows = []indexer.LogRow{rows[0], rows[4]}
	recorder := &preparedRangeHost{uriHistoryHost: host}
	if err := x.Attach(recorder); err != nil {
		t.Fatal(err)
	}
	p := collectPreparedURITest(t, x, db, rows)
	if err := p.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.ids) != 5001 || recorder.ids[0] != "0" || recorder.ids[1] != "0" {
		t.Fatalf("range did not retain earlier shadow mint plus exactly 5000 ordered refreshes: %d", len(recorder.ids))
	}
	prepared := append([]string{}, recorder.ids...)
	recorder.ids = nil
	if err := applyPreparedURITest(x, db, nil, rows); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(prepared) != fmt.Sprint(recorder.ids) {
		t.Fatal("prepared range changed original lexicographic selection or the 5000-token boundary")
	}
}
