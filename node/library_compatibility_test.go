// Golden values were observed with Go 1.26.11 and op-geth v1.101702.2 at
// Node 9a5c6d6145bc46979c08b7d3b3c06100ee4a8b3e and the original Core Gitlink.
// The migration receipt retains old-codec acceptance of the new payload.
// These synthetic vectors do not qualify live state or newer chain forks.
package node

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"reflect"
	"testing"

	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

const dependencyLibraryLegacyGolden = "{\"genesis\":{\"isthmus\":{\"blockHash\":\"0xaba1f252bb3ae1f25a528c4088e7682cff245ed5b3ae398422a6d913b4fe7d33\",\"chainConfig\":{\"arrowGlacierBlock\":0,\"bedrockBlock\":0,\"berlinBlock\":0,\"byzantiumBlock\":0,\"cancunTime\":0,\"canyonTime\":0,\"chainId\":321,\"constantinopleBlock\":0,\"depositContractAddress\":\"0x0000000000000000000000000000000000000000\",\"ecotoneTime\":0,\"eip150Block\":0,\"eip155Block\":0,\"eip158Block\":0,\"fjordTime\":0,\"graniteTime\":0,\"grayGlacierBlock\":0,\"holoceneTime\":0,\"homesteadBlock\":0,\"istanbulBlock\":0,\"isthmusTime\":0,\"londonBlock\":0,\"mergeNetsplitBlock\":0,\"muirGlacierBlock\":0,\"optimism\":{\"eip1559Denominator\":50,\"eip1559DenominatorCanyon\":250,\"eip1559Elasticity\":6},\"petersburgBlock\":0,\"pragueTime\":0,\"regolithTime\":0,\"shanghaiTime\":0,\"terminalTotalDifficulty\":0},\"headerRLP\":\"f90268a00000000000000000000000000000000000000000000000000000000000000000a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347940000000000000000000000000000000000000000a080c9352600da1c04a9f6620fd1775070ab9d964ea07abd64b1a927d63f133f1da056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421b901000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000080808401c9c38080846553f1008900000000fa00000006a00000000000000000000000000000000000000000000000000000000000000000880000000000000000843b9aca00a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b4218080a00000000000000000000000000000000000000000000000000000000000000000a0e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"stateRoot\":\"0x80c9352600da1c04a9f6620fd1775070ab9d964ea07abd64b1a927d63f133f1d\"},\"karst\":{\"blockHash\":\"0xf1ebec0e4a6d08e749f9f547cdbc66d055d6001e5f94fd05e72905767ce2b3d6\",\"chainConfig\":{\"arrowGlacierBlock\":0,\"bedrockBlock\":0,\"berlinBlock\":0,\"byzantiumBlock\":0,\"cancunTime\":0,\"canyonTime\":0,\"chainId\":321,\"constantinopleBlock\":0,\"depositContractAddress\":\"0x0000000000000000000000000000000000000000\",\"ecotoneTime\":0,\"eip150Block\":0,\"eip155Block\":0,\"eip158Block\":0,\"fjordTime\":0,\"graniteTime\":0,\"grayGlacierBlock\":0,\"holoceneTime\":0,\"homesteadBlock\":0,\"istanbulBlock\":0,\"isthmusTime\":0,\"jovianTime\":0,\"karstTime\":0,\"londonBlock\":0,\"mergeNetsplitBlock\":0,\"muirGlacierBlock\":0,\"optimism\":{\"eip1559Denominator\":50,\"eip1559DenominatorCanyon\":250,\"eip1559Elasticity\":6},\"osakaTime\":0,\"petersburgBlock\":0,\"pragueTime\":0,\"regolithTime\":0,\"shanghaiTime\":0,\"terminalTotalDifficulty\":0},\"headerRLP\":\"f90270a00000000000000000000000000000000000000000000000000000000000000000a01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347940000000000000000000000000000000000000000a080c9352600da1c04a9f6620fd1775070ab9d964ea07abd64b1a927d63f133f1da056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421b901000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000080808401c9c38080846553f1009101000000fa000000060000000000000001a00000000000000000000000000000000000000000000000000000000000000000880000000000000000843b9aca00a056e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b4218080a00000000000000000000000000000000000000000000000000000000000000000a0e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"stateRoot\":\"0x80c9352600da1c04a9f6620fd1775070ab9d964ea07abd64b1a927d63f133f1d\"}},\"legacyHeader\":{\"encoding\":\"f901f8a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000940000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000001234a00000000000000000000000000000000000000000000000000000000000000000a00000000000000000000000000000000000000000000000000000000000000000b901000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000080018401c9c380825208846553f10180a000000000000000000000000000000000000000000000000000000000000000008800000000000000000a\",\"hash\":\"0x0f33cb390bb5e0e52aafd369486f63ad1a81bf09572d6eb7e1d81655fef00e60\"},\"payload\":{\"eip1559Params\":\"0x0000000000000000\",\"gasLimit\":\"0x1c9c380\",\"noTxPool\":true,\"parentBeaconBlockRoot\":\"0x0000000000000000000000000000000000000000000000000000000000001234\",\"prevRandao\":\"0x0000000000000000000000000000000000000000000000000000000000000088\",\"slotNumber\":null,\"suggestedFeeRecipient\":\"0x1111111111111111111111111111111111111111\",\"timestamp\":\"0x6553f101\",\"withdrawals\":[]},\"transactions\":[{\"encoding\":\"df01028252089411111111111111111111111111111111111111110380808080\",\"hash\":\"0xb6800782b3ef6f77fa877798c2301266f50f3ba9ffd55a6cba773ad789adc5b4\"},{\"encoding\":\"02e682014104020a82659094111111111111111111111111111111111111111105820102c0808080\",\"hash\":\"0xd15e9e3ad939d5c3e4d6e99f88cae5d066748485950e6876f2b96f83568c26c3\"},{\"encoding\":\"7ef854a00000000000000000000000000000000000000000000000000000000000000042941111111111111111111111111111111111111111941111111111111111111111111111111111111111090482c35080820304\",\"hash\":\"0x7010e44bede3e0d96ce614bc5ec152ead3868dcd962dddaaa54e350b2171fe17\"}]}"

func dependencyLibraryFail(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDependencyLibraryCompatibility(t *testing.T) {
	genesis := map[string]any{}
	for _, rules := range []string{"isthmus", "karst"} {
		cfg := &rollup.Config{L2ChainID: 321, GasLimit: 30_000_000, EVM: rules, MinBaseFee: 1}
		value, err := l2.BuildGenesis(cfg, 1_700_000_000)
		dependencyLibraryFail(t, err)
		block := value.ToBlock()
		encoded, err := rlp.EncodeToBytes(block.Header())
		dependencyLibraryFail(t, err)
		config, err := json.Marshal(value.Config)
		dependencyLibraryFail(t, err)
		genesis[rules] = map[string]any{"blockHash": block.Hash().Hex(), "stateRoot": block.Root().Hex(),
			"headerRLP": hex.EncodeToString(encoded), "chainConfig": json.RawMessage(config)}
	}
	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	transactions := []*types.Transaction{
		types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(2), Gas: 21_000, To: &to, Value: big.NewInt(3)}),
		types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(321), Nonce: 4, GasTipCap: big.NewInt(2),
			GasFeeCap: big.NewInt(10), Gas: 26_000, To: &to, Value: big.NewInt(5), Data: []byte{1, 2}}),
		types.NewTx(&types.DepositTx{SourceHash: common.HexToHash("0x42"), From: to, To: &to,
			Mint: big.NewInt(9), Value: big.NewInt(4), Gas: 50_000, Data: []byte{3, 4}}),
	}
	txVectors := make([]map[string]string, len(transactions))
	for i, tx := range transactions {
		encoded, err := tx.MarshalBinary()
		dependencyLibraryFail(t, err)
		txVectors[i] = map[string]string{"hash": tx.Hash().Hex(), "encoding": hex.EncodeToString(encoded)}
	}
	header := &types.Header{Number: big.NewInt(1), Difficulty: big.NewInt(0), GasLimit: 30_000_000,
		GasUsed: 21_000, Time: 1_700_000_001, BaseFee: big.NewInt(10), Root: common.HexToHash("0x1234")}
	headerRLP, err := rlp.EncodeToBytes(header)
	dependencyLibraryFail(t, err)
	legacyHeader := map[string]string{"hash": header.Hash().Hex(), "encoding": hex.EncodeToString(headerRLP)}
	beaconRoot := common.HexToHash("0x1234")
	gasLimit := uint64(30_000_000)
	payload := &engine.PayloadAttributes{Timestamp: 1_700_000_001, Random: common.HexToHash("0x88"),
		SuggestedFeeRecipient: to, Withdrawals: []*types.Withdrawal{}, BeaconRoot: &beaconRoot,
		Transactions: [][]byte{}, NoTxPool: true, GasLimit: &gasLimit, EIP1559Params: make([]byte, 8)}
	payloadJSON, err := json.Marshal(payload)
	dependencyLibraryFail(t, err)
	actual := map[string]any{"genesis": genesis, "transactions": txVectors,
		"legacyHeader": legacyHeader, "payload": json.RawMessage(payloadJSON)}
	encoded, err := json.Marshal(actual)
	dependencyLibraryFail(t, err)
	var got, want map[string]any
	dependencyLibraryFail(t, json.Unmarshal(encoded, &got))
	dependencyLibraryFail(t, json.Unmarshal([]byte(dependencyLibraryLegacyGolden), &want))
	payloadFields := got["payload"].(map[string]any)
	value, present := payloadFields["targetGasLimit"]
	if !present || value != nil {
		t.Fatal("candidate payload targetGasLimit profile changed")
	}
	delete(payloadFields, "targetGasLimit")
	if !reflect.DeepEqual(got, want) {
		t.Fatal("synthetic chain-321, transaction, legacy-header or payload compatibility vector changed")
	}
}
