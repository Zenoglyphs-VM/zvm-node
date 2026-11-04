package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const closureRecoveryBatch = 100
const closureRecoveryInputBytes = 16 << 20
const closureUnprovedSQL = `(closed_block IS NULL OR closed_block!=checked_block
 OR length(checked_hash)!=66 OR substr(checked_hash,1,2)!='0x'
 OR substr(checked_hash,3) GLOB '*[^0-9a-f]*')`

var closureIndexes = []string{
	`CREATE INDEX IF NOT EXISTS nft_orders_closure_check ON nft_orders(exchange,checked_epoch,hash) WHERE status!='open'`,
	`CREATE INDEX IF NOT EXISTS nft_orders_closure_fill ON nft_orders(exchange,filled_epoch,hash) WHERE status!='open'`,
	`CREATE INDEX IF NOT EXISTS nft_orders_closure_unproved ON nft_orders(exchange,hash) WHERE status!='open' AND ` + closureUnprovedSQL,
}

// EffectiveOrderCodeSQL requires aliases o for nft_orders and m for its
// rewind_epoch row. A closed cache without a current verified observation is
// unknown too. Original status and input remain available as cached history.
const EffectiveOrderCodeSQL = `CASE WHEN m.key IS NULL OR typeof(m.value)!='integer' OR m.value<0
	OR o.checked_epoch!=m.value
	OR (o.status!='open' AND (o.closed_block IS NULL OR o.closed_block!=o.checked_block
		OR length(o.checked_hash)!=66 OR substr(o.checked_hash,1,2)!='0x'
		OR substr(o.checked_hash,3) GLOB '*[^0-9a-f]*'))
	THEN 255 ELSE o.code END`

type closureRow struct {
	hash, status, checkedHash string
	code                      uint8
	filled, checkedBlock      int64
	closedBlock               sql.NullInt64
	checkedEpoch, fillEpoch   int64
	inputBytes                int64
	raw                       string
	in                        Input
}

type completeOrderCheck struct {
	check
	filled int64
}

// completeCheck reads both views in one hash-selected call. Early checkOrder
// refusals report no remaining quantity, whereas filled(hash) still names the
// complete cumulative fill state. Neither word is inferred from the other.
func (b *Book) completeCheck(ctx context.Context, exchange common.Address, in *Input, head *types.Header) (completeOrderCheck, error) {
	var out completeOrderCheck
	if in == nil || in.Order.Check() != nil || validHeader(head) != nil || b.cfg.Artifact == nil {
		return out, errors.New("closed-order recovery requires a valid original input, header and ABI")
	}
	client, ok := b.cfg.Eth.(hashCaller)
	if !ok {
		return out, errors.New("closed-order recovery requires block-hash contract calls")
	}
	amount, err := storedAmount(in)
	if err != nil {
		return out, err
	}
	checkData, err := b.cfg.Artifact.Call("checkOrder", in.ABI())
	if err != nil {
		return out, err
	}
	fillData, err := b.cfg.Artifact.Call("filled", in.Order.Hash())
	if err != nil {
		return out, err
	}
	type call struct {
		Target       common.Address
		AllowFailure bool
		CallData     []byte
	}
	data, err := mcABI.Pack("aggregate3", []call{{exchange, true, checkData}, {exchange, true, fillData}})
	if err != nil {
		return out, err
	}
	raw, err := client.CallContractAtHash(ctx, ethereum.CallMsg{To: &multicall3, Data: data}, head.Hash())
	if err != nil {
		return out, err
	}
	var results []struct {
		Success    bool
		ReturnData []byte
	}
	if err := mcABI.UnpackIntoInterface(&results, "aggregate3", raw); err != nil {
		return out, err
	}
	if len(results) != 2 || !results[0].Success || !results[1].Success || len(results[0].ReturnData) != 64 || len(results[1].ReturnData) != 32 {
		return out, errors.New("closed-order recovery received incomplete contract evidence")
	}
	code := new(big.Int).SetBytes(results[0].ReturnData[:32])
	remaining := new(big.Int).SetBytes(results[0].ReturnData[32:])
	filled := new(big.Int).SetBytes(results[1].ReturnData)
	if !code.IsUint64() || code.Uint64() > uint64(CodeMalformed) || !remaining.IsInt64() || !filled.IsInt64() || remaining.Int64() > amount || filled.Int64() > amount {
		return out, errors.New("closed-order recovery received out-of-range evidence")
	}
	out = completeOrderCheck{check{uint8(code.Uint64()), remaining.Uint64()}, filled.Int64()}
	switch out.code {
	case CodeOK, CodeNotHeld, CodeNotFunded:
		if out.remaining == 0 || out.filled != amount-int64(out.remaining) {
			return completeOrderCheck{}, errors.New("closed-order remaining and filled evidence conflict")
		}
	case CodeFilled:
		if out.remaining != 0 || out.filled != amount {
			return completeOrderCheck{}, errors.New("closed-order full-fill evidence conflicts")
		}
	default:
		if out.remaining != 0 {
			return completeOrderCheck{}, errors.New("closed-order refusal reports an unexpected remaining amount")
		}
	}
	return out, nil
}

func (b *Book) pendingClosures(ctx context.Context, exchange common.Address, at observation) ([]closureRow, error) {
	// Each indexed branch selects at most one batch. A healthy closed history
	// is not scanned on every poll; structural gaps have their own partial index.
	rows, err := b.cfg.DB.QueryContext(ctx, `WITH
 stale_check AS (SELECT hash FROM nft_orders WHERE exchange=? AND status!='open' AND checked_epoch<? ORDER BY checked_epoch,hash LIMIT ?),
 future_check AS (SELECT hash FROM nft_orders WHERE exchange=? AND status!='open' AND checked_epoch>? ORDER BY checked_epoch,hash LIMIT ?),
 stale_fill AS (SELECT hash FROM nft_orders WHERE exchange=? AND status!='open' AND filled_epoch<? ORDER BY filled_epoch,hash LIMIT ?),
 future_fill AS (SELECT hash FROM nft_orders WHERE exchange=? AND status!='open' AND filled_epoch>? ORDER BY filled_epoch,hash LIMIT ?),
 unproved AS (SELECT hash FROM nft_orders WHERE exchange=? AND status!='open' AND `+closureUnprovedSQL+` ORDER BY hash LIMIT ?)
 SELECT hash,status,code,filled,checked_block,checked_hash,closed_block,checked_epoch,filled_epoch,length(CAST(input AS BLOB))
 FROM nft_orders WHERE hash IN (
 SELECT hash FROM stale_check UNION SELECT hash FROM future_check UNION SELECT hash FROM stale_fill UNION SELECT hash FROM future_fill UNION SELECT hash FROM unproved)
 ORDER BY hash LIMIT ?`,
		exchange.Hex(), at.epoch, closureRecoveryBatch, exchange.Hex(), at.epoch, closureRecoveryBatch,
		exchange.Hex(), at.epoch, closureRecoveryBatch, exchange.Hex(), at.epoch, closureRecoveryBatch,
		exchange.Hex(), closureRecoveryBatch, closureRecoveryBatch)
	if err != nil {
		return nil, err
	}
	var out []closureRow
	var used int64
	for rows.Next() {
		var row closureRow
		if err := rows.Scan(&row.hash, &row.status, &row.code, &row.filled, &row.checkedBlock, &row.checkedHash, &row.closedBlock, &row.checkedEpoch, &row.fillEpoch, &row.inputBytes); err != nil {
			rows.Close()
			return nil, err
		}
		if row.inputBytes <= 0 || row.inputBytes > closureRecoveryInputBytes {
			rows.Close()
			return nil, errors.New("closed-order original input exceeds the recovery byte budget")
		}
		if used+row.inputBytes > closureRecoveryInputBytes {
			break
		}
		used += row.inputBytes
		out = append(out, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Release the reader before using another query on one-connection DBs.
	for i := range out {
		row := &out[i]
		if err := b.cfg.DB.QueryRowContext(ctx, "SELECT input FROM nft_orders WHERE hash=? AND exchange=?", row.hash, exchange.Hex()).Scan(&row.raw); err != nil {
			return nil, err
		}
		if int64(len(row.raw)) != row.inputBytes || json.Unmarshal([]byte(row.raw), &row.in) != nil || row.in.Order.Check() != nil || row.in.Order.Hash().Hex() != row.hash {
			return nil, errors.New("closed-order original input or hash cannot be verified")
		}
	}
	return out, nil
}

func permanentClosure(code uint8) bool {
	return code == CodeFilled || code == CodeCancelled || code == CodeStaleNonce || code == CodeExpired
}

func (b *Book) historicalClosure(ctx context.Context, exchange common.Address, row closureRow, at observation) (*types.Header, completeOrderCheck, bool) {
	var empty completeOrderCheck
	if !permanentClosure(row.code) || row.status != statusFor(row.code) || row.checkedBlock < 0 || uint64(row.checkedBlock) > at.head.Number.Uint64() || !row.closedBlock.Valid || row.closedBlock.Int64 != row.checkedBlock || len(row.checkedHash) != 66 || common.HexToHash(row.checkedHash).Hex() != row.checkedHash {
		return nil, empty, false
	}
	head, err := b.cfg.Eth.HeaderByNumber(ctx, big.NewInt(row.checkedBlock))
	if err != nil || validHeader(head) != nil || head.Number.Int64() != row.checkedBlock || head.Hash().Hex() != row.checkedHash {
		return nil, empty, false
	}
	proof, err := b.completeCheck(ctx, exchange, &row.in, head)
	if err != nil || proof.code != row.code || proof.filled != row.filled {
		return nil, empty, false
	}
	current, err := b.cfg.Eth.HeaderByNumber(ctx, head.Number)
	if err != nil || validHeader(current) != nil || current.Number.Cmp(head.Number) != 0 || current.Hash() != head.Hash() {
		return nil, empty, false
	}
	return head, proof, true
}

// reconcileClosures is bounded and runs after ordinary open-order work. Missing
// old provenance can establish only a new selected-header observation, never a
// fabricated first-closure time. A historical observation is retained only after
// its canonical header, status and independent full-fill counter agree.
func (b *Book) reconcileClosures(ctx context.Context, exchange common.Address, at observation) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := b.pendingClosures(ctx, exchange, at)
	if err != nil {
		return err
	}
	for _, row := range rows {
		head, proof, historical := b.historicalClosure(ctx, exchange, row, at)
		if !historical {
			head = at.head
			proof, err = b.completeCheck(ctx, exchange, &row.in, head)
			if err != nil {
				return fmt.Errorf("checking closed-order provenance: %w", err)
			}
		}
		if err := b.verifyHeader(ctx, at.head); err != nil {
			return err
		}
		tx, err := b.observationTx(ctx, at.epoch)
		if err != nil {
			return err
		}
		err = func() error {
			defer tx.Rollback()
			if err := guardScanHeight(tx, exchange, at.head); err != nil {
				return err
			}
			var current closureRow
			if err := tx.QueryRow(`SELECT input,status,code,filled,checked_block,checked_hash,closed_block,checked_epoch,filled_epoch
				FROM nft_orders WHERE hash=? AND exchange=?`, row.hash, exchange.Hex()).Scan(&current.raw, &current.status, &current.code, &current.filled, &current.checkedBlock, &current.checkedHash, &current.closedBlock, &current.checkedEpoch, &current.fillEpoch); err != nil {
				return err
			}
			if current.raw != row.raw || current.status != row.status || current.code != row.code || current.filled != row.filled || current.checkedBlock != row.checkedBlock || current.checkedHash != row.checkedHash || current.closedBlock != row.closedBlock || current.checkedEpoch != row.checkedEpoch || current.fillEpoch != row.fillEpoch {
				return errStaleObservation
			}
			if historical {
				_, err := tx.Exec("UPDATE nft_orders SET checked_epoch=?,filled_epoch=?,checked_at=? WHERE hash=? AND exchange=?", at.epoch, at.epoch, time.Now().Unix(), row.hash, exchange.Hex())
				if err != nil {
					return err
				}
			} else {
				accepted, err := b.observeFill(tx, &row.in, exchange, proof.check, observation{at.epoch, head}, time.Now().Unix(), &proof.filled)
				if err != nil {
					return err
				}
				if !accepted {
					return errCheckConflict
				}
			}
			return tx.Commit()
		}()
		if err != nil {
			return err
		}
	}
	return nil
}
