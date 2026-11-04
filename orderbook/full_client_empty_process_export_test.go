//go:build fullclient_recovery

package orderbook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// All three service/CLI children start with the original empty account storage.
// The parent opens no execution client before those exact children have joined.
func NewFullClientEmptyRecoveryProcessFixture(t *testing.T) *FullClientRecoveryCLIFixture {
	t.Helper()
	return newFullClientRecoveryCLIFixture(t, false, false)
}

func OwnedFullClientEmptyCLIArgs(t *testing.T, directory string, args []string, phase string) ([]string, func()) {
	t.Helper()
	return ownedFullClientCLIArgsProfile(t, directory, args, phase, false, false)
}

func OwnedFullClientEmptyKernelKillCLIArgs(t *testing.T, directory string, args []string, phase string) ([]string, func()) {
	t.Helper()
	return ownedFullClientCLIArgsProfile(t, directory, args, phase, true, false)
}

func (f *FullClientRecoveryCLIFixture) emptyOwnedClientContext(t *testing.T) string {
	t.Helper()
	genesis := f.r.scenario.genesis
	account := genesis.Alloc[params.OptimismL2ToL1MessagePasser]
	if len(account.Code) == 0 || len(account.Storage) != 0 || genesis.ToBlock().Hash() != f.r.scenario.cfg.L2GenesisHash {
		t.Fatal("owned empty-client input context differs")
	}
	code := sha256.Sum256(account.Code)
	raw, err := json.Marshal(map[string]any{"genesis_hash": genesis.ToBlock().Hash().Hex(), "message_passer_code_sha256": hex.EncodeToString(code[:]),
		"message_passer_storage_seeded": false, "actual_genesis_header_empty_storage_root": true, "actual_running_head_account_storage_root_empty": true,
		"loaded_original_genesis_allocation_hash_bound": true, "genesis_allocation_rewritten": false,
		"raw_catalyst_payload_rewritten_by_fixture": false, "real_NoM_or_release_genesis_authority": false})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Bind the original allocation to the running genesis header, then inspect the
// actual running head state. Reopening a pruned path client does not require a
// separately available historical genesis state database for this assertion.
func (f *FullClientRecoveryCLIFixture) AssertEmptyKernelClientContext(t *testing.T) string {
	t.Helper()
	context := f.emptyOwnedClientContext(t)
	chain := f.r.backend.chain
	genesis, head := chain.GetBlockByNumber(0), chain.CurrentBlock()
	if genesis == nil || head == nil || genesis.Hash() != f.r.scenario.cfg.L2GenesisHash || genesis.Hash() != f.r.scenario.genesis.ToBlock().Hash() {
		t.Fatal("owned empty client lacks its exact original genesis header")
	}
	root := genesis.WithdrawalsRoot()
	if root == nil || *root != types.EmptyWithdrawalsHash {
		t.Fatal("owned empty genesis header commitment differs")
	}
	state, err := chain.StateAt(head.Root)
	account := f.r.scenario.genesis.Alloc[params.OptimismL2ToL1MessagePasser]
	if err != nil || state.GetStorageRoot(params.OptimismL2ToL1MessagePasser) != types.EmptyWithdrawalsHash || !bytes.Equal(state.GetCode(params.OptimismL2ToL1MessagePasser), account.Code) {
		t.Fatal("running owned MessagePasser code or empty storage differs")
	}
	t.Log("N04_EMPTY_KERNEL_CONTEXT " + context)
	return genesis.Hash().Hex()
}

// Emit only after checking the running child's actual genesis and account state.
// fmt is required: the deliberately killed child never flushes a test-log buffer.
func writeOwnedEmptyFullClientContext(t *testing.T, f *FullClientRecoveryCLIFixture) {
	t.Helper()
	f.AssertEmptyKernelClientContext(t)
	fmt.Println("N04_EMPTY_KERNEL_CHILD_CONTEXT " + f.emptyOwnedClientContext(t))
}

// Called only after the exact child joined; the original lifecycle and kernel
// validators remain separate and mandatory at every requested boundary.
func (f *FullClientRecoveryCLIFixture) AssertOwnedEmptyFullClientContext(t *testing.T, output string) {
	t.Helper()
	want := "N04_EMPTY_KERNEL_CHILD_CONTEXT " + f.emptyOwnedClientContext(t)
	count, all := 0, 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "N04_EMPTY_KERNEL_CHILD_CONTEXT ") {
			all++
		}
		if line == want {
			count++
		}
	}
	if count != 1 || all != 1 {
		t.Fatal("owned child lacks its exact verified empty-storage context")
	}
	t.Log("N04_EMPTY_KERNEL_CHILD_CONTEXT_VERIFIED " + f.emptyOwnedClientContext(t))
}
