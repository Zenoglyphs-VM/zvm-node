// Package node runs the derivation loop: read momentums from NoM, plan each
// one's L2 block, and have op-geth build exactly that block.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// Deriver follows NoM and drives op-geth.
type Deriver struct {
	Cfg    *rollup.Config
	L1     *znn.Client
	Engine *l2.Engine
	Eth    *ethclient.Client
	DB     *store.DB
	Log    *slog.Logger

	// Confirmations is how far behind the NoM frontier to derive. NoM does not
	// reorg in normal operation, so 0 (derive at the tip) is the default.
	Confirmations uint64

	// OnDerived, if set, is called after every block is committed.
	OnDerived func(*store.Derived)
	// OnRewind, if set, is called when derivation rewinds to an L2 block,
	// so downstream indexes can drop what came after.
	OnRewind func(l2Number uint64)

	// StallAfter is how long the head may stand still, while NoM holds
	// momentums this node has not derived, before the node calls itself
	// stalled. Zero means DefaultStallAfter. A node level with the frontier is
	// never stalled: there is nothing to derive, which is an idle chain's
	// normal state rather than a fault.
	StallAfter time.Duration

	chainCfg *params.ChainConfig

	mu   sync.RWMutex
	head store.Head

	l1Frontier atomic.Uint64
	// lastProgress is when the head last moved, as unix nanoseconds.
	lastProgress atomic.Int64
}

// DefaultStallAfter is nine momentums at this devnet's 10 s cadence: long
// enough that an ordinary gap between momentums never trips it, short enough
// that a reader notices before the chain is far behind.
const DefaultStallAfter = 90 * time.Second

// errRestart abandons the current derivation step because the ground moved: the
// store was rewound to match an engine that had lost blocks. It is a recovery,
// not a failure, so the loop retries at once and says nothing alarming.
var errRestart = errors.New("derivation restarted after reconciling with the engine")

// Status is a snapshot for the API.
type Status struct {
	L1Frontier    uint64      `json:"l1Frontier"`
	DerivedHeight uint64      `json:"derivedMomentum"`
	DerivedHash   znn.Hash    `json:"derivedMomentumHash"`
	L2Number      uint64      `json:"l2Number"`
	L2Hash        common.Hash `json:"l2Hash"`
	LagMomentums  uint64      `json:"lagMomentums"`
	FinalizedL2   uint64      `json:"finalizedL2"`
	// Stalled reports that this node is behind and has stopped catching up.
	// Derivation retries forever rather than exiting (docs/HALT.md), so
	// without this a halted node keeps answering confidently from a chain
	// that stopped.
	Stalled bool `json:"stalled"`
	// SinceProgressMS is how long ago the head last moved.
	SinceProgressMS int64 `json:"sinceProgressMs"`
}

func (d *Deriver) Status() Status {
	d.mu.RLock()
	h := d.head
	d.mu.RUnlock()
	s := Status{L1Frontier: d.l1Frontier.Load(), DerivedHeight: h.Height, DerivedHash: h.Hash, L2Number: h.L2Number, L2Hash: h.L2Hash}
	if s.L1Frontier > h.Height {
		s.LagMomentums = s.L1Frontier - h.Height
	}
	if h.Height > d.Cfg.FinalityDepth {
		s.FinalizedL2 = d.Cfg.L2BlockForMomentum(h.Height - d.Cfg.FinalityDepth)
	}
	since := time.Since(d.progressedAt())
	s.SinceProgressMS = since.Milliseconds()
	s.Stalled = s.LagMomentums > 0 && since > d.stallAfter()
	return s
}

func (d *Deriver) stallAfter() time.Duration {
	if d.StallAfter > 0 {
		return d.StallAfter
	}
	return DefaultStallAfter
}

// progressedAt is when the head last moved. Before anything has been derived it
// is the moment the deriver started, so a node that can never reach its engine
// still reports itself stalled rather than newly born forever.
func (d *Deriver) progressedAt() time.Time {
	if ns := d.lastProgress.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	// Nothing has moved yet. Start the clock at the first observation rather
	// than leaving it at zero, which would read as an infinitely long stall.
	d.lastProgress.CompareAndSwap(0, time.Now().UnixNano())
	return time.Unix(0, d.lastProgress.Load())
}

func (d *Deriver) markProgress() { d.lastProgress.Store(time.Now().UnixNano()) }

// Init checks the engine against the rollup config and reconciles the store
// with the engine's head, so a restart resumes where the chain actually is.
func (d *Deriver) Init(ctx context.Context) error {
	d.chainCfg = l2.ChainConfig(d.Cfg)
	genesis, err := d.Eth.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("reading the engine's genesis: %w", err)
	}
	if genesis.Hash() != d.Cfg.L2GenesisHash {
		return fmt.Errorf("the engine's genesis is %s, rollup config says %s", genesis.Hash(), d.Cfg.L2GenesisHash)
	}
	return d.reconcileWithEngine(ctx, genesis)
}

// reconcileWithEngine rewinds this node's store to the newest block the engine
// still has, and points the engine at it. Init does this at startup; the derive
// loop does it again if the engine ever answers that it does not know the head
// we are building on.
//
// That is not hypothetical on op-reth. It keeps recent canonical blocks in
// memory and flushes them behind `--engine.persistence-threshold`, so a
// container restart can come back a few blocks short — measured: the engine
// returned at block 7,214 with this node's store at 7,217. On a based rollup
// there is nobody to sync those blocks from: they exist only here, derived from
// NoM. So the engine answers every forkchoice with SYNCING, forever, and
// without this the node retries forever against a head that will never arrive.
func (d *Deriver) reconcileWithEngine(ctx context.Context, genesis *types.Header) error {
	if genesis == nil {
		var err error
		genesis, err = d.Eth.HeaderByNumber(ctx, big.NewInt(0))
		if err != nil {
			return fmt.Errorf("reading the engine's genesis: %w", err)
		}
	}
	gethHead, err := d.Eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}

	start := d.Cfg.L1StartHeight
	parentMomentum, err := d.momentumHash(ctx, start-1)
	if err != nil {
		return err
	}
	genesisHead := store.Head{Height: start - 1, Hash: parentMomentum, L2Number: 0, L2Hash: genesis.Hash()}

	// Any block this drops from the store goes from the index too, as in
	// reorg. This path used to rewind the store alone: the blocks it re-derives
	// usually come back identical, but not if NoM moved while the engine was
	// behind, and then the index would keep the old ones.
	before, err := d.DB.Head()
	if err != nil {
		return err
	}
	rewound := func(l2Number uint64) {
		if d.OnRewind != nil && before != nil && before.L2Number > l2Number {
			d.OnRewind(l2Number)
		}
	}

	if gethHead.Number.Uint64() == 0 {
		if err := d.DB.RewindTo(start - 1); err != nil {
			return err
		}
		d.setHead(genesisHead)
		rewound(0)
		d.Log.Info("starting from genesis", "l1Start", start, "l2Genesis", genesis.Hash())
		return nil
	}

	// The engine has blocks. Find the newest one the store also knows.
	number := gethHead.Number.Uint64()
	for {
		height := d.Cfg.MomentumForL2Block(number)
		row, err := d.DB.MomentumHead(height)
		if err != nil {
			return err
		}
		if row != nil {
			hdr, err := d.Eth.HeaderByNumber(ctx, new(big.Int).SetUint64(number))
			if err == nil && hdr.Hash() == row.L2Hash {
				if err := d.DB.RewindTo(height); err != nil {
					return err
				}
				if number != gethHead.Number.Uint64() {
					if err := d.setGethHead(ctx, row.L2Hash); err != nil {
						return err
					}
				}
				d.setHead(*row)
				rewound(row.L2Number)
				d.Log.Info("resuming", "momentum", row.Height, "l2", row.L2Number, "l2Hash", row.L2Hash)
				return nil
			}
		}
		if number == 0 {
			break
		}
		number--
	}
	// Nothing in common: rebuild from genesis.
	if err := d.setGethHead(ctx, genesis.Hash()); err != nil {
		return err
	}
	if err := d.DB.RewindTo(start - 1); err != nil {
		return err
	}
	d.setHead(genesisHead)
	rewound(0)
	d.Log.Warn("store and the engine share no block; re-deriving from genesis")
	return nil
}

func (d *Deriver) momentumHash(ctx context.Context, height uint64) (znn.Hash, error) {
	ms, err := d.L1.DetailedMomentums(ctx, height, 1)
	if err != nil {
		return znn.Hash{}, err
	}
	if len(ms) == 0 || ms[0].Momentum == nil {
		return znn.Hash{}, fmt.Errorf("momentum %d not available from the L1 node", height)
	}
	return ms[0].Momentum.Hash, nil
}

func (d *Deriver) setHead(h store.Head) {
	d.mu.Lock()
	d.head = h
	d.mu.Unlock()
	d.markProgress()
}

func (d *Deriver) Head() store.Head {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.head
}

func (d *Deriver) setGethHead(ctx context.Context, hash common.Hash) error {
	res, err := d.Engine.ForkchoiceUpdated(ctx, engine.ForkchoiceStateV1{HeadBlockHash: hash, SafeBlockHash: hash}, nil)
	if err != nil {
		return fmt.Errorf("rewinding op-geth to %s: %w", hash, err)
	}
	if res.PayloadStatus.Status != engine.VALID {
		return fmt.Errorf("rewinding op-geth to %s: status %s", hash, res.PayloadStatus.Status)
	}
	return nil
}

// Run derives until ctx ends.
func (d *Deriver) Run(ctx context.Context) error {
	backoff := time.Second
	d.markProgress()
	// The retry loop would otherwise log the same failure every backoff, and
	// the first one is the interesting one (docs/HALT.md).
	var reported string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		progressed, err := d.step(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, errRestart) {
				reported, backoff = "", time.Second
				continue
			}
			if msg := err.Error(); msg != reported {
				d.Log.Error("derivation stopped; retrying until it clears", "err", err)
				reported = msg
			} else {
				d.Log.Debug("derivation still stopped", "err", err, "backoff", backoff)
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if reported != "" {
			d.Log.Info("derivation resumed")
			reported = ""
		}
		backoff = time.Second
		if !progressed {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (d *Deriver) step(ctx context.Context) (bool, error) {
	frontier, err := d.L1.FrontierMomentum(ctx)
	if err != nil {
		return false, err
	}
	d.l1Frontier.Store(frontier.Height)
	head := d.Head()
	if frontier.Height < d.Confirmations || head.Height+1 > frontier.Height-d.Confirmations {
		// Nothing new to derive — but NoM may have replaced the very momentum
		// this node's head came from. Waiting for a later momentum to name a
		// different parent is how that used to surface, and on a quiet devnet
		// that took 8 minutes 40 seconds for block 23,790, every one of them
		// spent serving an orphan as this chain's head. The frontier is already
		// in hand, so comparing it costs nothing.
		if frontier.Height == head.Height && frontier.Hash != head.Hash && head.Height >= d.Cfg.L1StartHeight {
			return true, d.reorg(ctx)
		}
		return false, nil
	}
	count := min(frontier.Height-d.Confirmations-head.Height, 64)
	ms, err := d.L1.DetailedMomentums(ctx, head.Height+1, count)
	if err != nil {
		return false, err
	}
	for _, dm := range ms {
		if dm.Momentum.PreviousHash != d.Head().Hash {
			return true, d.reorg(ctx)
		}
		if err := d.deriveOne(ctx, dm); err != nil {
			return true, fmt.Errorf("momentum %d: %w", dm.Momentum.Height, err)
		}
	}
	return len(ms) > 0, nil
}

// reorg rewinds to the newest momentum the store and NoM still agree on.
func (d *Deriver) reorg(ctx context.Context) error {
	head := d.Head()
	for h := head.Height; h >= d.Cfg.L1StartHeight; h-- {
		row, err := d.DB.MomentumHead(h)
		if err != nil {
			return err
		}
		hash, err := d.momentumHash(ctx, h)
		if err != nil {
			return err
		}
		if row != nil && row.Hash == hash {
			d.Log.Warn("NoM reorganised; rewinding", "from", head.Height, "to", h)
			if err := d.setGethHead(ctx, row.L2Hash); err != nil {
				return err
			}
			if err := d.DB.RewindTo(h); err != nil {
				return err
			}
			// The head moves first. Told first, the index rewound while this
			// node still reported the orphan as its head, and in that gap the
			// index read the orphan straight back from op-reth — which still
			// serves it by number — and kept it (block 23,790 on chain 69).
			d.setHead(*row)
			if d.OnRewind != nil {
				d.OnRewind(row.L2Number)
			}
			return nil
		}
	}
	return errors.New("NoM reorganised below the rollup's start height")
}

func (d *Deriver) finalizedHash(height uint64) common.Hash {
	if height <= d.Cfg.FinalityDepth {
		return d.Cfg.L2GenesisHash
	}
	row, err := d.DB.MomentumHead(height - d.Cfg.FinalityDepth)
	if err != nil || row == nil {
		return d.Cfg.L2GenesisHash
	}
	return row.L2Hash
}

func (d *Deriver) deriveOne(ctx context.Context, dm *znn.DetailedMomentum) error {
	started := time.Now()
	head := d.Head()
	plan, err := derive.BuildPlan(d.Cfg, dm, d.DB)
	if err != nil {
		return err
	}
	if err := d.prevalidate(ctx, plan, head); err != nil {
		return err
	}

	finalized := d.finalizedHash(dm.Momentum.Height)
	beaconRoot := common.Hash(dm.Momentum.Hash)
	attempts := 0
	for {
		attempts++
		attrs, err := plan.Attributes()
		if err != nil {
			return err
		}
		res, err := d.Engine.ForkchoiceUpdated(ctx, engine.ForkchoiceStateV1{
			HeadBlockHash: head.L2Hash, SafeBlockHash: head.L2Hash, FinalizedBlockHash: finalized,
		}, attrs)
		if err != nil {
			dropped, rerr := d.dropRefused(plan, err)
			if rerr != nil {
				return rerr
			}
			if dropped {
				continue
			}
			return fmt.Errorf("forkchoiceUpdated with attributes: %w", err)
		}
		if res.PayloadStatus.Status == engine.SYNCING {
			// The engine does not have the head we are building on. Nobody can
			// send it: on a based rollup these blocks exist only here. Rewind
			// to what the engine does have and derive forward again.
			d.Log.Warn("the engine does not have this node's head; rewinding to what it has",
				"head", head.L2Hash, "momentum", dm.Momentum.Height)
			if err := d.reconcileWithEngine(ctx, nil); err != nil {
				return fmt.Errorf("reconciling with an engine that is behind: %w", err)
			}
			return errRestart
		}
		if res.PayloadStatus.Status != engine.VALID || res.PayloadID == nil {
			return fmt.Errorf("forkchoiceUpdated: status %s, payload id %v", res.PayloadStatus.Status, res.PayloadID)
		}
		env, err := d.Engine.GetPayload(ctx, *res.PayloadID)
		if err != nil {
			return fmt.Errorf("getPayload: %w", err)
		}
		payload := env.ExecutionPayload
		// The engine has the last word (D12), and it may take it without
		// saying anything: a client that skips a transaction it cannot apply
		// returns a payload shorter than the plan. Whatever came back without
		// is what it refused.
		if rebuilt, rerr := d.reconcile(plan, payload.Transactions); rerr != nil {
			return rerr
		} else if rebuilt {
			continue
		}
		status, err := d.Engine.NewPayload(ctx, payload, beaconRoot)
		if err != nil {
			return fmt.Errorf("newPayload: %w", err)
		}
		if status.Status != engine.VALID {
			reason := ""
			if status.ValidationError != nil {
				reason = *status.ValidationError
			}
			return fmt.Errorf("newPayload: status %s %s", status.Status, reason)
		}
		fc, err := d.Engine.ForkchoiceUpdated(ctx, engine.ForkchoiceStateV1{
			HeadBlockHash: payload.BlockHash, SafeBlockHash: payload.BlockHash, FinalizedBlockHash: finalized,
		}, nil)
		if err != nil {
			return fmt.Errorf("forkchoiceUpdated to the new head: %w", err)
		}
		if fc.PayloadStatus.Status != engine.VALID {
			return fmt.Errorf("forkchoiceUpdated to the new head: status %s", fc.PayloadStatus.Status)
		}
		if len(payload.Transactions) != len(plan.Txs) {
			return fmt.Errorf("the engine built %d transactions for a plan of %d", len(payload.Transactions), len(plan.Txs))
		}

		rec := &store.Derived{
			Momentum:  dm.Momentum,
			NomBlocks: len(dm.Blocks),
			L2Number:  payload.Number,
			L2Hash:    payload.BlockHash,
			GasUsed:   payload.GasUsed,
			BuildMS:   time.Since(started).Milliseconds(),
			Attempts:  attempts,
			Plan:      plan,
		}
		if err := d.DB.RecordDerived(rec, d.Cfg.ChunkTTL); err != nil {
			return fmt.Errorf("recording: %w", err)
		}
		d.setHead(store.Head{Height: dm.Momentum.Height, Hash: dm.Momentum.Hash, L2Number: payload.Number, L2Hash: payload.BlockHash})
		if len(plan.Messages) > 0 || attempts > 1 {
			d.Log.Info("derived", "momentum", dm.Momentum.Height, "l2", payload.Number, "txs", len(plan.Txs),
				"messages", len(plan.Messages), "gas", payload.GasUsed, "attempts", attempts, "ms", rec.BuildMS)
		}
		if d.OnDerived != nil {
			d.OnDerived(rec)
		}
		return nil
	}
}

// dropRefused reads an engine's refusal and takes the transaction it named out
// of the plan. It reports whether it changed anything; an error means the
// engine refused something this node may not drop, and the caller stops.
//
// The engine names a transaction in its own words, and those words are the
// client's, not the protocol's (D2, D12): op-geth says "failed to force-include
// tx: 0x…", another client says something else. So every hash the error
// mentions is tried against the plan, and one that is not in the plan is
// ignored — the payload comparison in reconcile is the backstop that needs no
// wording at all.
func (d *Deriver) dropRefused(plan *derive.Plan, err error) (bool, error) {
	hashes, reason := l2.RefusedTransactions(err)
	for _, h := range hashes {
		if !plan.Has(h) {
			continue
		}
		if derr := plan.Drop(h, reason); derr != nil {
			return false, derr
		}
		d.Log.Debug("engine refused a transaction; rebuilding without it", "tx", h, "reason", reason)
		return true, nil
	}
	return false, nil
}

// reconcile compares the payload the engine built against the plan it was
// given. A transaction that is missing was refused silently; one that is
// present but was never planned means the engine built a block this node did
// not derive, and no node may accept that.
func (d *Deriver) reconcile(plan *derive.Plan, payload [][]byte) (bool, error) {
	extra, err := plan.Unplanned(payload)
	if err != nil {
		return false, err
	}
	if len(extra) > 0 {
		return false, fmt.Errorf("the engine added %d transactions this node did not derive, the first %s", len(extra), extra[0])
	}
	missing, mandatory, err := plan.Missing(payload)
	if err != nil {
		return false, err
	}
	if len(missing) == 0 {
		return false, nil
	}
	if mandatory {
		return false, fmt.Errorf("the engine left out %d transactions, one of them a deposit, a payout or a native call (the first is %s): this node cannot build the chain its peers will",
			len(missing), missing[0])
	}
	for _, h := range missing {
		if derr := plan.Drop(h, "the engine built the block without it"); derr != nil {
			return false, derr
		}
	}
	d.Log.Debug("engine built the block without some transactions; rebuilding to match", "txs", len(missing))
	return true, nil
}

// prevalidate drops relayed transactions that parent state already rules out
// — a nonce out of sequence, a fee cap under the block's base fee — so the
// engine is not asked to rebuild the block once per bad transaction. It reads
// only the parent block, so every node reaches the same verdicts.
func (d *Deriver) prevalidate(ctx context.Context, plan *derive.Plan, head store.Head) error {
	var batch []*derive.PlannedTx
	for _, t := range plan.Txs {
		if t.Lane == derive.LaneBatch {
			if t.Tx.Type() == types.SetCodeTxType {
				// Authorizations move other accounts' nonces; leave the whole
				// block to the engine.
				return nil
			}
			batch = append(batch, t)
		}
	}
	if len(batch) == 0 {
		return nil
	}
	parent, err := d.Eth.HeaderByHash(ctx, head.L2Hash)
	if err != nil {
		return fmt.Errorf("reading parent header: %w", err)
	}
	baseFee := eip1559.CalcBaseFee(d.chainCfg, parent, plan.Momentum.Timestamp)
	signer := types.LatestSignerForChainID(d.Cfg.L2ChainIDBig())

	senders := make([]common.Address, len(batch))
	unique := map[common.Address]bool{}
	var elems []rpc.BatchElem
	nonceHex := map[common.Address]*string{}
	for i, t := range batch {
		from, err := types.Sender(signer, t.Tx)
		if err != nil {
			return err // BuildPlan already checked signatures
		}
		senders[i] = from
		if !unique[from] {
			unique[from] = true
			s := new(string)
			nonceHex[from] = s
			elems = append(elems, rpc.BatchElem{
				Method: "eth_getTransactionCount",
				Args:   []any{from, head.L2Hash.Hex()},
				Result: s,
			})
		}
	}
	for i := 0; i < len(elems); i += 500 {
		chunk := elems[i:min(i+500, len(elems))]
		if err := d.Eth.Client().BatchCallContext(ctx, chunk); err != nil {
			return fmt.Errorf("reading nonces: %w", err)
		}
		for _, e := range chunk {
			if e.Error != nil {
				return fmt.Errorf("reading nonce: %w", e.Error)
			}
		}
	}
	next := map[common.Address]uint64{}
	for from, s := range nonceHex {
		n, ok := new(big.Int).SetString(trim0x(*s), 16)
		if !ok {
			return fmt.Errorf("bad nonce %q for %s", *s, from)
		}
		next[from] = n.Uint64()
	}
	for i, t := range batch {
		from := senders[i]
		switch {
		case t.Tx.Nonce() < next[from]:
			if err := plan.Drop(t.Tx.Hash(), fmt.Sprintf("nonce too low: tx %d, account %d", t.Tx.Nonce(), next[from])); err != nil {
				return err
			}
		case t.Tx.Nonce() > next[from]:
			if err := plan.Drop(t.Tx.Hash(), fmt.Sprintf("nonce too high: tx %d, account %d", t.Tx.Nonce(), next[from])); err != nil {
				return err
			}
		case t.Tx.GasFeeCap().Cmp(baseFee) < 0:
			if err := plan.Drop(t.Tx.Hash(), fmt.Sprintf("max fee %s below base fee %s", t.Tx.GasFeeCap(), baseFee)); err != nil {
				return err
			}
		default:
			next[from]++
		}
	}
	return nil
}

func trim0x(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}
