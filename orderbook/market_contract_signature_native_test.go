package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Real deployed wallet/exchange calls under declared NoM observations. The
// corresponding production profiles are a separate early gate in the same CI.
func TestExecutedERC1271NativeSignaturesAndRecovery(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit compiled ERC-1271 fixture")
	}
	cases := []struct {
		name                string
		mode                uint8
		wrongSigner, revoke bool
	}{
		{name: "valid-owner"}, {name: "wrong-signer", wrongSigner: true}, {name: "wrong-magic", mode: 1},
		{name: "reverting-wallet", mode: 2}, {name: "short-magic", mode: 3}, {name: "revoked-before-fill", revoke: true},
	}
	records := []map[string]any{}
	for _, kind := range []string{"single", "bulk"} {
		for _, item := range cases {
			ownerKey := executedMarketKey(t)
			owner := crypto.PubkeyToAddress(ownerKey.PublicKey)
			setup := loadContractSignatureWallet(t, owner)
			setup.mode = item.mode
			setup.revokeBeforeFill = item.revoke
			signingKey := ownerKey
			if item.wrongSigner {
				scalar := make([]byte, 32)
				scalar[31] = 3
				var err error
				signingKey, err = crypto.ToECDSA(scalar)
				if err != nil {
					t.Fatal(err)
				}
			}
			scenario := newExecutionScenarioInputs(t, false, false, executionScenarioInputs{
				seller: znn.Address(owner), timestamp: 1_800_000_000, contractSignature: setup,
				sign: func(s *executionScenario) []*Input {
					domain := Domain(s.cfg.L2ChainID, s.exchange)
					if kind == "bulk" {
						inputs, err := SignBulk(signingKey, domain, s.orders)
						if err != nil {
							t.Fatal(err)
						}
						return inputs
					}
					out := []*Input{}
					for _, order := range s.orders {
						input, err := Sign(signingKey, domain, order)
						if err != nil {
							t.Fatal(err)
						}
						out = append(out, input)
					}
					return out
				},
			})
			run := newExecutionRun(t, scenario)
			run.nomMu.Lock()
			run.branch = 1
			run.nomMu.Unlock()
			run.through(9, false)
			ctx := context.Background()
			input := scenario.inputs[0]
			price := input.Order.Price.Big()
			digest := contractInputDigest(Domain(scenario.cfg.L2ChainID, scenario.exchange), input)
			recovered, ok := Recover(Domain(scenario.cfg.L2ChainID, scenario.exchange), input)
			if !ok || recovered != crypto.PubkeyToAddress(signingKey.PublicKey) || recovered == setup.wallet || input.Order.Trader != setup.wallet || setup.wallet == owner {
				t.Fatal("native fixture did not separate contract trader and signing owner")
			}
			payout := scenario.payouts
			if payout == nil || payout.feeBps != 200 || payout.royaltyBps != 375 {
				t.Fatal("native contract-signature payout setup differs")
			}
			actors := []common.Address{common.Address(scenario.buyer), setup.wallet, payout.feeRecipient, payout.royaltyRecipient}
			before := map[common.Address]*big.Int{}
			for _, actor := range actors {
				if actor == (common.Address{}) || before[actor] != nil {
					t.Fatal("native payout actors must be distinct")
				}
				value, err := run.eth.BalanceAt(ctx, actor, nil)
				if err != nil {
					t.Fatal(err)
				}
				before[actor] = new(big.Int).Set(value)
			}
			if before[actors[0]].Cmp(price) < 0 {
				t.Fatal("contract-signature buyer prefix is not fully funded")
			}
			purchase, err := scenario.arts.MustGet("ZVMExchange").Call("buy", input.ABI())
			if err != nil {
				t.Fatal(err)
			}
			observations := []map[string]any{}
			var transaction common.Hash
			observe := func(phase string, attempted bool) map[string]any {
				header, err := run.eth.HeaderByNumber(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				hash := header.Hash()
				code, err := run.eth.CodeAtHash(ctx, setup.wallet, hash)
				if err != nil || !bytes.Equal(code, setup.runtime) {
					t.Fatalf("native %s/%s external wallet runtime differs", kind, item.name)
				}
				walletRead := func(method string, args ...any) ([]byte, error) {
					return run.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &setup.wallet, Data: setup.call(t, method, args...)}, hash)
				}
				raw, err := walletRead("owner")
				if err != nil || len(raw) != 32 || common.BytesToAddress(raw) != owner {
					t.Fatal("native wallet owner differs")
				}
				mode := item.mode
				if item.revoke && attempted {
					mode = 2
				}
				raw, err = walletRead("signatureMode")
				if err != nil || len(raw) != 32 || new(big.Int).SetBytes(raw).Uint64() != uint64(mode) {
					t.Fatal("native signature mode differs from actual canonical state")
				}
				response, signatureError := walletRead("isValidSignature", digest, input.Signature)
				signatureValid := mode == 0 && !item.wrongSigner
				if mode == 2 {
					if signatureError == nil || !strings.Contains(signatureError.Error(), "execution reverted") {
						t.Fatal("native ERC-1271 revert was not executed")
					}
				} else if mode == 3 {
					if signatureError != nil || !bytes.Equal(response, []byte{0x16, 0x26, 0xba, 0x7e}) {
						t.Fatal("native short magic response differs")
					}
				} else {
					magic := []byte{0xff, 0xff, 0xff, 0xff}
					if signatureValid {
						magic = []byte{0x16, 0x26, 0xba, 0x7e}
					}
					if signatureError != nil || len(response) != 32 || !bytes.Equal(response[:4], magic) {
						t.Fatal("native ERC-1271 full reply differs")
					}
				}
				filled := attempted && item.name == "valid-owner"
				read := func(artifact, method string, to common.Address, args ...any) []byte {
					data, err := scenario.arts.MustGet(artifact).Call(method, args...)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := run.eth.CallContractAtHash(ctx, ethereum.CallMsg{To: &to, Data: data}, hash)
					if err != nil {
						t.Fatal(err)
					}
					return raw
				}
				checked := read("ZVMExchange", "checkOrder", scenario.exchange, input.ABI())
				wantCode, remaining := CodeOK, int64(1)
				if !signatureValid {
					wantCode, remaining = CodeBadSignature, 0
				} else if filled {
					wantCode, remaining = CodeFilled, 0
				}
				if len(checked) != 64 || new(big.Int).SetBytes(checked[:32]).Uint64() != uint64(wantCode) || new(big.Int).SetBytes(checked[32:]).Cmp(big.NewInt(remaining)) != 0 {
					t.Fatalf("native ERC-1271 checkOrder differs: kind=%s case=%s phase=%s code=%d", kind, item.name, phase, wantCode)
				}
				count := int64(0)
				wantOwner := setup.wallet
				if filled {
					count = 1
					wantOwner = common.Address(scenario.buyer)
				}
				raw = read("ZVMExchange", "filled", scenario.exchange, input.Order.Hash())
				if len(raw) != 32 || new(big.Int).SetBytes(raw).Cmp(big.NewInt(count)) != 0 {
					t.Fatal("native contract-signature fill differs")
				}
				raw = read("ZVMCollection", "ownerOf", scenario.token, big.NewInt(1))
				if len(raw) != 32 || common.BytesToAddress(raw) != wantOwner {
					t.Fatal("native contract-signature ownership differs")
				}
				gross, royalty, fee := new(big.Int), new(big.Int), new(big.Int)
				if filled {
					gross.Set(price)
					royalty = payoutShare(price, 375)
					fee = payoutShare(price, 200)
				}
				proceeds := new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Set(gross), royalty), fee)
				wanted := []*big.Int{new(big.Int).Sub(new(big.Int).Set(before[actors[0]]), gross),
					new(big.Int).Add(new(big.Int).Set(before[actors[1]]), proceeds),
					new(big.Int).Add(new(big.Int).Set(before[actors[2]]), fee), new(big.Int).Add(new(big.Int).Set(before[actors[3]]), royalty)}
				balances := []string{}
				for i, actor := range actors {
					value, err := run.eth.BalanceAtHash(ctx, actor, hash)
					if err != nil || value.Cmp(wanted[i]) != 0 {
						t.Fatalf("native contract-signature balance differs: case=%s phase=%s role=%d got=%v want=%v error=%v", item.name, phase, i, value, wanted[i], err)
					}
					credit := read("ZVMExchange", "credits", scenario.exchange, actor)
					if len(credit) != 32 || new(big.Int).SetBytes(credit).Sign() != 0 {
						t.Fatal("native wallet retained an undeclared exchange credit")
					}
					balances = append(balances, value.String())
				}
				var found common.Hash
				matches := 0
				run.backend.mu.Lock()
				for n := uint64(1); n <= run.backend.chain.CurrentBlock().Number.Uint64(); n++ {
					for _, tx := range run.backend.chain.GetBlockByNumber(n).Transactions() {
						if tx.Type() == types.DepositTxType && tx.From() == common.Address(scenario.buyer) && tx.To() != nil && *tx.To() == scenario.exchange && tx.Value().Cmp(price) == 0 && bytes.Equal(tx.Data(), purchase) {
							found = tx.Hash()
							matches++
						}
					}
				}
				run.backend.mu.Unlock()
				receiptStatus := uint64(0)
				gasUsed := uint64(0)
				if attempted {
					if matches != 1 || found == (common.Hash{}) || (transaction != (common.Hash{}) && transaction != found) {
						t.Fatal("native signature purchase bytes/hash differ")
					}
					transaction = found
					receipt, err := run.eth.TransactionReceipt(ctx, found)
					expected := uint64(types.ReceiptStatusFailed)
					if filled {
						expected = types.ReceiptStatusSuccessful
					}
					if err != nil || receipt.Status != expected || receipt.Type != types.DepositTxType || receipt.GasUsed <= 21000 {
						t.Fatal("native signature purchase lacks the expected actual receipt")
					}
					receiptStatus, gasUsed = receipt.Status, receipt.GasUsed
				} else {
					if matches != 0 {
						t.Fatal("replacement retained the native signature purchase")
					}
					if transaction != (common.Hash{}) {
						receipt, err := run.eth.TransactionReceipt(ctx, transaction)
						if receipt != nil || err == nil {
							t.Fatal("orphaned native signature receipt remained canonical")
						}
					}
				}
				after, err := run.eth.HeaderByNumber(ctx, header.Number)
				if err != nil || after.Hash() != hash {
					t.Fatal("native signature observation lost its canonical hash")
				}
				value := map[string]any{"phase": phase, "hash": hash.Hex(), "attempted": attempted, "filled": filled, "signatureMode": mode, "signatureValid": signatureValid,
					"signatureReplyBytes": len(response), "signatureCallReverted": signatureError != nil, "checkOrderCode": wantCode, "remaining": remaining, "owner": wantOwner.Hex(),
					"balances": balances, "gross": gross.String(), "royalty": royalty.String(), "fee": fee.String(), "sellerProceeds": proceeds.String(),
					"receiptStatus": receiptStatus, "receiptGasUsed": gasUsed, "actualHashBoundContractAndBalanceReads": true, "allCreditsZero": true}
				observations = append(observations, value)
				return value
			}
			observe("prefix", false)
			last := uint64(9)
			if item.revoke {
				last = 11
			}
			run.nomMu.Lock()
			run.branch = 0
			run.nomMu.Unlock()
			run.through(last, false)
			mined := observe("mined", true)
			run.nomMu.Lock()
			run.branch = 1
			run.nomMu.Unlock()
			run.through(last, false)
			observe("replacement", false)
			run.nomMu.Lock()
			run.branch = 0
			run.nomMu.Unlock()
			run.through(last, false)
			restored := observe("restored", true)
			for _, key := range []string{"hash", "balances", "signatureMode", "checkOrderCode", "owner", "receiptStatus", "receiptGasUsed"} {
				if !reflect.DeepEqual(mined[key], restored[key]) {
					t.Fatal("native signature restoration differs")
				}
			}
			run.close()
			run.open()
			run.catchUp()
			reopened := observe("reopened", true)
			for _, key := range []string{"hash", "balances", "signatureMode", "checkOrderCode", "owner", "receiptStatus", "receiptGasUsed"} {
				if !reflect.DeepEqual(mined[key], reopened[key]) {
					t.Fatal("native signature reopening differs")
				}
			}
			run.close()
			records = append(records, map[string]any{"case": item.name, "kind": kind, "sourceSHA256": setup.sourceSHA, "compilerSHA256": setup.compilerSHA, "artifactSHA256": setup.artifactSHA,
				"owner": owner.Hex(), "trader": setup.wallet.Hex(), "recoveredEOA": recovered.Hex(), "digest": digest.Hex(), "price": price.String(), "transaction": transaction.Hex(),
				"observations": observations, "actualDeployedWalletAndExchange": true, "actualNativePurchaseAttempt": true, "directStateOrStorageInsertion": 0})
		}
	}
	if len(records) != 12 {
		t.Fatal("native ERC-1271 matrix is incomplete")
	}
	raw, err := json.Marshal(map[string]any{"pass": true, "records": records, "singleAndBulk": true, "canonicalRevocationAndRecovery": true,
		"declaredNoMObservations": true, "authenticatedNoMQualified": false, "physicalWalletOrPublicNetwork": false, "productionBrowserExecutionQualified": false})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ERC1271-NATIVE-QUALIFICATION " + string(raw))
}
