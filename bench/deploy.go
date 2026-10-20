package bench

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// Dev key indexes the benchmark uses. They are public devnet keys.
const (
	FunderEVMKey     = 900  // deploys everything and funds the wallets
	WalletEVMBase    = 1000 // relayed-lane wallets
	WalletNomBase    = devnet.BenchNative
	MaxNativeWallets = devnet.NumBenchNative
)

// Deployment is what `zvm bench setup` leaves behind.
type Deployment struct {
	ChainID  uint64         `json:"chainId"`
	Funder   common.Address `json:"funder"`
	ERC20    common.Address `json:"erc20"`
	TokenB   common.Address `json:"tokenB"`
	ERC721   common.Address `json:"erc721"`
	ERC1155  common.Address `json:"erc1155"`
	Churn    common.Address `json:"storageChurn"`
	Disperse common.Address `json:"disperse"`
	// WZNN is the ERC-20 wrapper for native ZNN. The JSON key stays "weth"
	// because deployment files already written name it that, and the node
	// reads those files to find the DEX.
	WETH     common.Address `json:"weth"`
	Factory  common.Address `json:"uniswapFactory"`
	Router   common.Address `json:"uniswapRouter"`
	Pair     common.Address `json:"uniswapPair"`
	Deployed time.Time      `json:"deployedAt"`
}

func LoadDeployment(path string) (*Deployment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d := new(Deployment)
	return d, json.Unmarshal(raw, d)
}

// Sender signs and submits transactions for one EVM key, tracking its own
// nonce so a burst can go out without waiting.
type Sender struct {
	Key    *ecdsa.PrivateKey
	From   common.Address
	Eth    *ethclient.Client
	Submit *ethclient.Client
	Signer types.Signer
	Tip    *big.Int
	FeeCap *big.Int
	nonce  uint64
}

var senderTip = rollup.DefaultTip()

func NewSender(ctx context.Context, key *ecdsa.PrivateKey, eth, submit *ethclient.Client, cfg *rollup.Config) (*Sender, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	// The relayer, not the chain: it is this chain's mempool, so only it knows
	// what it is still holding for this sender. See startNonce in run.go — a
	// chain nonce here is what failed the swap step's wallet preparation with
	// "replacement transaction underpriced".
	nonce, err := senderStartNonce(ctx, eth, submit, from)
	if err != nil {
		return nil, err
	}
	head, err := eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Room for the base fee to move while a burst is in flight, sized to this
	// chain's own parameters rather than a flat multiple.
	feeCap := cfg.SuggestedFeeCap(head.BaseFee, senderTip)
	return NewSenderAt(key, eth, submit, cfg.L2ChainID, nonce, feeCap), nil
}

// NewSenderAt builds a sender whose nonce and fee cap are already known, so a
// hundred thousand of them cost one head read rather than one each.
func NewSenderAt(key *ecdsa.PrivateKey, eth, submit *ethclient.Client, chainID, nonce uint64, feeCap *big.Int) *Sender {
	return &Sender{
		Key: key, From: crypto.PubkeyToAddress(key.PublicKey), Eth: eth, Submit: submit,
		Signer: types.LatestSignerForChainID(new(big.Int).SetUint64(chainID)),
		Tip:    senderTip,
		FeeCap: feeCap,
		nonce:  nonce,
	}
}

func (s *Sender) Nonce() uint64 { return s.nonce }

func (s *Sender) Send(ctx context.Context, to *common.Address, value *big.Int, data []byte, gas uint64) (*types.Transaction, error) {
	if gas == 0 {
		est, err := s.Eth.EstimateGas(ctx, ethereum.CallMsg{From: s.From, To: to, Value: value, Data: data})
		if err != nil {
			return nil, fmt.Errorf("estimating gas: %w", err)
		}
		gas = est * 13 / 10
	}
	tx, err := types.SignNewTx(s.Key, s.Signer, &types.DynamicFeeTx{
		ChainID: s.Signer.ChainID(), Nonce: s.nonce, GasTipCap: s.Tip, GasFeeCap: s.FeeCap,
		Gas: gas, To: to, Value: value, Data: data,
	})
	if err != nil {
		return nil, err
	}
	if err := s.Submit.SendTransaction(ctx, tx); err != nil {
		return nil, err
	}
	s.nonce++
	return tx, nil
}

func WaitReceipt(ctx context.Context, eth *ethclient.Client, tx *types.Transaction, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := eth.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			if r.Status != types.ReceiptStatusSuccessful {
				return r, fmt.Errorf("transaction %s reverted in block %d", tx.Hash(), r.BlockNumber)
			}
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("no receipt for %s within %s", tx.Hash(), timeout)
}

func waitAll(ctx context.Context, eth *ethclient.Client, txs []*types.Transaction, timeout time.Duration) ([]*types.Receipt, error) {
	out := make([]*types.Receipt, len(txs))
	for i, tx := range txs {
		r, err := WaitReceipt(ctx, eth, tx, timeout)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

// Setup funds the deployer through the bridge if it needs it, then deploys
// the contract set.
// SetupOptions is what a caller has to decide before the contract set goes out.
type SetupOptions struct {
	// FundZNN is the ZNN the deployer is brought up to, in whole units; 0 means
	// the 200,000 that suited a chain genesised with `zvm l1-genesis`. On a chain
	// funded by hand it has to be told, or it asks the bench funder for a hundred
	// times what it holds and the whole setup fails on its first send.
	FundZNN int64
	// SwapOnly deploys what a swap needs and nothing else: WZNN, the factory and
	// the router. No BNA, no BNB, no ERC-721 or ERC-1155, no storage churn or
	// disperse, and no BNA/BNB pool.
	//
	// The reason is that a Uniswap pair is a fact on chain from the moment the
	// factory emits PairCreated, and the swap page lists pairs from the indexer
	// rather than from this file — so a benchmark token deployed once is a token
	// on the swap page forever, on a chain that cannot be re-genesised without
	// abandoning every balance. A chain that exists to *run* the ZVM wants only
	// the pairs it means to have; a chain that exists to be benchmarked wants the
	// whole set, because the scenarios need it.
	SwapOnly bool
	Log      *slog.Logger
}

func Setup(ctx context.Context, cfg *rollup.Config, eth, submit *ethclient.Client, l1 *znn.Client, opt SetupOptions) (*Deployment, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	fundZNN := opt.FundZNN
	if fundZNN == 0 {
		fundZNN = 200_000
	}
	arts, err := LoadArtifacts()
	if err != nil {
		return nil, err
	}
	key := devnet.EVMKey(FunderEVMKey)
	from := crypto.PubkeyToAddress(key.PublicKey)

	// The deployer's ZNN comes in through the vault like everyone else's.
	want := new(big.Int).Mul(big.NewInt(fundZNN), big.NewInt(1e18))
	balance, err := eth.BalanceAt(ctx, from, nil)
	if err != nil {
		return nil, err
	}
	if balance.Cmp(want) < 0 {
		log.Info("funding the deployer through the vault", "address", from, "znn", fundZNN)
		acct := znn.NewAccount(devnet.Key(devnet.BenchFunder), l1, cfg.L1ChainID)
		acct.AssumeFused = true
		units := new(big.Int).Mul(big.NewInt(fundZNN), znn.OneZNN)
		call := derive.Call{To: &from, Value: new(big.Int).Mul(units, rollup.ZNNScale), Gas: 30_000}
		blk, err := acct.Send(ctx, cfg.Vault, znn.ZNN, units, derive.EncodeCall(call))
		if err != nil {
			return nil, fmt.Errorf("funding the deployer: %w", err)
		}
		tx := derive.DepositCallTx(cfg, blk, call)
		if _, err := WaitReceipt(ctx, eth, tx, 3*time.Minute); err != nil {
			return nil, err
		}
	}

	s, err := NewSender(ctx, key, eth, submit, cfg)
	if err != nil {
		return nil, err
	}
	d := &Deployment{ChainID: cfg.L2ChainID, Funder: from, Deployed: time.Now().UTC()}
	supply := new(big.Int).Mul(big.NewInt(1_000_000_000), big.NewInt(1e18))

	// Round one: everything that depends on nothing. They go out together and
	// land in the same block, which is one momentum rather than eight.
	type job struct {
		name string
		init []byte
		out  *common.Address
	}
	initTokenA, err := arts.MustGet("BenchERC20").Deploy("ZVM Bench A", "BNA", supply)
	if err != nil {
		return nil, err
	}
	initTokenB, err := arts.MustGet("BenchERC20").Deploy("ZVM Bench B", "BNB", supply)
	if err != nil {
		return nil, err
	}
	initERC721, err := arts.MustGet("BenchERC721").Deploy()
	if err != nil {
		return nil, err
	}
	initERC1155, err := arts.MustGet("BenchERC1155").Deploy()
	if err != nil {
		return nil, err
	}
	initChurn, err := arts.MustGet("StorageChurn").Deploy()
	if err != nil {
		return nil, err
	}
	initDisperse, err := arts.MustGet("Disperse").Deploy()
	if err != nil {
		return nil, err
	}
	initWZNN, err := arts.MustGet("WZNN").Deploy()
	if err != nil {
		return nil, err
	}
	initFactory, err := arts.MustGet("UniswapV2Factory").Deploy(from)
	if err != nil {
		return nil, err
	}
	round1 := []job{
		{"BenchERC20 A", initTokenA, &d.ERC20},
		{"BenchERC20 B", initTokenB, &d.TokenB},
		{"BenchERC721", initERC721, &d.ERC721},
		{"BenchERC1155", initERC1155, &d.ERC1155},
		{"StorageChurn", initChurn, &d.Churn},
		{"Disperse", initDisperse, &d.Disperse},
		{"WZNN", initWZNN, &d.WETH},
		{"UniswapV2Factory", initFactory, &d.Factory},
	}
	if opt.SwapOnly {
		round1 = []job{
			{"WZNN", initWZNN, &d.WETH},
			{"UniswapV2Factory", initFactory, &d.Factory},
		}
	}
	var txs []*types.Transaction
	for _, j := range round1 {
		tx, err := s.Send(ctx, nil, new(big.Int), j.init, 0)
		if err != nil {
			return nil, fmt.Errorf("deploying %s: %w", j.name, err)
		}
		txs = append(txs, tx)
	}
	receipts, err := waitAll(ctx, eth, txs, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	for i, r := range receipts {
		*round1[i].out = r.ContractAddress
		log.Info("deployed", "contract", round1[i].name, "address", r.ContractAddress, "gas", r.GasUsed, "block", r.BlockNumber)
	}

	// Round two: the router needs the factory. Its init code is 22 KB, which
	// is larger than a NoM block — the relayer chunks it.
	initRouter, err := arts.MustGet("UniswapV2Router02").Deploy(d.Factory, d.WETH)
	if err != nil {
		return nil, err
	}
	routerTx, err := s.Send(ctx, nil, new(big.Int), initRouter, 6_000_000)
	if err != nil {
		return nil, err
	}
	r, err := WaitReceipt(ctx, eth, routerTx, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	d.Router = r.ContractAddress
	log.Info("deployed", "contract", "UniswapV2Router02", "address", d.Router, "gas", r.GasUsed, "block", r.BlockNumber,
		"initCodeBytes", len(initRouter))

	// Round three: approvals, then liquidity for the BNA/BNB pool. Swap-only
	// chains stop here — the only pair they get is the one `zvm dex seed` opens.
	if opt.SwapOnly {
		log.Info("swap only: WZNN, the factory and the router, and no pair",
			"weth", d.WETH, "factory", d.Factory, "router", d.Router)
		return d, nil
	}
	amount := new(big.Int).Mul(big.NewInt(100_000_000), big.NewInt(1e18))
	var approveTxs []*types.Transaction
	for _, token := range []common.Address{d.ERC20, d.TokenB} {
		data, err := arts.MustGet("BenchERC20").Call("approve", d.Router, supply)
		if err != nil {
			return nil, err
		}
		tx, err := s.Send(ctx, &token, new(big.Int), data, 80_000)
		if err != nil {
			return nil, err
		}
		approveTxs = append(approveTxs, tx)
	}
	if _, err := waitAll(ctx, eth, approveTxs, 3*time.Minute); err != nil {
		return nil, err
	}
	addLiquidity, err := arts.MustGet("UniswapV2Router02").Call("addLiquidity",
		d.ERC20, d.TokenB, amount, amount, new(big.Int), new(big.Int), from, big.NewInt(time.Now().Add(time.Hour).Unix()))
	if err != nil {
		return nil, err
	}
	lpTx, err := s.Send(ctx, &d.Router, new(big.Int), addLiquidity, 4_000_000)
	if err != nil {
		return nil, err
	}
	if _, err := WaitReceipt(ctx, eth, lpTx, 5*time.Minute); err != nil {
		return nil, err
	}
	getPair, err := arts.MustGet("UniswapV2Factory").Call("getPair", d.ERC20, d.TokenB)
	if err != nil {
		return nil, err
	}
	out, err := eth.CallContract(ctx, ethereum.CallMsg{To: &d.Factory, Data: getPair}, nil)
	if err != nil {
		return nil, err
	}
	d.Pair = common.BytesToAddress(out)
	log.Info("uniswap pair", "address", d.Pair)
	return d, nil
}

func (d *Deployment) Save(path string) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
