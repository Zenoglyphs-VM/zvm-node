package indexer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

func TestDecodeTransferBatch(t *testing.T) {
	uints, _ := abi.NewType("uint256[]", "", nil)
	args := abi.Arguments{{Type: uints}, {Type: uints}}
	data, err := args.Pack([]*big.Int{big.NewInt(7), big.NewInt(9)}, []*big.Int{big.NewInt(5), big.NewInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	ids, values, ok := decodeTransferBatch(data)
	if !ok || len(ids) != 2 || ids[0].Int64() != 7 || ids[1].Int64() != 9 || values[0].Int64() != 5 || values[1].Int64() != 1 {
		t.Fatalf("ids %v values %v ok %v", ids, values, ok)
	}
	if _, _, ok := decodeTransferBatch(data[:len(data)-32]); ok {
		t.Fatal("a truncated batch decoded")
	}
	mismatched, _ := args.Pack([]*big.Int{big.NewInt(1)}, []*big.Int{})
	if _, _, ok := decodeTransferBatch(mismatched); ok {
		t.Fatal("ids and values of different lengths decoded")
	}
	if batchIdx(0, 0) <= 1000 || batchIdx(3, 1) != batchIdx(3, 0)+1 {
		t.Fatal("batch indexes overlap real log indexes or lose order")
	}
}
