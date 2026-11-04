package indexer_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/indexer/nft"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func retainedApproval(owner, spender common.Address, id common.Hash) *types.Log {
	return &types.Log{Address: nftCollection, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("Approval(address,address,uint256)")), common.BytesToHash(owner[:]), common.BytesToHash(spender[:]), id,
	}}
}

func retainedOperator(owner, operator common.Address, approved bool) *types.Log {
	data := make([]byte, 32)
	if approved {
		data[31] = 1
	}
	return &types.Log{Address: nftCollection, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("ApprovalForAll(address,address,bool)")), common.BytesToHash(owner[:]), common.BytesToHash(operator[:]),
	}, Data: data}
}

func TestNFTRetainedTokenApprovalMatchesSurvivingHistory(t *testing.T) {
	for _, scenario := range []string{"new-grant", "replace", "revoke", "prefix-revoked", "before-transfer", "after-transfer", "uint256-id"} {
		t.Run(scenario, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			id := common.Hash{31: 1}
			if scenario == "uint256-id" {
				for i := range id {
					id[i] = 255
				}
			}
			// Synthetic mixed-case addresses exercise the same canonical
			// address representation used by normal indexing and SQL readers.
			a, b := common.Address{0: 0xab, 19: 0xcd}, common.Address{0: 0xcd, 19: 0xab}
			mint := nftTransfer(common.Address{}, nftAlice)
			mint.Topics[3] = id
			prefix := []*types.Log{mint}
			owner := nftAlice
			if scenario != "new-grant" {
				prefix = append(prefix, retainedApproval(owner, a, id))
			}
			if scenario == "prefix-revoked" {
				prefix = append(prefix, retainedApproval(owner, common.Address{}, id))
			}
			if scenario == "before-transfer" || scenario == "after-transfer" {
				transfer := nftTransfer(nftAlice, nftBob)
				transfer.Topics[3] = id
				prefix = append(prefix, transfer)
				owner = nftBob
				if scenario == "after-transfer" {
					prefix = append(prefix, retainedApproval(owner, a, id))
				}
			}
			branchSpender := b
			if scenario == "revoke" {
				branchSpender = common.Address{}
			}
			chain.Put(1, 0, prefix...)
			chain.Put(2, 0, retainedApproval(owner, branchSpender, id))
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 2)
			if err := ix.Rewind(1); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 1)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

func TestNFTRetainedOperatorApprovalMatchesSurvivingHistory(t *testing.T) {
	for _, scenario := range []string{"grant", "revoke", "unrelated-keys"} {
		t.Run(scenario, func(t *testing.T) {
			chain := indexer.NewRecoveryChain()
			grant := scenario != "grant"
			prefix := []*types.Log{nftTransfer(common.Address{}, nftAlice), retainedOperator(nftAlice, nftOperator, grant)}
			if scenario == "unrelated-keys" {
				prefix = append(prefix, retainedOperator(nftBob, nftOperator, false), retainedOperator(nftAlice, nftExchange, true))
			}
			chain.Put(1, 0, prefix...)
			chain.Put(2, 0, retainedOperator(nftAlice, nftOperator, !grant))
			ix, db, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, ix, 1, 2)
			if err := ix.Rewind(1); err != nil {
				t.Fatal(err)
			}
			reference, referenceDB, _ := nftRecoveryIndex(t, chain)
			nftIndexThrough(t, reference, 1, 1)
			indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
		})
	}
}

func TestNFTRetainedOwnershipUsesOnlyERC721Transfers(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice))
	words := make([]byte, 64)
	words[31], words[63] = 1, 5
	chain.Put(2, 0, &types.Log{Address: nftCollection, Topics: []common.Hash{
		crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)")), common.BytesToHash(nftAlice[:]), common.BytesToHash(nftAlice[:]), common.BytesToHash(nftBob[:]),
	}, Data: words})
	chain.Put(3, 0, nftTransfer(nftAlice, nftOperator))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 3)
	if err := ix.Rewind(2); err != nil {
		t.Fatal(err)
	}
	var owner string
	var approved sql.NullString
	if err := db.SQL().QueryRow("SELECT owner,approved FROM nft_tokens WHERE collection=? AND token_id='1'", nftCollection.Hex()).Scan(&owner, &approved); err != nil {
		t.Fatal(err)
	}
	if owner != nftAlice.Hex() || approved.Valid {
		t.Fatalf("retained ERC-721 owner/approval = %s/%v, want %s/no approval", owner, approved, nftAlice.Hex())
	}
	// This case qualifies ownership and the transfer counter. Historical
	// metadata/update-height convergence remains a separate acceptance scope.
	var transfers int
	if err := db.SQL().QueryRow("SELECT transfers FROM tokens WHERE address=?", nftCollection.Hex()).Scan(&transfers); err != nil || transfers != 2 {
		t.Fatalf("retained transfer count=%d, want 2: %v", transfers, err)
	}
}

func TestNFTHistoryReplayFailureRollsBackEveryTable(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedApproval(nftAlice, nftExchange, common.Hash{31: 1}), retainedOperator(nftAlice, nftOperator, true))
	chain.Put(2, 0, retainedApproval(nftAlice, nftOperator, common.Hash{31: 1}), retainedOperator(nftAlice, nftOperator, false))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 2)
	before := indexer.RecoveryState(t, db)
	if _, err := db.SQL().Exec("CREATE TRIGGER n04_approval_replay_refusal BEFORE INSERT ON nft_approvals BEGIN SELECT RAISE(ABORT,'declared approval replay refusal'); END"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Rewind(1); err == nil {
		t.Fatal("approval history rebuild did not reach the declared SQL refusal")
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
	if _, err := db.SQL().Exec("DROP TRIGGER n04_approval_replay_refusal"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	reference, referenceDB, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, reference, 1, 1)
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
}

func TestRetainedTransferCounterMatchesSurvivingRows(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	for n := uint64(1); n <= 2; n++ {
		data := make([]byte, 32)
		data[31] = byte(n)
		chain.Put(n, 0, &types.Log{Address: common.Address{19: 33}, Topics: []common.Hash{
			crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)")), common.BytesToHash(nftAlice[:]), common.BytesToHash(nftBob[:]),
		}, Data: data})
	}
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 2)
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	reference, referenceDB, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, reference, 1, 1)
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
	// A replay after rewind must count the replacement exactly once.
	if err := ix.IndexBlock(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := reference.IndexBlock(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
}

func TestNFTHistoryUpgradeRepairsPermissionsOnce(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedApproval(nftAlice, nftExchange, common.Hash{31: 1}), retainedOperator(nftAlice, nftOperator, true))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 1)
	before := indexer.RecoveryState(t, db)
	for _, statement := range []string{
		"DELETE FROM index_meta WHERE key='nft_approval_recovery_v1'",
		"UPDATE nft_tokens SET approved=NULL",
		"UPDATE nft_approvals SET approved=0",
	} {
		if _, err := db.SQL().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	reopen := func() {
		t.Helper()
		fault, _ := indexer.NewRecoveryFailure()
		if _, err := indexer.New(db, ix.Eth(), &rollup.Config{L1StartHeight: 2, L2ChainID: 69}, ix.Log(), nft.New(nft.Config{}), fault); err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
	if _, err := db.SQL().Exec("CREATE TRIGGER n04_repeat_history_refusal BEFORE UPDATE ON nft_tokens BEGIN SELECT RAISE(ABORT,'unexpected repeat approval rebuild'); END"); err != nil {
		t.Fatal(err)
	}
	reopen() // A healthy upgraded database must not replay approval history again.
	if _, err := db.SQL().Exec("DROP TRIGGER n04_repeat_history_refusal"); err != nil {
		t.Fatal(err)
	}
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), before)
}

func TestNFTRetainedOperatorKeysUseTheLiveAddressDecoding(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	last := retainedOperator(nftAlice, nftOperator, false)
	// Addr decodes the final 20 bytes in the live handler. The preceding
	// bytes must not turn the same owner/operator into separate replay keys.
	last.Topics[1][0], last.Topics[2][0] = 1, 2
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedOperator(nftAlice, nftOperator, true), last)
	chain.Put(2, 0, retainedOperator(nftAlice, nftOperator, true))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 2)
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	reference, referenceDB, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, reference, 1, 1)
	indexer.AssertRecoveryState(t, indexer.RecoveryState(t, db), indexer.RecoveryState(t, referenceDB))
}

func TestNFTRetainedApprovalWithoutOwnershipHistoryStaysUnknown(t *testing.T) {
	chain := indexer.NewRecoveryChain()
	chain.Put(1, 0, nftTransfer(common.Address{}, nftAlice), retainedApproval(nftAlice, nftExchange, common.Hash{31: 1}))
	ix, db, _ := nftRecoveryIndex(t, chain)
	nftIndexThrough(t, ix, 1, 1)
	// Declared incomplete legacy ownership history: a cached token remains,
	// but the transfer that would bind its approval can no longer be proved.
	if _, err := db.SQL().Exec("DELETE FROM token_transfers WHERE token=?", nftCollection.Hex()); err != nil {
		t.Fatal(err)
	}
	if err := ix.Rewind(1); err != nil {
		t.Fatal(err)
	}
	var approved sql.NullString
	if err := db.SQL().QueryRow("SELECT approved FROM nft_tokens WHERE collection=? AND token_id='1'", nftCollection.Hex()).Scan(&approved); err != nil || approved.Valid {
		t.Fatalf("unbound approval retained: %v, %v", approved, err)
	}
}
