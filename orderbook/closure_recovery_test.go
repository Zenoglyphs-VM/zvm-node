package orderbook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// These are declared read-only ABI/header ports, not a wallet or EVM. Actual
// default-client restore remains a separate fullclient_recovery qualification.
type closureProofReply struct {
	code, remaining, filled uint64
	mutate                  func([]recoveryRPCResult) []recoveryRPCResult
}

type closureProofRPC struct {
	*recoveryRPC
	book              *Book
	replies           map[common.Hash]closureProofReply
	legacySingleCheck bool
}

func (p *closureProofRPC) CallContractAtHash(ctx context.Context, msg ethereum.CallMsg, hash common.Hash) ([]byte, error) {
	// Only the pre-existing cancellation regression uses its original single-check port.
	// Complete closure proofs still require two separately declared ABI results.
	if p.legacySingleCheck && len(msg.Data) >= 4 {
		args, err := mcABI.Methods["aggregate3"].Inputs.Unpack(msg.Data[4:])
		if err != nil {
			return nil, err
		}
		if reflect.ValueOf(args[0]).Len() == 1 {
			return p.recoveryRPC.CallContractAtHash(ctx, msg, hash)
		}
	}
	if p.blockCall {
		if err := p.wait(ctx); err != nil {
			return nil, err
		}
	}
	reply, ok := p.replies[hash]
	if !ok {
		return nil, errors.New("declared historical state unavailable")
	}
	if msg.To == nil || *msg.To != multicall3 || len(msg.Data) < 4 {
		return nil, errors.New("undeclared closure contract port")
	}
	args, err := mcABI.Methods["aggregate3"].Inputs.Unpack(msg.Data[4:])
	if err != nil {
		return nil, err
	}
	calls := reflect.ValueOf(args[0])
	if calls.Len() != 2 {
		return nil, errors.New("closure proof must contain exactly two calls")
	}
	for i, name := range []string{"checkOrder", "filled"} {
		call := calls.Index(i)
		data := call.FieldByName("CallData").Bytes()
		if call.FieldByName("Target").Interface().(common.Address) != p.book.cfg.Exchange() || !call.FieldByName("AllowFailure").Bool() || len(data) < 4 || !bytes.Equal(data[:4], p.book.cfg.Artifact.ABI.Methods[name].ID) {
			return nil, errors.New("closure evidence selector or exchange changed")
		}
	}
	word := func(n uint64) []byte { return common.LeftPadBytes(new(big.Int).SetUint64(n).Bytes(), 32) }
	results := []recoveryRPCResult{{true, append(word(reply.code), word(reply.remaining)...)}, {true, word(reply.filled)}}
	if reply.mutate != nil {
		results = reply.mutate(results)
	}
	return mcABI.Methods["aggregate3"].Outputs.Pack(results)
}

func closedRecoveryFixture(t *testing.T) (*Book, *closureProofRPC, *sql.DB, common.Hash, observation) {
	t.Helper()
	b, base, db, hash := recoveryBook(t)
	p := &closureProofRPC{recoveryRPC: base, book: b, replies: map[common.Hash]closureProofReply{}}
	b.cfg.Eth = p
	p.replies[p.header.Hash()] = closureProofReply{uint64(CodeCancelled), 0, 3, nil}
	if _, err := db.Exec(`UPDATE nft_orders SET status=?,code=?,filled=10,checked_epoch=0,filled_epoch=-1,
		checked_block=0,checked_hash='',closed_block=NULL WHERE hash=?`, StatusFilled, CodeFilled, hash.Hex()); err != nil {
		t.Fatal(err)
	}
	at, err := b.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return b, p, db, hash, at
}

func TestClosedOrderCompleteEvidence(t *testing.T) {
	for _, name := range []string{"ok", "cancelled-partial", "expired-partial", "bad-signature-partial", "filled", "missing-result", "failed-check", "failed-fill", "short-check", "short-fill", "code-range", "filled-overflow", "filled-above-amount", "remaining-above-amount", "inconsistent-remaining", "unexpected-refusal-remaining"} {
		t.Run(name, func(t *testing.T) {
			b, p, db, hash, at := closedRecoveryFixture(t)
			in, _, _, err := b.Get(hash)
			if err != nil {
				t.Fatal(err)
			}
			reply := closureProofReply{uint64(CodeCancelled), 0, 3, nil}
			valid := true
			switch name {
			case "ok":
				reply.code, reply.remaining, reply.filled = uint64(CodeOK), 8, 2
			case "expired-partial":
				reply.code = uint64(CodeExpired)
			case "bad-signature-partial":
				reply.code = uint64(CodeBadSignature)
			case "filled":
				reply.code, reply.filled = uint64(CodeFilled), 10
			case "missing-result":
				reply.mutate = func(r []recoveryRPCResult) []recoveryRPCResult { return r[:1] }
			case "failed-check":
				reply.mutate = func(r []recoveryRPCResult) []recoveryRPCResult { r[0].Success = false; return r }
			case "failed-fill":
				reply.mutate = func(r []recoveryRPCResult) []recoveryRPCResult { r[1].Success = false; return r }
			case "short-check":
				reply.mutate = func(r []recoveryRPCResult) []recoveryRPCResult { r[0].ReturnData = r[0].ReturnData[:63]; return r }
			case "short-fill":
				reply.mutate = func(r []recoveryRPCResult) []recoveryRPCResult { r[1].ReturnData = r[1].ReturnData[:31]; return r }
			case "code-range":
				reply.code = 256
			case "filled-overflow":
				reply.filled = uint64(1) << 63
			case "filled-above-amount":
				reply.filled = 11
			case "remaining-above-amount":
				reply.code, reply.remaining = uint64(CodeOK), 11
			case "inconsistent-remaining":
				reply.code, reply.remaining, reply.filled = uint64(CodeOK), 8, 3
			case "unexpected-refusal-remaining":
				reply.remaining = 1
			}
			valid = name == "ok" || name == "cancelled-partial" || name == "expired-partial" || name == "bad-signature-partial" || name == "filled"
			p.replies[at.head.Hash()] = reply
			before := recoveryBookState(t, db)
			got, err := b.completeCheck(context.Background(), b.cfg.Exchange(), in, at.head)
			if valid && (err != nil || got.code != uint8(reply.code) || got.remaining != reply.remaining || got.filled != int64(reply.filled)) {
				t.Fatalf("independent complete evidence lost: %+v/%v", got, err)
			}
			if !valid && err == nil {
				t.Fatal("invalid complete evidence was accepted")
			}
			if recoveryBookState(t, db) != before {
				t.Fatal("read-only closure evidence changed stored intent or caches")
			}
		})
	}
}

func TestClosedOrderRecovery(t *testing.T) {
	for _, name := range []string{"legacy-filled", "legacy-cancelled", "legacy-expired", "legacy-bad-signature", "orphan-reopens", "valid-history", "unavailable-history", "commit-refusal"} {
		t.Run(name, func(t *testing.T) {
			b, p, db, hash, at := closedRecoveryFixture(t)
			var original string
			if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&original); err != nil {
				t.Fatal(err)
			}
			reply := p.replies[at.head.Hash()]
			switch name {
			case "legacy-filled":
				reply.code, reply.filled = uint64(CodeFilled), 10
			case "legacy-expired":
				reply.code = uint64(CodeExpired)
			case "legacy-bad-signature":
				reply.code = uint64(CodeBadSignature)
			case "orphan-reopens":
				reply.code, reply.remaining, reply.filled = uint64(CodeOK), 8, 2
			}
			p.replies[at.head.Hash()] = reply
			if name == "valid-history" || name == "unavailable-history" {
				old := types.CopyHeader(p.header)
				old.Number, old.Time = big.NewInt(1), 1010
				p.headers = map[uint64]*types.Header{1: old}
				if name == "valid-history" {
					p.replies[old.Hash()] = reply
				}
				if _, err := db.Exec(`UPDATE nft_orders SET status=?,code=?,filled=3,checked_block=1,checked_hash=?,closed_block=1 WHERE hash=?`, StatusCancelled, CodeCancelled, old.Hash().Hex(), hash.Hex()); err != nil {
					t.Fatal(err)
				}
			}
			if name == "commit-refusal" {
				if _, err := db.Exec(`CREATE TRIGGER closure_refusal BEFORE UPDATE ON nft_orders BEGIN SELECT RAISE(ABORT,'declared closure refusal'); END`); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryBookState(t, db)
			_, _, code, err := b.Get(hash)
			if err != nil || code != CodeUnchecked {
				t.Fatalf("unproved closed cache appeared checked: %d/%v", code, err)
			}
			err = b.reconcileClosures(context.Background(), b.cfg.Exchange(), at)
			if name == "commit-refusal" {
				if err == nil || !strings.Contains(err.Error(), "declared closure refusal") || recoveryBookState(t, db) != before {
					t.Fatalf("refused closure left a partial write: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var raw, status, checkedHash string
			var filled, checkedBlock, checkEpoch, fillEpoch int64
			var closed sql.NullInt64
			if err := db.QueryRow(`SELECT input,status,filled,checked_block,checked_hash,closed_block,checked_epoch,filled_epoch FROM nft_orders WHERE hash=?`, hash.Hex()).Scan(&raw, &status, &filled, &checkedBlock, &checkedHash, &closed, &checkEpoch, &fillEpoch); err != nil {
				t.Fatal(err)
			}
			wantBlock, wantHash := int64(2), at.head.Hash().Hex()
			if name == "valid-history" {
				wantBlock, wantHash = 1, p.headers[1].Hash().Hex()
			}
			if raw != original || status != statusFor(uint8(reply.code)) || filled != int64(reply.filled) || checkedBlock != wantBlock || checkedHash != wantHash || checkEpoch != int64(at.epoch) || fillEpoch != int64(at.epoch) || closed.Valid != (status != StatusOpen) || closed.Valid && closed.Int64 != wantBlock {
				t.Fatalf("closure recovery lost intent or provenance: %s/%d checked=%d/%s closed=%v epochs=%d/%d", status, filled, checkedBlock, checkedHash, closed, checkEpoch, fillEpoch)
			}
			if err := b.reconcileClosures(context.Background(), b.cfg.Exchange(), at); err != nil {
				t.Fatalf("completed closure recovery was not idempotent: %v", err)
			}
		})
	}
}

func TestClosedOrderRecoveryFences(t *testing.T) {
	for _, name := range []string{"rewind", "restart", "newer-row"} {
		t.Run(name, func(t *testing.T) {
			b, p, db, hash, at := closedRecoveryFixture(t)
			p.blockCall = true
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- b.reconcileClosures(ctx, b.cfg.Exchange(), at) }()
			awaitRecoveryRPC(t, p.recoveryRPC)
			switch name {
			case "rewind":
				if err := b.Rewind(1); err != nil {
					t.Fatal(err)
				}
			case "restart":
				if _, err := New(b.cfg); err != nil {
					t.Fatal(err)
				}
			case "newer-row":
				if _, err := db.Exec(`UPDATE nft_orders SET status=?,code=?,filled=4,checked_epoch=?,filled_epoch=?,checked_block=2,checked_hash=?,closed_block=2 WHERE hash=?`, StatusCancelled, CodeCancelled, at.epoch, at.epoch, at.head.Hash().Hex(), hash.Hex()); err != nil {
					t.Fatal(err)
				}
			}
			after := recoveryBookState(t, db)
			close(p.release)
			if err := <-done; !errors.Is(err, errStaleObservation) {
				t.Fatalf("old closure response crossed its fence: %v", err)
			}
			if recoveryBookState(t, db) != after {
				t.Fatal("late closure response replaced newer state")
			}
		})
	}
}

func TestClosedOrderRecoveryPages(t *testing.T) {
	b, p, db, hash, at := closedRecoveryFixture(t)
	var raw string
	if err := db.QueryRow("SELECT input FROM nft_orders WHERE hash=?", hash.Hex()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < closureRecoveryBatch; i++ {
		var in Input
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		in.Order.Salt = U(int64(100 + i))
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.insert(tx, &in, b.cfg.Exchange(), check{CodeOK, 10}, at, "declared-closure-page", 1020); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE nft_orders SET status=?,code=?,filled=10,checked_epoch=0,filled_epoch=-1,checked_hash='',closed_block=NULL`, StatusFilled, CodeFilled); err != nil {
		t.Fatal(err)
	}
	p.replies[at.head.Hash()] = closureProofReply{uint64(CodeFilled), 0, 10, nil}
	for pass, expected := range []int{closureRecoveryBatch, closureRecoveryBatch + 1} {
		if err := b.reconcileClosures(context.Background(), b.cfg.Exchange(), at); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM nft_orders WHERE checked_epoch=? AND filled_epoch=?", at.epoch, at.epoch).Scan(&count); err != nil || count != expected {
			t.Fatalf("page %d checked %d rows, want %d: %v", pass, count, expected, err)
		}
	}
}
