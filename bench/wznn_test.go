package bench

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/simulated"
)

// The wrapper this chain deploys used to be Uniswap's WETH9, unmodified, which
// meant the token standing in for ZNN introduced itself as "Wrapped Ether" on a
// chain with no ether on it. Every explorer, wallet and pool label inherited
// that name. The fix is in the contract rather than in a lookup table, so this
// checks the deployed bytecode itself rather than any label the node applies.
func TestWZNNNamesItselfAfterZNN(t *testing.T) {
	arts, err := LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	art := arts.MustGet("WZNN")

	key, _ := crypto.GenerateKey()
	from := crypto.PubkeyToAddress(key.PublicKey)
	rich := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	sim := simulated.NewBackend(types.GenesisAlloc{from: {Balance: rich}})
	defer sim.Close()
	client := sim.Client()
	chainID := big.NewInt(1337)

	init, err := art.Deploy()
	if err != nil {
		t.Fatal(err)
	}
	tx := types.MustSignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(100e9),
		Gas: 8_000_000, Value: new(big.Int), Data: init,
	})
	if err := client.SendTransaction(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	sim.Commit()
	receipt, err := client.TransactionReceipt(context.Background(), tx.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatal("deploying WZNN reverted")
	}
	wznn := receipt.ContractAddress

	read := func(method string) []any {
		t.Helper()
		data, err := art.Call(method)
		if err != nil {
			t.Fatal(err)
		}
		out, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &wznn, Data: data}, nil)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		values, err := art.ABI.Unpack(method, out)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return values
	}

	if got := read("name")[0].(string); got != "Wrapped ZNN" {
		t.Errorf("name() = %q, want %q", got, "Wrapped ZNN")
	}
	if got := read("symbol")[0].(string); got != "WZNN" {
		t.Errorf("symbol() = %q, want %q", got, "WZNN")
	}
	if got := read("decimals")[0].(uint8); got != 18 {
		t.Errorf("decimals() = %d, want 18", got)
	}

	// The router only ever reaches this contract through deposit, withdraw and
	// transfer, so a rename that broke any of them would break every pool.
	deposit, err := art.Call("deposit")
	if err != nil {
		t.Fatal(err)
	}
	wad := new(big.Int).Mul(big.NewInt(3), big.NewInt(1e18))
	depositTx := types.MustSignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: 1, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(100e9),
		Gas: 200_000, To: &wznn, Value: wad, Data: deposit,
	})
	if err := client.SendTransaction(context.Background(), depositTx); err != nil {
		t.Fatal(err)
	}
	sim.Commit()
	depositReceipt, err := client.TransactionReceipt(context.Background(), depositTx.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if depositReceipt.Status != types.ReceiptStatusSuccessful {
		t.Fatal("deposit reverted")
	}

	balanceData, err := art.Call("balanceOf", from)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &wznn, Data: balanceData}, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := art.ABI.Unpack("balanceOf", out)
	if err != nil {
		t.Fatal(err)
	}
	if got := values[0].(*big.Int); got.Cmp(wad) != 0 {
		t.Errorf("balanceOf after deposit = %s, want %s", got, wad)
	}
	if got := read("totalSupply")[0].(*big.Int); got.Cmp(wad) != 0 {
		t.Errorf("totalSupply = %s, want %s", got, wad)
	}
}
