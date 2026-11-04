//go:build fullclient_recovery

package orderbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// AssertEmptyMessagePasserGenesis binds this fixture to the original empty
// storage condition, without changing the genesis allocation or any RPC reply.
func (f *FullClientRecoveryCLIFixture) AssertEmptyMessagePasserGenesis(t *testing.T) string {
	t.Helper()
	account := f.r.scenario.genesis.Alloc[params.OptimismL2ToL1MessagePasser]
	if len(account.Code) == 0 || len(account.Storage) != 0 {
		t.Fatal("the original empty MessagePasser allocation changed")
	}
	genesis := f.r.backend.chain.GetBlockByNumber(0)
	if genesis == nil || genesis.Hash() != f.r.scenario.cfg.L2GenesisHash || genesis.Hash() != f.r.scenario.genesis.ToBlock().Hash() {
		t.Fatal("actual client genesis differs from the loader-checked empty-storage fixture")
	}
	root := genesis.WithdrawalsRoot()
	if root == nil || *root != types.EmptyWithdrawalsHash {
		t.Fatal("actual genesis lacks its committed empty storage root")
	}
	state, err := f.r.backend.chain.StateAt(genesis.Root())
	if err != nil || state.GetStorageRoot(params.OptimismL2ToL1MessagePasser) != types.EmptyWithdrawalsHash ||
		!bytes.Equal(state.GetCode(params.OptimismL2ToL1MessagePasser), account.Code) {
		t.Fatal("actual genesis MessagePasser code or empty account storage differs")
	}
	code := sha256.Sum256(account.Code)
	data, err := json.Marshal(map[string]any{"genesis_hash": genesis.Hash().Hex(), "message_passer_code_sha256": hex.EncodeToString(code[:]),
		"message_passer_storage_seeded": false, "actual_genesis_and_account_storage_root_empty": true, "genesis_allocation_rewritten": false,
		"raw_catalyst_payload_rewritten_by_fixture": false, "real_NoM_or_release_genesis_authority": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("N04_EMPTY_STORAGE_CONTEXT " + string(data))
	return genesis.Hash().Hex()
}
