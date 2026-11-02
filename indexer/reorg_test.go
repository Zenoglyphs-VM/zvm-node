package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
)

// fakeChain is an L2 the test can reorganise: real op-geth blocks and
// receipts, served over JSON-RPC so the index reads them through the same
// ethclient it uses against op-reth.
type fakeChain struct {
	mu       sync.Mutex
	blocks   map[uint64]*types.Block
	receipts map[common.Hash][]*types.Receipt
	// answers is what eth_call returns: contract → selector → return data. A
	// call with no answer reverts.
	answers map[common.Address]map[[4]byte][]byte
	// gas is the gas limit each eth_call asked for, in order.
	gas         []uint64
	hashAnswers map[common.Hash]map[common.Address]map[[4]byte][]byte
	callProbe   func(context.Context) error
}

func newFakeChain() *fakeChain {
	return &fakeChain{blocks: map[uint64]*types.Block{}, receipts: map[common.Hash][]*types.Receipt{},
		answers: map[common.Address]map[[4]byte][]byte{}}
}

// answer makes addr return out for the function sig.
func (c *fakeChain) answer(addr common.Address, sig string, out []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answers[addr] == nil {
		c.answers[addr] = map[[4]byte][]byte{}
	}
	var sel [4]byte
	copy(sel[:], crypto.Keccak256([]byte(sig))[:4])
	c.answers[addr][sel] = out
}

// put makes block `number` on top of whatever the chain holds below it.
// `variant` stands for the momentum it was derived from: two variants at one
// height are the orphan and its replacement.
func (c *fakeChain) put(number uint64, variant byte) *types.Block {
	return c.putWithLogs(number, variant, nil)
}

// putWithLogs is put with logs on the block's one transaction's receipt.
func (c *fakeChain) putWithLogs(number uint64, variant byte, logs []*types.Log) *types.Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	parent := common.Hash{}
	if p := c.blocks[number-1]; p != nil {
		parent = p.Hash()
	}
	to := rollup.L1BlockAddr
	tx := types.NewTx(&types.DepositTx{
		SourceHash: common.Hash{byte(number), variant},
		From:       rollup.L1InfoDepositor,
		To:         &to,
		Value:      new(big.Int),
		Gas:        1_000_000,
		Data:       []byte{0x3d, 0xb6, 0xbe, 0x2b, byte(number), variant},
	})
	if logs == nil {
		logs = []*types.Log{}
	}
	for i, l := range logs {
		l.TxHash, l.Index, l.BlockNumber = tx.Hash(), uint(i), number
	}
	rcpt := &types.Receipt{
		Type: types.DepositTxType, Status: types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 43_855, GasUsed: 43_855, TxHash: tx.Hash(), Logs: logs,
	}
	hdr := &types.Header{
		ParentHash: parent, Number: new(big.Int).SetUint64(number),
		Time: 1_000 + number*10 + uint64(variant), GasLimit: 30_000_000,
		Difficulty: common.Big0, BaseFee: big.NewInt(1_000_000),
	}
	blk := types.NewBlock(hdr, &types.Body{Transactions: []*types.Transaction{tx}}, []*types.Receipt{rcpt}, trie.NewStackTrie(nil), types.DefaultBlockConfig)
	rcpt.BlockHash, rcpt.BlockNumber = blk.Hash(), blk.Number()
	for _, l := range logs {
		l.BlockHash = blk.Hash()
	}
	c.blocks[number] = blk
	c.receipts[blk.Hash()] = []*types.Receipt{rcpt}
	return blk
}

func (c *fakeChain) at(number uint64) *types.Block {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocks[number]
}

type fakeEth struct{ c *fakeChain }

func (f *fakeEth) GetBlockByNumber(_ context.Context, n rpc.BlockNumber, _ bool) (json.RawMessage, error) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	number := uint64(n)
	if n < 0 {
		number = 0
		for k := range f.c.blocks {
			number = max(number, k)
		}
	}
	blk := f.c.blocks[number]
	if blk == nil {
		return json.RawMessage("null"), nil
	}
	var out map[string]any
	raw, _ := json.Marshal(blk.Header())
	json.Unmarshal(raw, &out)
	txs := []any{}
	for i, tx := range blk.Transactions() {
		var t map[string]any
		raw, _ := json.Marshal(tx)
		json.Unmarshal(raw, &t)
		t["blockHash"], t["blockNumber"], t["transactionIndex"] = blk.Hash(), fmt.Sprintf("%#x", number), fmt.Sprintf("%#x", i)
		txs = append(txs, t)
	}
	out["transactions"], out["uncles"] = txs, []any{}
	return json.Marshal(out)
}

func (f *fakeEth) GetBlockReceipts(_ context.Context, id rpc.BlockNumberOrHash) ([]*types.Receipt, error) {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	if h, ok := id.Hash(); ok {
		return f.c.receipts[h], nil
	}
	if n, ok := id.Number(); ok && f.c.blocks[uint64(n)] != nil {
		return f.c.receipts[f.c.blocks[uint64(n)].Hash()], nil
	}
	return nil, nil
}

type callArgs struct {
	To    *common.Address `json:"to"`
	Input hexutil.Bytes   `json:"input"`
	Data  hexutil.Bytes   `json:"data"`
	Gas   *hexutil.Uint64 `json:"gas"`
}

func (f *fakeEth) Call(ctx context.Context, args callArgs, block rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	f.c.mu.Lock()
	probe := f.c.callProbe
	f.c.mu.Unlock()
	if probe != nil {
		if err := probe(ctx); err != nil {
			return nil, err
		}
	}
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	gas := uint64(0)
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	f.c.gas = append(f.c.gas, gas)
	data := args.Input
	if len(data) == 0 {
		data = args.Data
	}
	if args.To == nil || len(data) < 4 {
		return nil, errors.New("execution reverted")
	}
	var sel [4]byte
	copy(sel[:], data[:4])
	if block.BlockHash != nil {
		valid := false
		for _, b := range f.c.blocks {
			if b.Hash() == *block.BlockHash {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("unknown selected block hash")
		}
		if out, ok := f.c.hashAnswers[*block.BlockHash][*args.To][sel]; ok {
			return out, nil
		}
	}
	if out, ok := f.c.answers[*args.To][sel]; ok {
		return out, nil
	}
	return nil, errors.New("execution reverted")
}

func newReorgIndex(t *testing.T, chain *fakeChain) (*Indexer, *store.DB) {
	t.Helper()
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", &fakeEth{chain}); err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(srv)
	t.Cleanup(web.Close)
	eth, err := ethclient.Dial(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eth.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "zvm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ix, err := New(db, eth, &rollup.Config{L1StartHeight: 2, L2ChainID: 69}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return ix, db
}

// follow runs Follow against a head the test moves, until the test ends.
func follow(t *testing.T, ix *Indexer, head *atomic.Uint64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ix.Follow(ctx, head.Load)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func storedHash(t *testing.T, db *store.DB, number uint64) string {
	t.Helper()
	var h string
	if err := db.SQL().QueryRow("SELECT hash FROM blocks WHERE number = ?", number).Scan(&h); err != nil {
		return ""
	}
	return h
}

func txIndexed(t *testing.T, db *store.DB, hash common.Hash) bool {
	t.Helper()
	var n int
	db.SQL().QueryRow("SELECT COUNT(*) FROM txs WHERE hash = ?", hash.Hex()).Scan(&n)
	return n > 0
}

// until polls cond for up to ten seconds: Follow sleeps 500 ms between passes.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The live case, block 23,790 on chain 69: the index has already read a block
// when NoM replaces the momentum under it. When the next block arrives it no
// longer links to what the index holds, and the index must drop the orphan
// and read the replacement — not carry on from the height it had reached.
func TestIndexReplacesABlockReorganisedUnderIt(t *testing.T) {
	chain := newFakeChain()
	for n := uint64(1); n <= 3; n++ {
		chain.put(n, 0)
	}
	orphan := chain.at(3)
	ix, db := newReorgIndex(t, chain)
	var rewoundTo atomic.Int64
	rewoundTo.Store(-1)
	ix.AfterRewind(func(n uint64) { rewoundTo.Store(int64(n)) })

	var head atomic.Uint64
	head.Store(3)
	follow(t, ix, &head)
	until(t, "blocks 1-3 indexed", func() bool { return storedHash(t, db, 3) == orphan.Hash().Hex() })

	replacement := chain.put(3, 1)
	next := chain.put(4, 0)
	head.Store(4)
	until(t, "block 4 indexed", func() bool { return storedHash(t, db, 4) == next.Hash().Hex() })

	if got := storedHash(t, db, 3); got != replacement.Hash().Hex() {
		t.Errorf("block 3 is %s, want the replacement %s (the orphan was %s)", got, replacement.Hash().Hex(), orphan.Hash().Hex())
	}
	if txIndexed(t, db, orphan.Transactions()[0].Hash()) {
		t.Errorf("the orphan's transaction %s is still indexed", orphan.Transactions()[0].Hash().Hex())
	}
	if !txIndexed(t, db, replacement.Transactions()[0].Hash()) {
		t.Errorf("the replacement's transaction %s is not indexed", replacement.Transactions()[0].Hash().Hex())
	}
	if got := rewoundTo.Load(); got != 2 {
		t.Errorf("AfterRewind saw %d, want 2: the order book has to rewind with the index", got)
	}
}

// The index as the old code left it: the orphan already buried under blocks
// read from the replacement chain. Nothing new will ever fail to link, so the
// check at start is what has to find it.
func TestIndexRepairsAnOrphanAlreadyBuried(t *testing.T) {
	chain := newFakeChain()
	for n := uint64(1); n <= 3; n++ {
		chain.put(n, 0)
	}
	orphan := chain.at(3)
	ix, db := newReorgIndex(t, chain)
	ctx := context.Background()
	for n := uint64(1); n <= 3; n++ {
		if err := ix.IndexBlock(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	replacement := chain.put(3, 1)
	for n := uint64(4); n <= 5; n++ {
		b := chain.put(n, 0)
		// Written the way the old code wrote them, without the parent check.
		if _, err := db.SQL().Exec(`INSERT INTO blocks (number, hash, parent, timestamp, momentum, txs, user_txs, gas_used, gas_limit, base_fee, size)
			VALUES (?,?,?,?,0,1,0,0,0,'0',0)`, n, b.Hash().Hex(), b.ParentHash().Hex(), b.Time()); err != nil {
			t.Fatal(err)
		}
	}

	var head atomic.Uint64
	head.Store(5)
	follow(t, ix, &head)
	until(t, "the orphan replaced", func() bool { return storedHash(t, db, 3) == replacement.Hash().Hex() })
	until(t, "block 5 re-indexed", func() bool { return storedHash(t, db, 5) == chain.at(5).Hash().Hex() })

	if txIndexed(t, db, orphan.Transactions()[0].Hash()) {
		t.Errorf("the orphan's transaction %s is still indexed", orphan.Transactions()[0].Hash().Hex())
	}
	for n := uint64(1); n <= 5; n++ {
		if got, want := storedHash(t, db, n), chain.at(n).Hash().Hex(); got != want {
			t.Errorf("block %d is %s, want %s", n, got, want)
		}
	}
}

// A chain that never reorganised is left alone: the check must not rewind a
// healthy index on every start.
func TestIndexLeavesAnIntactChainAlone(t *testing.T) {
	chain := newFakeChain()
	for n := uint64(1); n <= 4; n++ {
		chain.put(n, 0)
	}
	ix, db := newReorgIndex(t, chain)
	rewinds := atomic.Int32{}
	ix.AfterRewind(func(uint64) { rewinds.Add(1) })
	var head atomic.Uint64
	head.Store(4)
	follow(t, ix, &head)
	until(t, "blocks 1-4 indexed", func() bool { return storedHash(t, db, 4) == chain.at(4).Hash().Hex() })
	time.Sleep(700 * time.Millisecond) // one more Follow pass
	if n := rewinds.Load(); n != 0 {
		t.Errorf("rewound %d times on a chain that never reorganised", n)
	}
}
