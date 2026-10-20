package indexer

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func pairCreated(factory, token0, token1, pair common.Address) *types.Log {
	return &types.Log{
		Address: factory,
		Topics:  []common.Hash{topicPairCreated, common.BytesToHash(token0[:]), common.BytesToHash(token1[:])},
		Data:    append(common.LeftPadBytes(pair[:], 32), common.LeftPadBytes([]byte{1}, 32)...),
	}
}

// A pair's address is known before it exists, and the dex_pairs row is
// first-claim-wins, so PairCreated counts only from the factory the pair says
// deployed it. Otherwise anyone could announce a pool before it was created,
// under a factory and tokens of their own, and the real pool would never be
// priced.
func TestPairCreatedCountsOnlyFromThePairsOwnFactory(t *testing.T) {
	var (
		factory = common.HexToAddress("0x00000000000000000000000000000000000000f1")
		forger  = common.HexToAddress("0x00000000000000000000000000000000000000f2")
		tokenA  = common.HexToAddress("0x00000000000000000000000000000000000000a1")
		tokenB  = common.HexToAddress("0x00000000000000000000000000000000000000b1")
		wrong   = common.HexToAddress("0x00000000000000000000000000000000000000c1")
		pair    = common.HexToAddress("0x000000000000000000000000000000000000beef")
	)
	chain := newFakeChain()
	chain.answer(pair, "factory()", common.LeftPadBytes(factory[:], 32))
	// The forger front-runs the pool with its own factory and tokens, then
	// the real factory announces it.
	chain.putWithLogs(1, 0, []*types.Log{pairCreated(forger, wrong, tokenB, pair)})
	chain.putWithLogs(2, 0, []*types.Log{pairCreated(factory, tokenA, tokenB, pair)})
	ix, db := newReorgIndex(t, chain)
	ctx := context.Background()
	for n := uint64(1); n <= 2; n++ {
		if err := ix.IndexBlock(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	var gotFactory, gotToken0 string
	if err := db.SQL().QueryRow("SELECT factory, token0 FROM dex_pairs WHERE pair = ?", pair.Hex()).Scan(&gotFactory, &gotToken0); err != nil {
		t.Fatalf("the real pool is not indexed: %v", err)
	}
	if gotFactory != factory.Hex() || gotToken0 != tokenA.Hex() {
		t.Errorf("the pool is recorded under factory %s with token0 %s; want %s and %s", gotFactory, gotToken0, factory.Hex(), tokenA.Hex())
	}
}

// A token's name() is code its deployer wrote, run inside the indexing
// transaction: it gets a gas limit, and what it returns is clipped to a
// name's length and to valid text before it is stored.
func TestTokenMetadataIsBounded(t *testing.T) {
	token := common.HexToAddress("0x000000000000000000000000000000000000700c")
	chain := newFakeChain()
	abiString := func(s string) []byte {
		out := append(common.LeftPadBytes([]byte{0x20}, 32), common.LeftPadBytes(big.NewInt(int64(len(s))).Bytes(), 32)...)
		out = append(out, []byte(s)...)
		return append(out, make([]byte, (32-len(s)%32)%32)...)
	}
	chain.answer(token, "name()", abiString(strings.Repeat("é", 1<<16)+"\xff"))
	chain.answer(token, "symbol()", abiString("OK"))
	// decimals() is a uint8; 2^63+5 read as an int64 used to be negative.
	huge := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(5))
	chain.answer(token, "decimals()", common.LeftPadBytes(huge.Bytes(), 32))
	from, to := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	chain.putWithLogs(1, 0, []*types.Log{{
		Address: token,
		Topics:  []common.Hash{topicTransfer, common.BytesToHash(from[:]), common.BytesToHash(to[:])},
		Data:    common.LeftPadBytes([]byte{5}, 32),
	}})
	ix, db := newReorgIndex(t, chain)
	if err := ix.IndexBlock(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var name, symbol string
	var decimals int
	if err := db.SQL().QueryRow("SELECT name, symbol, decimals FROM tokens WHERE address = ?", token.Hex()).Scan(&name, &symbol, &decimals); err != nil {
		t.Fatal(err)
	}
	if len(name) > maxTextBytes || !utf8.ValidString(name) || !strings.HasPrefix(name, "éé") {
		t.Errorf("a 128 KB name was stored as %d bytes (valid UTF-8: %v)", len(name), utf8.ValidString(name))
	}
	if symbol != "OK" {
		t.Errorf("symbol = %q", symbol)
	}
	if decimals != 0 {
		t.Errorf("decimals() answered 2^63+5 and was stored as %d; want 0", decimals)
	}
	for i, g := range chain.gas {
		if g == 0 || g > metadataGas {
			t.Errorf("eth_call %d asked for %d gas; want a limit of at most %d", i, g, metadataGas)
		}
	}
}
