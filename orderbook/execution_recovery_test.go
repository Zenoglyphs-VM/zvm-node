package orderbook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/store"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
	"github.com/Zenoglyphs-VM/zvm-node/bench"
	"github.com/Zenoglyphs-VM/zvm-node/indexer"
	"github.com/Zenoglyphs-VM/zvm-node/indexer/nft"
	"github.com/Zenoglyphs-VM/zvm-node/node"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

// This fixture executes the pinned op-geth block builder, validator, state and
// shipped contract artifacts. Its Engine/eth RPC control plane and unsigned NoM
// observations are test adapters. Ordinary recovery profiles use approved orders;
// the optional market profile uses public-test-key off-chain signatures. No real
// daemon, account manager, wallet or public RPC is involved. Reopening is a
// process-boundary model with graceful handle closure, not a power-loss test.
type executionBackend struct{ chain *core.BlockChain }

func (b executionBackend) BlockChain() *core.BlockChain { return b.chain }
func (executionBackend) TxPool() *txpool.TxPool         { return nil }

type executionEngine struct {
	mu           sync.Mutex
	db           ethdb.Database
	chain        *core.BlockChain
	builder      *miner.Miner
	payloads     map[engine.PayloadID]*engine.ExecutionPayloadEnvelope
	lostReply    uint64
	processPhase string
	closed       bool
	provider     *ExecutedMarketProviderFixture
}

func openExecutionEngine(t *testing.T, path string, genesis *core.Genesis, buffered bool) *executionEngine {
	t.Helper()
	kv, err := leveldb.New(path, 16, 16, "execution-recovery", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	cfg := core.DefaultConfig()
	cfg.TrieCleanLimit, cfg.TrieDirtyLimit, cfg.SnapshotLimit = 16, 16, 0
	cfg.ArchiveMode, cfg.TrieNoAsyncFlush = !buffered, true
	sealer := beacon.New(ethash.NewFaker())
	chain, err := core.NewBlockChain(db, genesis, sealer, cfg)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	b := &executionEngine{db: db, chain: chain, payloads: map[engine.PayloadID]*engine.ExecutionPayloadEnvelope{}}
	b.builder = miner.New(executionBackend{chain}, miner.DefaultConfig, sealer)
	t.Cleanup(b.close)
	return b
}

func (b *executionEngine) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.builder.Close()
		b.chain.Stop()
		b.db.Close()
		b.closed = true
	}
}

func (b *executionEngine) ForkchoiceUpdatedV3(ctx context.Context, fc engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	head := b.chain.GetBlockByHash(fc.HeadBlockHash)
	if head == nil {
		return engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: engine.SYNCING}}, nil
	}
	if _, err := b.chain.SetCanonical(head); err != nil {
		return engine.ForkChoiceResponse{}, err
	}
	result := engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &fc.HeadBlockHash}}
	if attrs == nil {
		if b.processPhase == "engine-commit" && head.NumberU64() == 7 {
			exitOwnedRecoveryProcess(b.processPhase)
		}
		if b.lostReply != 0 && head.NumberU64() == b.lostReply {
			b.lostReply = 0
			return engine.ForkChoiceResponse{}, errors.New("declared lost reply after canonical engine commit")
		}
		return result, nil
	}
	if !attrs.NoTxPool {
		return engine.ForkChoiceResponse{}, errors.New("fixture requires explicitly supplied transactions and no transaction pool")
	}
	txs, err := engine.DecodeTransactions(attrs.Transactions)
	if err != nil {
		return engine.ForkChoiceResponse{}, err
	}
	for _, tx := range txs {
		if tx.Type() != types.DepositTxType && (b.provider == nil || tx.Type() != types.DynamicFeeTxType) {
			return engine.ForkChoiceResponse{}, errors.New("signed transaction outside fixture scope")
		}
	}
	args := &miner.BuildPayloadArgs{Parent: fc.HeadBlockHash, Timestamp: attrs.Timestamp, FeeRecipient: attrs.SuggestedFeeRecipient,
		Random: attrs.Random, Withdrawals: attrs.Withdrawals, BeaconRoot: attrs.BeaconRoot, Version: engine.PayloadV3,
		NoTxPool: true, Transactions: txs, GasLimit: attrs.GasLimit, EIP1559Params: attrs.EIP1559Params, MinBaseFee: attrs.MinBaseFee}
	payload, err := b.builder.BuildPayload(ctx, args, false)
	if err != nil {
		return engine.ForkChoiceResponse{}, err
	}
	envelope := payload.ResolveFull()
	if envelope == nil {
		return engine.ForkChoiceResponse{}, errors.New("missing executed payload")
	}
	// The pinned encoder omits an empty MessagePasser storage root because it
	// equals EmptyWithdrawalsHash. This fixture uses that empty genesis state.
	// Restore the RPC field explicitly: ExecutableDataToBlock still requires
	// the original block hash, and InsertBlockWithoutSetHead re-executes it and
	// validates the state and withdrawal roots. This adapter is not evidence
	// that the full daemon's unmodified RPC endpoint supports this genesis.
	if envelope.ExecutionPayload.WithdrawalsRoot == nil {
		empty := types.EmptyRootHash
		envelope.ExecutionPayload.WithdrawalsRoot = &empty
	}
	id := args.Id()
	b.payloads[id], result.PayloadID = envelope, &id
	return result, nil
}

func (b *executionEngine) GetPayloadV4(_ context.Context, id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if value := b.payloads[id]; value != nil {
		return value, nil
	}
	return nil, errors.New("unknown fixture payload")
}

func (b *executionEngine) NewPayloadV4(ctx context.Context, data engine.ExecutableData, hashes []common.Hash, root *common.Hash, requests []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rawRequests := make([][]byte, len(requests))
	for i := range requests {
		rawRequests[i] = requests[i]
	}
	block, err := engine.ExecutableDataToBlock(data, hashes, root, rawRequests, b.chain.Config())
	if err != nil {
		return engine.PayloadStatusV1{}, err
	}
	if _, err := b.chain.InsertBlockWithoutSetHead(ctx, block, false); err != nil {
		return engine.PayloadStatusV1{}, err
	}
	hash := block.Hash()
	return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &hash}, nil
}

type executionEth struct{ backend *executionEngine }

func (s executionEth) ChainId() *hexutil.Big {
	return (*hexutil.Big)(new(big.Int).Set(s.backend.chain.Config().ChainID))
}

func (s executionEth) header(selector rpc.BlockNumberOrHash) (*types.Header, error) {
	b := s.backend
	if hash, ok := selector.Hash(); ok {
		if header := b.chain.GetHeaderByHash(hash); header != nil {
			return header, nil
		}
	} else if number, ok := selector.Number(); ok {
		if number == rpc.LatestBlockNumber || number == rpc.PendingBlockNumber {
			return b.chain.CurrentBlock(), nil
		}
		if number >= 0 {
			if header := b.chain.GetHeaderByNumber(uint64(number)); header != nil {
				return header, nil
			}
		}
	}
	return nil, errors.New("unknown executed block")
}

func (s executionEth) BlockNumber() hexutil.Uint64 {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	return hexutil.Uint64(s.backend.chain.CurrentBlock().Number.Uint64())
}

func (s executionEth) GetBlockByNumber(_ context.Context, n rpc.BlockNumber, full bool) (map[string]any, error) {
	return s.block(rpc.BlockNumberOrHashWithNumber(n), full)
}
func (s executionEth) GetBlockByHash(_ context.Context, hash common.Hash, full bool) (map[string]any, error) {
	return s.block(rpc.BlockNumberOrHashWithHash(hash, false), full)
}
func (s executionEth) block(selector rpc.BlockNumberOrHash, full bool) (map[string]any, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	header, err := s.header(selector)
	if err != nil {
		return nil, err
	}
	block := s.backend.chain.GetBlock(header.Hash(), header.Number.Uint64())
	raw, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	transactions := make([]any, 0, len(block.Transactions()))
	for i, tx := range block.Transactions() {
		if !full {
			transactions = append(transactions, tx.Hash())
			continue
		}
		raw, err := tx.MarshalJSON()
		if err != nil {
			return nil, err
		}
		item := map[string]any{}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		item["blockHash"], item["blockNumber"], item["transactionIndex"] = block.Hash(), hexutil.Uint64(block.NumberU64()), hexutil.Uint64(i)
		if tx.Type() == types.DepositTxType {
			item["from"] = tx.From()
		} else {
			from, err := types.Sender(types.LatestSignerForChainID(s.backend.chain.Config().ChainID), tx)
			if err != nil {
				return nil, err
			}
			item["from"] = from
		}
		transactions = append(transactions, item)
	}
	out["transactions"], out["uncles"], out["withdrawals"] = transactions, []common.Hash{}, block.Withdrawals()
	return out, nil
}

func (s executionEth) GetBlockReceipts(_ context.Context, selector rpc.BlockNumberOrHash) (types.Receipts, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	header, err := s.header(selector)
	if err != nil {
		return nil, err
	}
	return s.backend.chain.GetReceiptsByHash(header.Hash()), nil
}

func (s executionEth) GetCode(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	header, err := s.header(selector)
	if err != nil {
		return nil, err
	}
	state, err := s.backend.chain.StateAt(header.Root)
	if err != nil {
		return nil, err
	}
	return state.GetCode(address), nil
}

type executionCall struct {
	From  common.Address
	To    common.Address
	Gas   hexutil.Uint64
	Input hexutil.Bytes
	Data  hexutil.Bytes
}

func (s executionEth) Call(ctx context.Context, args executionCall, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	header, err := s.header(selector)
	if err != nil {
		return nil, err
	}
	state, err := s.backend.chain.StateAt(header.Root)
	if err != nil {
		return nil, err
	}
	evm := vm.NewEVM(core.NewEVMBlockContext(header, s.backend.chain, nil, s.backend.chain.Config(), state), state, s.backend.chain.Config(), vm.Config{NoBaseFee: true})
	evm.SetTxContext(vm.TxContext{Origin: args.From, GasPrice: new(uint256.Int)})
	gas := uint64(args.Gas)
	if gas == 0 || gas > 8_000_000 {
		gas = 8_000_000
	}
	data := args.Input
	if data == nil {
		data = args.Data
	}
	result, _, err := evm.StaticCall(args.From, args.To, data, gas)
	return result, err
}

func (s executionEth) GetLogs(_ context.Context, query struct {
	FromBlock rpc.BlockNumber
	ToBlock   rpc.BlockNumber
	Address   []common.Address
	Topics    [][]common.Hash
}) ([]*types.Log, error) {
	s.backend.mu.Lock()
	defer s.backend.mu.Unlock()
	end := s.backend.chain.CurrentBlock().Number.Uint64()
	if query.ToBlock >= 0 {
		end = min(end, uint64(query.ToBlock))
	}
	start := uint64(0)
	if query.FromBlock >= 0 {
		start = uint64(query.FromBlock)
	}
	result := []*types.Log{}
	for n := start; n <= end; n++ {
		for _, receipt := range s.backend.chain.GetReceiptsByHash(s.backend.chain.GetHeaderByNumber(n).Hash()) {
			for _, entry := range receipt.Logs {
				matches := len(query.Address) == 0
				for _, address := range query.Address {
					matches = matches || address == entry.Address
				}
				for i, alternatives := range query.Topics {
					match := len(alternatives) == 0
					for _, topic := range alternatives {
						match = match || (i < len(entry.Topics) && entry.Topics[i] == topic)
					}
					matches = matches && match
				}
				if matches {
					result = append(result, entry)
				}
			}
		}
	}
	return result, nil
}

type executionScenario struct {
	cfg                      *rollup.Config
	genesis                  *core.Genesis
	arts                     *bench.Artifacts
	seller, buyer            znn.Address
	factory, exchange, token common.Address
	orders                   []*Order
	inputs                   []*Input
	branches                 [2]map[uint64]*znn.DetailedMomentum
	validated, buffered      bool
	provider                 bool
	providerAction           string
	payment                  string
	credit                   *executedCreditSetup
	payouts                  *executedMarketPayouts
}

func newExecutionScenario(t *testing.T) *executionScenario {
	return newExecutionScenarioProfile(t, false, false)
}

// Defaults preserve the original recovery scenario and its historical inputs.
type executionScenarioInputs struct {
	buyer     znn.Address
	provider  bool
	action    string
	payment   string
	seller    znn.Address
	timestamp uint64
	sign      func(*executionScenario) []*Input
	orders    []*Order
	credit    *executedCreditSetup
}

func newExecutionScenarioProfile(t *testing.T, validated, buffered bool, profileDir ...string) *executionScenario {
	t.Helper()
	return newExecutionScenarioInputs(t, validated, buffered, executionScenarioInputs{
		seller: znn.Address{19: 161}, timestamp: 1_800_000_000,
	}, profileDir...)
}

func newExecutionScenarioInputs(t *testing.T, validated, buffered bool, inputs executionScenarioInputs, profileDir ...string) *executionScenario {
	t.Helper()
	arts, err := bench.LoadArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../apps/artifacts/apps.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := arts.AddCombined(raw); err != nil {
		t.Fatal(err)
	}
	s := &executionScenario{arts: arts, seller: inputs.seller, buyer: znn.Address{19: 162}, validated: validated, buffered: buffered}
	s.provider = inputs.provider
	s.providerAction = inputs.action
	s.payment = inputs.payment
	if s.payment != "" && (!inputs.provider || inputs.credit != nil ||
		(tokenPaymentBase(s.payment) != "qsr" || s.providerAction != "buy") && (tokenPaymentBase(s.payment) != "mixed" || (s.providerAction != "batch" && s.providerAction != "partial-batch"))) {
		t.Fatal("unsupported token payment scenario")
	}
	if tokenNeedsApproval(s.payment) && s.providerAction == "partial-batch" {
		t.Fatal("approval and seller-cancellation profiles remain separate")
	}
	s.credit = inputs.credit
	if s.provider && s.providerAction == "" {
		s.providerAction = "buy"
	}
	if inputs.provider && inputs.credit == nil && (s.providerAction == "buy" || s.providerAction == "batch" || s.providerAction == "partial-batch") {
		s.payouts = newExecutedMarketPayouts()
	}
	if inputs.buyer != (znn.Address{}) {
		s.buyer = inputs.buyer
	}
	s.cfg = &rollup.Config{L1ChainID: 70, L2ChainID: 7340470, Inbox: rollup.Inbox, Vault: znn.Address{19: 163},
		L1StartHeight: 2, L2GenesisTimestamp: inputs.timestamp, GasLimit: 30_000_000, GasPerPlasma: 1000,
		MaxNativeGas: 12_000_000, MaxNativeGasPerBlock: 22_500_000, NativeFloorBP: 2500, NativeCeilBP: 7500,
		MandatoryShareBP: 200, MaxTxGas: 30_000_000, ChunkTTL: 100, MaxAssembledBytes: 1 << 19, MaxBatchBytes: 1 << 22,
		FinalityDepth: 100, Forks: &rollup.Forks{Gas: 2, Wire: 2}}
	if validated {
		s.cfg.Forks = &rollup.Forks{Gas: 3, Wire: 3}
		s.cfg.FeeSplitter = rollup.FeeSplitterAddr
		s.cfg.MaxDepositCallGas = 1_000_000
		s.cfg.Fee = &rollup.Fee{FromHeight: 3, GasPerSupply: 100_000_000_000_000, GenesisTimestamp: int64(s.cfg.L2GenesisTimestamp),
			RefEpoch: 0, RefZNN: "100000000000000", RefQSR: "200000000000000"}
	}
	s.genesis, err = l2.BuildGenesis(s.cfg, s.cfg.L2GenesisTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.L2GenesisHash = s.genesis.ToBlock().Hash()
	if validated {
		// Children use their parent's owned fixture directory so an intentional
		// exit cannot leave an unowned sibling testing.T temporary directory.
		if len(profileDir) > 1 {
			t.Fatal("one owned profile directory is required")
		}
		var dir string
		if len(profileDir) == 1 {
			dir = profileDir[0]
		} else {
			dir = t.TempDir()
		}
		data, err := json.Marshal(s.cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "validated-rollup.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		s.cfg, err = rollup.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if s.cfg.L2GenesisHash != s.genesis.ToBlock().Hash() {
			t.Fatal("loaded genesis hash differs")
		}
	}
	offset := uint64(0)
	if validated {
		offset = 1
	} // The real CREATE2 deployer call consumes a sender nonce.
	implementation := crypto.CreateAddress(common.Address(s.seller), offset)
	s.factory = crypto.CreateAddress(common.Address(s.seller), offset+1)
	s.exchange = crypto.CreateAddress(common.Address(s.seller), offset+2)
	s.token = crypto.CreateAddress(s.factory, 1)
	for id := int64(1); id <= 2; id++ {
		s.orders = append(s.orders, &Order{Trader: common.Address(s.seller), Side: SideSell, Collection: s.token,
			TokenID: U(id), Amount: U(1), Price: B(big.NewInt(250_000_000_000_000_000)), ListingTime: s.cfg.L2GenesisTimestamp,
			ExpirationTime: s.cfg.L2GenesisTimestamp + 86400, Nonce: U(0), Salt: U(id)})
	}
	if s.payment != "" {
		for i, order := range s.orders {
			if tokenPaymentBase(s.payment) == "qsr" || i == 1 {
				order.PaymentToken, order.Price = rollup.QSRTokenAddr, U(20_000_001)
			}
		}
	}
	if inputs.orders != nil {
		if len(inputs.orders) != 2 {
			t.Fatal("executed scenario requires two actual accepted orders")
		}
		s.orders = inputs.orders
	}
	for _, order := range s.orders {
		s.inputs = append(s.inputs, &Input{Order: *order, Kind: KindOnChain})
	}
	if inputs.sign != nil {
		s.inputs = inputs.sign(s)
		if len(s.inputs) != len(s.orders) {
			t.Fatal("signed scenario input count differs")
		}
		for i, input := range s.inputs {
			if input == nil || input.Order.Hash() != s.orders[i].Hash() {
				t.Fatal("signed scenario changed the intended order")
			}
		}
	}
	callData := func(artifact, method string, args ...any) []byte {
		data, err := arts.MustGet(artifact).Call(method, args...)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	lastHeight, replacementHeight := uint64(9), uint64(8)
	if inputs.provider {
		lastHeight, replacementHeight = 11, 10
		if tokenNeedsApproval(s.payment) {
			lastHeight = 12
		}
		if !executedMarketBuyerAction(s.providerAction) {
			lastHeight, replacementHeight = 14, 13
			if s.providerAction == "partial-batch" {
				lastHeight = 15
			}
		}
	}
	if s.credit != nil {
		lastHeight, replacementHeight = creditFundingHeight+2, creditFundingHeight+1
	}
	for branch := range s.branches {
		momentums := map[uint64]*znn.DetailedMomentum{1: {Momentum: &znn.Momentum{Height: 1, Hash: znn.Sha3([]byte("declared-native-recovery-genesis")), Timestamp: s.cfg.L2GenesisTimestamp}}}
		heights := map[znn.Address]uint64{}
		previous := map[znn.Address]znn.Hash{}
		var chunk znn.Hash
		for h := uint64(2); h <= lastHeight; h++ {
			variant := byte(0)
			if h >= replacementHeight {
				variant = byte(branch)
			}
			dm := &znn.DetailedMomentum{Momentum: &znn.Momentum{Height: h, Hash: znn.Sha3([]byte{byte(h), variant}),
				PreviousHash: momentums[h-1].Momentum.Hash, Timestamp: s.cfg.L2GenesisTimestamp + h*10, ChainIdentifier: 70, Version: 1}}
			sendToken := func(from, to znn.Address, currency znn.TokenStandard, amount uint64, data []byte) *znn.AccountBlock {
				heights[from]++
				block := &znn.AccountBlock{Version: 1, ChainIdentifier: 70, BlockType: znn.BlockTypeUserSend, Height: heights[from],
					Address: from, ToAddress: to, TokenStandard: currency, Amount: znn.BigIntFromUint64(amount),
					Data: data, UsedPlasma: znn.BaseSendPlasma(len(data)), BasePlasma: znn.BaseSendPlasma(len(data))}
				block.FusedPlasma = block.UsedPlasma
				if validated {
					block.PreviousHash = previous[from]
					block.MomentumAcknowledged = znn.HashHeight{Hash: dm.Momentum.PreviousHash, Height: h - 1}
					if to == s.cfg.Inbox && s.cfg.Fee.Active(h) && len(data) > 0 && (data[0] == derive.MsgCall || data[0] == derive.MsgAssemble) {
						gas := uint64(500_000)
						if data[0] == derive.MsgCall {
							call, err := derive.DecodeCall(data[1:], true)
							if err != nil {
								t.Fatal(err)
							}
							gas = call.Gas
						}
						quote := derive.FeeQuote(s.cfg, znn.ZNN, gas, dm.Momentum, block)
						if !quote.IsUint64() {
							t.Fatal("synthetic native fee exceeds the declared amount bound")
						}
						block.TokenStandard, block.Amount = znn.ZNN, znn.BigIntFromUint64(quote.Uint64())
					}
				}
				block.Hash = block.ComputeHash()
				previous[from] = block.Hash
				dm.Blocks = append(dm.Blocks, block)
				dm.Momentum.Content = append(dm.Momentum.Content, znn.AccountHeader{Address: from, Height: block.Height, Hash: block.Hash})
				return block
			}
			send := func(from, to znn.Address, amount uint64, data []byte) *znn.AccountBlock {
				currency := znn.ZeroTokenStandard
				if amount > 0 {
					currency = znn.ZNN
				}
				return sendToken(from, to, currency, amount, data)
			}
			native := func(from znn.Address, to *common.Address, value *big.Int, data []byte) {
				send(from, s.cfg.Inbox, 0, derive.EncodeCall(derive.Call{To: to, Value: value, Gas: 8_000_000, Data: data}))
			}
			switch h {
			case 2, 3, 4:
				if h == 2 && s.payment != "" {
					// Mint through the real QSR bridge before later fee/funding messages.
					sendToken(s.buyer, s.cfg.Vault, znn.QSR, 100_000_000, nil)
				}
				if h == 2 && validated {
					deployer := rollup.Create2Deployer
					native(s.seller, &deployer, nil, rollup.DeployCalldata(rollup.FeesSalt, rollup.FeesArtifact.InitCode()))
				}
				name, args := "ZVMCollection", []any{}
				if h == 3 {
					name, args = "ZVMCollectionFactory", []any{implementation}
				} else if h == 4 {
					name, args = "ZVMExchange", []any{common.Address(s.seller), common.Address(s.seller), big.NewInt(0)}
					if s.payouts != nil {
						args = []any{common.Address(s.seller), s.payouts.feeRecipient, new(big.Int).SetUint64(s.payouts.feeBps)}
					}
					if s.credit != nil {
						args = []any{common.Address(s.seller), s.credit.wallet, big.NewInt(200)}
					}
				}
				init, err := arts.MustGet(name).Deploy(args...)
				if err != nil {
					t.Fatal(err)
				}
				native(s.seller, nil, nil, init)
			case 5:
				native(s.seller, &s.factory, nil, callData("ZVMCollectionFactory", "createCollection", "Recovery", "REC", "", "", big.NewInt(0)))
				if s.credit != nil {
					native(s.buyer, nil, nil, s.credit.init)
				}
			case 6:
				native(s.seller, &s.token, nil, callData("ZVMCollection", "mintBatch", common.Address(s.seller), []string{"one", "two", "three"}))
				native(s.seller, &s.token, nil, callData("ZVMCollection", "setApprovalForAll", s.exchange, true))
				if s.payouts != nil {
					// Mint and approval already reserve 16M gas in this momentum.
					// Bound the simple setter without widening the native ceiling.
					send(s.seller, s.cfg.Inbox, 0, derive.EncodeCall(derive.Call{To: &s.token, Gas: 500_000,
						Data: callData("ZVMCollection", "setRoyalty", s.payouts.royaltyRecipient, new(big.Int).SetUint64(s.payouts.royaltyBps))}))
				}
				if s.payment != "" && !tokenNeedsApproval(s.payment) {
					allowance := new(big.Int)
					for _, order := range s.orders {
						if order.PaymentToken == rollup.QSRTokenAddr {
							allowance.Add(allowance, order.Price.Big())
						}
					}
					qsr := rollup.QSRTokenAddr
					send(s.buyer, s.cfg.Inbox, 0, derive.EncodeCall(derive.Call{To: &qsr, Gas: 500_000,
						Data: tokenPaymentCall("approve(address,uint256)", tokenPaymentAddress(s.exchange), allowance)}))
				}
			case 7:
				if inputs.sign == nil {
					native(s.seller, &s.exchange, nil, callData("ZVMExchange", "approveOrders", ABIOrders(s.orders)))
				}
				payload := derive.EncodeCall(derive.Call{To: &s.token, Gas: 500_000, Data: callData("ZVMCollection", "transferFrom", common.Address(s.seller), common.Address(s.buyer), big.NewInt(3))})
				chunk = send(s.seller, s.cfg.Inbox, 0, append([]byte{derive.MsgChunk}, payload...)).Hash
				for i := 0; i < 3; i++ {
					send(s.buyer, s.cfg.Vault, 100_000_000, nil)
				}
				if s.provider && !executedMarketBuyerAction(s.providerAction) {
					for i := 0; i < 3; i++ {
						send(s.seller, s.cfg.Vault, 100_000_000, nil)
					}
				}
			case 8:
				if !inputs.provider {
					input := s.inputs[branch]
					native(s.buyer, &s.exchange, input.Order.Price.Big(), callData("ZVMExchange", "buy", input.ABI()))
				}
				send(s.seller, s.cfg.Inbox, 0, derive.EncodeAssemble([]znn.Hash{chunk}))
			case creditSaleHeight:
				if s.credit != nil {
					native(s.buyer, &s.exchange, s.inputs[0].Order.Price.Big(), callData("ZVMExchange", "buy", s.inputs[0].ABI()))
				}
			case creditAcceptHeight:
				if s.credit != nil {
					data, err := s.credit.abi.Pack("setRejectsNative", false)
					if s.credit.minedRevert {
						data, err = s.credit.abi.Pack("setRejectFromBlock", new(big.Int).SetUint64(creditFundingHeight))
					}
					if err != nil {
						t.Fatal(err)
					}
					native(s.buyer, &s.credit.wallet, nil, data)
				}
			}
			if validated {
				sort.Slice(dm.Blocks, func(i, j int) bool {
					a, b := dm.Blocks[i], dm.Blocks[j]
					if order := bytes.Compare(a.Address[:], b.Address[:]); order != 0 {
						return order < 0
					}
					if a.Height != b.Height {
						return a.Height < b.Height
					}
					return bytes.Compare(a.Hash[:], b.Hash[:]) < 0
				})
				dm.Momentum.Content = nil
				for _, block := range dm.Blocks {
					dm.Momentum.Content = append(dm.Momentum.Content, znn.AccountHeader{Address: block.Address, Height: block.Height, Hash: block.Hash})
				}
			}
			momentums[h] = dm
		}
		s.branches[branch] = momentums
	}
	return s
}

type executionRun struct {
	t                    *testing.T
	scenario             *executionScenario
	path                 string
	backend              *executionEngine
	web, nom             *httptest.Server
	eth                  *ethclient.Client
	engine               *l2.Engine
	db                   *store.DB
	ix                   *indexer.Indexer
	book                 *Book
	d                    *node.Deriver
	nomMu                sync.RWMutex
	branch               int
	limit                uint64
	fault, observedFault string
	callbackError        error
	processPhase         string
	clearProcessTrigger  bool
}

// Only these two read methods exist on the synthetic NoM port. The input
// blocks are declared observations, not freshly authenticated NoM blocks.
func (r *executionRun) nomReply(w http.ResponseWriter, request *http.Request) {
	r.nomMu.RLock()
	limit, selectedSource := r.limit, r.scenario.branches[r.branch]
	r.nomMu.RUnlock()
	var call struct {
		ID     json.RawMessage
		Method string
		Params []json.RawMessage
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, 4<<20)).Decode(&call); err != nil {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	var result any
	switch call.Method {
	case "ledger.getFrontierMomentum":
		result = selectedSource[limit].Momentum
	case "ledger.getDetailedMomentumsByHeight":
		var start, count uint64
		if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &start) != nil || json.Unmarshal(call.Params[1], &count) != nil || count > 64 {
			http.Error(w, "undeclared fixture range", http.StatusBadRequest)
			return
		}
		list := []*znn.DetailedMomentum{}
		for h := start; h < start+count && h <= limit; h++ {
			if dm := selectedSource[h]; dm != nil {
				list = append(list, dm)
			}
		}
		result = map[string]any{"count": len(list), "list": list}
	default:
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32601, "message": "undeclared NoM fixture operation"}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

func newExecutionRun(t *testing.T, scenario *executionScenario) *executionRun {
	t.Helper()
	r := &executionRun{t: t, scenario: scenario, path: t.TempDir(), limit: 1}
	r.nom = httptest.NewServer(http.HandlerFunc(r.nomReply))
	t.Cleanup(r.nom.Close)
	r.open()
	t.Cleanup(r.close)
	return r
}

func (r *executionRun) open() {
	r.t.Helper()
	r.callbackError = nil
	r.backend = openExecutionEngine(r.t, filepath.Join(r.path, "engine"), r.scenario.genesis, r.scenario.buffered)
	srv := rpc.NewServer()
	if err := srv.RegisterName("engine", r.backend); err != nil {
		r.t.Fatal(err)
	}
	if err := srv.RegisterName("eth", executionEth{r.backend}); err != nil {
		r.t.Fatal(err)
	}
	r.web = httptest.NewServer(srv)
	var err error
	r.eth, err = ethclient.Dial(r.web.URL)
	if err != nil {
		r.t.Fatal(err)
	}
	r.engine, err = l2.DialEngine(context.Background(), r.web.URL, [32]byte{})
	if err != nil {
		r.t.Fatal(err)
	}
	r.db, err = store.Open(filepath.Join(r.path, "node.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.db.SQL().SetMaxOpenConns(1)
	if r.clearProcessTrigger {
		// This trigger belongs only to the newly created process fixture.
		if _, err := r.db.SQL().Exec("DROP TRIGGER IF EXISTS n04_owned_process_exit"); err != nil {
			r.t.Fatal(err)
		}
	}
	if _, err := r.db.SQL().Exec("PRAGMA foreign_keys=ON"); err != nil {
		r.t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plug := nft.New(nft.Config{Apps: func() nft.Apps { return nft.Apps{Exchange: r.scenario.exchange, CollectionFactory: r.scenario.factory} }})
	var plugin indexer.Plugin = plug
	if r.processPhase == "index-before-commit" {
		plugin = ownedProcessPlugin{Plugin: plug, phase: r.processPhase}
	}
	r.ix, err = indexer.New(r.db, r.eth, r.scenario.cfg, logger, plugin)
	if err != nil {
		r.t.Fatal(err)
	}
	r.book, err = New(Config{DB: r.db.SQL(), Eth: r.eth, ChainID: r.scenario.cfg.L2ChainID, Exchange: func() common.Address { return r.scenario.exchange },
		Artifact: r.scenario.arts.MustGet("ZVMExchange"), Recheck: time.Nanosecond, Log: logger})
	if err != nil {
		r.t.Fatal(err)
	}
	r.ix.BeforeRewindCommit(r.book.RewindTx)
	if err := r.ix.ReconcileExtensionState(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	r.d = &node.Deriver{Cfg: r.scenario.cfg, DB: r.db, Eth: r.eth, Engine: r.engine, L1: znn.NewClient(r.nom.URL), Log: logger}
	r.d.OnRewind = func(number uint64) { r.callbackError = r.ix.Rewind(number) }
	if err := r.d.Init(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	if r.callbackError != nil {
		r.t.Fatal(r.callbackError)
	}
	if err := r.ix.ReconcileCanonical(context.Background(), r.d.Head()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *executionRun) close() {
	if r.web != nil {
		r.eth.Close()
		r.engine.Close()
		r.web.Close()
		r.db.Close()
		r.backend.close()
		r.web = nil
	}
}

type executionFailureLog struct {
	cancel context.CancelFunc
	failed *error
}

func (h executionFailureLog) Enabled(context.Context, slog.Level) bool { return true }
func (h executionFailureLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h executionFailureLog) WithGroup(string) slog.Handler            { return h }
func (h executionFailureLog) Handle(_ context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		record.Attrs(func(a slog.Attr) bool {
			if a.Key == "err" {
				*h.failed = fmt.Errorf("%v", a.Value.Any())
			}
			return true
		})
		h.cancel()
	}
	return nil
}

func (r *executionRun) through(height uint64, wantFailure bool) {
	r.t.Helper()
	r.nomMu.Lock()
	r.limit = height
	r.nomMu.Unlock()
	r.callbackError = nil
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var derivationError error
	r.d.Log = slog.New(executionFailureLog{cancel: cancel, failed: &derivationError})
	r.d.OnDerived = func(row *store.Derived) {
		if row.Momentum.Height == 8 && r.processPhase == "derivation-after-commit" {
			exitOwnedRecoveryProcess(r.processPhase)
		}
		if r.callbackError == nil {
			r.callbackError = r.ix.IndexBlock(ctx, row.L2Number)
		}
		if r.callbackError == nil && row.Momentum.Height == 8 && r.processPhase == "index-after-commit" {
			exitOwnedRecoveryProcess(r.processPhase)
		}
		if r.callbackError == nil {
			r.callbackError = r.book.step(ctx)
		}
		if r.callbackError == nil && row.Momentum.Height == 8 && r.processPhase == "book-after-commit" {
			exitOwnedRecoveryProcess(r.processPhase)
		}
		if r.callbackError != nil || row.Momentum.Height == height {
			cancel()
		}
	}
	err := r.d.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		r.t.Fatalf("derivation did not stop at the declared boundary: %v", err)
	}
	failure := derivationError
	if failure == nil {
		failure = r.callbackError
	}
	if wantFailure {
		commitRefusal := failure != nil && (r.fault == "derivation-commit" || r.fault == "index-commit") && strings.Contains(failure.Error(), "FOREIGN KEY constraint failed")
		if failure == nil || (!strings.Contains(failure.Error(), "declared") && !commitRefusal) {
			r.t.Fatalf("declared interruption not observed: %v", failure)
		}
		r.observedFault = failure.Error()
	} else if failure != nil || r.d.Head().Height != height {
		r.t.Fatalf("native derivation/index/book failed at %d: %v", r.d.Head().Height, failure)
	}
}

func (r *executionRun) catchUp() {
	r.t.Helper()
	head, err := r.ix.Head()
	if err != nil {
		r.t.Fatal(err)
	}
	for number := head + 1; number <= r.d.Head().L2Number; number++ {
		if err := r.ix.IndexBlock(context.Background(), number); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.book.step(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *executionRun) faultSQL(phase string) {
	r.t.Helper()
	r.fault = phase
	statements := map[string]string{
		"derivation-write": `CREATE TRIGGER execution_fault BEFORE UPDATE ON chunks WHEN NEW.consumed_at=8 BEGIN SELECT RAISE(ABORT,'declared derivation write interruption'); END`,
		"index-plugin":     `CREATE TRIGGER execution_fault BEFORE INSERT ON nft_sales WHEN NEW.block=7 BEGIN SELECT RAISE(ABORT,'declared NFT plugin interruption'); END`,
		"book-page":        `CREATE TRIGGER execution_fault BEFORE INSERT ON nft_order_meta WHEN NEW.key LIKE 'scanned:%' AND NEW.value=7 BEGIN SELECT RAISE(ABORT,'declared order book page interruption'); END`,
	}
	if phase == "derivation-commit" || phase == "index-commit" {
		for _, stmt := range []string{
			`CREATE TABLE execution_parent(id INTEGER PRIMARY KEY)`,
			`CREATE TABLE execution_child(id INTEGER REFERENCES execution_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		} {
			if _, err := r.db.SQL().Exec(stmt); err != nil {
				r.t.Fatal(err)
			}
		}
		table, column, value := "momentums", "height", 8
		if phase == "index-commit" {
			table, column, value = "blocks", "number", 7
		}
		statements[phase] = fmt.Sprintf(`CREATE TRIGGER execution_fault AFTER INSERT ON %s WHEN NEW.%s=%d BEGIN INSERT INTO execution_child VALUES(1); END`, table, column, value)
	}
	if _, err := r.db.SQL().Exec(statements[phase]); err != nil {
		r.t.Fatal(err)
	}
}

// Canonical data excludes duration and observation bookkeeping: a restart is
// allowed to advance its local epoch and recheck time. Original stored order
// inputs/hashes, chain-derived closure fields and all other tables are compared.
func (r *executionRun) state() map[string][]string {
	return r.stateFrom(r.db.SQL())
}

func (r *executionRun) stateFrom(db *sql.DB) map[string][]string {
	r.t.Helper()
	out := map[string][]string{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'execution_%' ORDER BY name`)
	if err != nil {
		r.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			r.t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			r.t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			r.t.Fatal(err)
		}
		out[table] = []string{}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range ptrs {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				r.t.Fatal(err)
			}
			item := map[string]any{}
			for i, col := range cols {
				if table == "momentums" && col == "build_ms" || table == "nft_orders" && (col == "checked_at" || col == "checked_epoch" || col == "filled_epoch") {
					continue
				}
				item[col] = values[i]
			}
			if table == "nft_order_meta" && item["key"] == "rewind_epoch" {
				continue
			}
			raw, err := json.Marshal(item)
			if err != nil {
				r.t.Fatal(err)
			}
			out[table] = append(out[table], string(raw))
		}
		if err := rows.Err(); err != nil {
			r.t.Fatal(err)
		}
		rows.Close()
		sort.Strings(out[table])
	}
	head := r.backend.chain.CurrentBlock()
	state, err := r.backend.chain.StateAt(head.Root)
	if err != nil {
		r.t.Fatal(err)
	}
	out["executed-engine"] = []string{head.Hash().Hex(), head.Root.Hex(), head.ReceiptHash.Hex(), head.TxHash.Hex()}
	for _, account := range []common.Address{common.Address(r.scenario.seller), common.Address(r.scenario.buyer)} {
		out["executed-balances"] = append(out["executed-balances"], account.Hex()+":"+state.GetBalance(account).ToBig().String())
	}
	return out
}

func assertExecutionState(t *testing.T, got, want map[string][]string) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	for table, expected := range want {
		if !reflect.DeepEqual(got[table], expected) {
			t.Errorf("canonical %s differs\ngot  %v\nwant %v", table, got[table], expected)
		}
	}
	if len(got) != len(want) {
		t.Error("canonical table inventory differs")
	}
	t.FailNow()
}

func (r *executionRun) assertExpected() {
	r.assertExpectedFrom(r.db.SQL())
}

func (r *executionRun) assertExpectedFrom(db *sql.DB) {
	r.t.Helper()
	s := r.scenario
	head := r.backend.chain.CurrentBlock()
	state, err := r.backend.chain.StateAt(head.Root)
	if err != nil {
		r.t.Fatal(err)
	}
	for account, amount := range map[common.Address]string{common.Address(s.seller): "250000000000000000", common.Address(s.buyer): "2750000000000000000"} {
		if got := state.GetBalance(account).ToBig().String(); got != amount {
			r.t.Fatalf("executed balance %s = %s, want %s", account, got, amount)
		}
	}
	for id := int64(1); id <= 3; id++ {
		owner := common.Address(s.seller)
		if id == 1 || id == 3 {
			owner = common.Address(s.buyer)
		}
		data, err := s.arts.MustGet("ZVMCollection").Call("ownerOf", big.NewInt(id))
		if err != nil {
			r.t.Fatal(err)
		}
		raw, err := (executionEth{r.backend}).Call(context.Background(), executionCall{To: s.token, Input: data}, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber))
		if err != nil || len(raw) != 32 || common.BytesToAddress(raw) != owner {
			r.t.Fatalf("executed ownerOf(%d) disagrees: %x, %v", id, raw, err)
		}
		var indexed string
		if err := db.QueryRow("SELECT owner FROM nft_tokens WHERE collection=? AND token_id=?", s.token.Hex(), fmt.Sprint(id)).Scan(&indexed); err != nil || indexed != owner.Hex() {
			r.t.Fatalf("indexed ownerOf(%d) = %s: %v", id, indexed, err)
		}
	}
	for i, order := range s.orders {
		var status, input string
		var filled int
		if err := db.QueryRow("SELECT status,filled,input FROM nft_orders WHERE hash=?", order.Hash().Hex()).Scan(&status, &filled, &input); err != nil {
			r.t.Fatal(err)
		}
		if i == 0 && (status != StatusFilled || filled != 1) || i == 1 && (status != StatusOpen || filled != 0) {
			r.t.Fatalf("executed order %d has status %s and fill %d", i, status, filled)
		}
		var original Input
		if err := json.Unmarshal([]byte(input), &original); err != nil || original.Order.Hash() != order.Hash() || original.Kind != KindOnChain || len(original.Signature) != 0 {
			r.t.Fatal("original unsigned on-chain order input changed")
		}
	}
	var chunks, consumed, deferred, released int
	if err := db.QueryRow("SELECT count(*),sum(consumed_at=8) FROM chunks").Scan(&chunks, &consumed); err != nil || chunks != 1 || consumed != 1 {
		r.t.Fatalf("chunk consumption differs: %d/%d, %v", chunks, consumed, err)
	}
	if err := db.QueryRow("SELECT count(*),sum(released_at=8) FROM deferred").Scan(&deferred, &released); err != nil || deferred != 1 || released != 1 {
		r.t.Fatalf("deferred release differs: %d/%d, %v", deferred, released, err)
	}
	for n := uint64(1); n <= 8; n++ {
		block := r.backend.chain.GetBlockByNumber(n)
		if block == nil {
			r.t.Fatal("executed canonical block missing")
		}
		for _, receipt := range r.backend.chain.GetReceiptsByHash(block.Hash()) {
			if receipt.Status != types.ReceiptStatusSuccessful {
				r.t.Fatalf("executed native transaction reverted in block %d", n)
			}
		}
	}
}

func TestExecutedNativeRecoveryConverges(t *testing.T) {
	scenario := newExecutionScenario(t)
	reference := newExecutionRun(t, scenario)
	reference.through(7, false)
	prefix := reference.state()
	reference.through(9, false)
	reference.assertExpected()
	want := reference.state()
	for _, phase := range []string{"reopen", "engine-commit-reply", "derivation-write", "derivation-commit", "index-plugin", "index-commit", "book-page", "engine-behind", "replacement-branch"} {
		t.Run(phase, func(t *testing.T) {
			r := newExecutionRun(t, scenario)
			r.through(7, false)
			assertExecutionState(t, r.state(), prefix)
			switch phase {
			case "engine-commit-reply":
				r.backend.mu.Lock()
				r.backend.lostReply = 7
				r.backend.mu.Unlock()
				r.through(8, true)
			case "derivation-write", "derivation-commit", "index-plugin", "index-commit", "book-page":
				r.faultSQL(phase)
				r.through(8, true)
				if _, err := r.db.SQL().Exec("DROP TRIGGER execution_fault"); err != nil {
					t.Fatal(err)
				}
			case "replacement-branch":
				r.nomMu.Lock()
				r.branch = 1
				r.nomMu.Unlock()
				r.through(9, false)
				if reflect.DeepEqual(r.state()["executed-engine"], want["executed-engine"]) {
					t.Fatal("competing branch did not change executed state")
				}
				r.nomMu.Lock()
				r.branch = 0
				r.nomMu.Unlock()
			case "engine-behind":
				r.through(9, false)
				if _, err := r.backend.chain.SetCanonical(r.backend.chain.GetBlockByNumber(6)); err != nil {
					t.Fatal(err)
				}
			}
			r.close()
			r.open()
			r.catchUp()
			if r.d.Head().Height < 9 || phase == "replacement-branch" {
				r.through(9, false)
			}
			r.assertExpected()
			assertExecutionState(t, r.state(), want)
			// A second reopen checks persistence and idempotence after recovery.
			r.close()
			r.open()
			r.catchUp()
			r.assertExpected()
			assertExecutionState(t, r.state(), want)
		})
	}
}
