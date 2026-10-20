package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/devnet"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

type RunConfig struct {
	Rollup     *rollup.Config
	Eth        *ethclient.Client
	Submit     *ethclient.Client
	L1         *znn.Client
	Deployment *Deployment
	APIBase    string
	Scenario   string
	Lane       string
	Wallets    int
	Rate       float64
	Duration   time.Duration
	Containers []string
	Log        *slog.Logger

	// Custom runs a workload defined outside this package — the application
	// contracts — through the same harness. Scenario then only names it.
	Custom *Scenario
	// Artifacts to build calls from; nil loads the benchmark's own.
	Artifacts *Artifacts
	// WalletBase (EVM) and NativeBase (Zenon) choose the key range; zero keeps
	// the defaults. NativeLimit is how many native keys the L1 has funded and
	// fused from NativeBase; zero with the default base is the genesis count.
	WalletBase  uint32
	NativeBase  uint32
	NativeLimit int
	// Fund is the ZVM balance each wallet is topped up with when it holds less
	// than a fifth of it; nil is 50 ZNN.
	Fund *big.Int
	// MinBalance is a balance every wallet must have before the run, for a
	// phase that knows what its calls will cost and cannot say so through the
	// scenario — a withdrawal sends a whole ZNN and reserves gas on top, and a
	// fifth of the funding is not enough for that. Measured 2026-09-17: 787 of
	// 1,000 withdrawals refused "insufficient funds" because their wallets
	// held 0.6–1.2 ZNN, over the threshold and under the cost.
	MinBalance *big.Int
	// Workers bounds concurrent submissions; zero is min(wallets, 512).
	Workers int
	// Drain is how long to wait for the last transactions after submission
	// stops; zero is three minutes.
	Drain time.Duration
}

type Record struct {
	Wallet     int    `json:"wallet"`
	Hash       string `json:"hash"`
	Nom        string `json:"nom,omitempty"`
	SubmitMS   int64  `json:"submitMs"`
	IncludedMS int64  `json:"includedMs,omitempty"`
	Block      uint64 `json:"block,omitempty"`
	BlockTime  int64  `json:"blockTime,omitempty"`
	GasUsed    uint64 `json:"gasUsed,omitempty"`
	Status     uint64 `json:"status"`
	// Receipt says Status was actually read. Without it, a block whose
	// receipts could not be fetched left every record at status 0 and the
	// summary called them reverted: 910 phantom reverts in one 10,000-wallet
	// phase, all of them 0x1 on chain (measured 2026-09-17).
	Receipt bool   `json:"receipt,omitempty"`
	Error   string `json:"error,omitempty"`
}

type BlockStat struct {
	Number    uint64 `json:"number"`
	Timestamp int64  `json:"timestamp"`
	Txs       int    `json:"txs"`
	GasUsed   uint64 `json:"gasUsed"`
	ZvmBytes  int    `json:"zvmBytes"`
	NomBlocks int    `json:"nomBlocks"`
	Messages  int    `json:"messages"`
	BuildMs   int    `json:"buildMs"`
	BaseFee   string `json:"baseFee"`
}

type Sample struct {
	At     string            `json:"at"`
	CPU    map[string]string `json:"cpu"`
	Mem    map[string]string `json:"mem"`
	DiskKB map[string]int64  `json:"diskKb"`
}

type Result struct {
	Scenario      string         `json:"scenario"`
	Lane          string         `json:"lane"`
	Wallets       int            `json:"wallets"`
	TargetRate    float64        `json:"targetRate"`
	Duration      string         `json:"duration"`
	StartedAt     time.Time      `json:"startedAt"`
	EndedAt       time.Time      `json:"endedAt"`
	PrepareMS     int64          `json:"prepareMs,omitempty"`
	StartBlock    uint64         `json:"startBlock"`
	EndBlock      uint64         `json:"endBlock"`
	Records       []Record       `json:"records"`
	Blocks        []BlockStat    `json:"blocks"`
	RelayerBefore map[string]any `json:"relayerBefore,omitempty"`
	RelayerAfter  map[string]any `json:"relayerAfter,omitempty"`
	Samples       []Sample       `json:"samples,omitempty"`
	Summary       Summary        `json:"summary"`
}

func (r *Result) Save(path string) error {
	raw, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Scenario describes one workload.
type Scenario struct {
	Name string
	// Gas is the limit every call declares; zero estimates each one (see
	// callGas), which is what an application workload must do.
	Gas uint64
	// MinGas is the least an estimated call declares. An estimate is taken
	// against the chain as it is when the call is built, and a wallet whose
	// previous call has not landed yet is estimated against a state it will not
	// run in: the farm's harvest looks like a no-op until the stake before it
	// lands, then pays a reward. Measured 2026-09-17: estimates of 51 k and
	// 78 k, padded to 67 k and 101 k, for calls that used up to 127 k — 409 of
	// 599 reverted out of gas. A floor at the worst case keeps the estimate
	// wherever it is higher.
	MinGas uint64
	// Build returns wallet w's next call. w.Seq counts the calls it has built.
	Build func(d *Deployment, arts *Artifacts, w *Wallet) (to *common.Address, value *big.Int, data []byte, err error)
	// NeedsToken airdrops the benchmark ERC-20; NeedsApproval approves the
	// router for it from every wallet.
	NeedsToken    bool
	NeedsApproval bool
	// Prepare runs once the wallets are funded, before load starts.
	Prepare func(ctx context.Context, cfg RunConfig, wallets []*Wallet) error
}

// Wallet is one sender. A wallet is used by one worker at a time, so its
// nonce and its Seq need no lock.
type Wallet struct {
	Index  int
	Addr   common.Address
	Seq    int
	Sender *Sender      // batch lane
	Acct   *znn.Account // native lane
}

func scenarios(arts *Artifacts) map[string]*Scenario {
	erc20 := arts.MustGet("BenchERC20")
	erc721 := arts.MustGet("BenchERC721")
	erc1155 := arts.MustGet("BenchERC1155")
	churn := arts.MustGet("StorageChurn")
	router := arts.MustGet("UniswapV2Router02")

	return map[string]*Scenario{
		"transfer": {Name: "transfer", Gas: 21_000, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			to := common.BigToAddress(big.NewInt(int64(0x1000 + (w.Index*7+w.Seq)%512)))
			return &to, big.NewInt(1_000_000_000), nil, nil
		}},
		// Every transfer pays an address that has never existed, so each one
		// adds an account to the state: the worst case for state growth.
		"transfer-new": {Name: "transfer to a new address", Gas: 21_000, Build: TransferNew},
		"erc20": {Name: "erc20 transfer", Gas: 70_000, NeedsToken: true, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			to := common.BigToAddress(big.NewInt(int64(0x2000 + (w.Index*7+w.Seq)%512)))
			data, err := erc20.Call("transfer", to, big.NewInt(1_000))
			token := d.ERC20
			return &token, new(big.Int), data, err
		}},
		"erc721": {Name: "erc721 mint", Gas: 140_000, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			data, err := erc721.Call("mint", w.Addr)
			addr := d.ERC721
			return &addr, new(big.Int), data, err
		}},
		"erc1155": {Name: "erc1155 mint", Gas: 120_000, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			data, err := erc1155.Call("mint", w.Addr, big.NewInt(int64(w.Index%16)), big.NewInt(5))
			addr := d.ERC1155
			return &addr, new(big.Int), data, err
		}},
		"swap": {Name: "uniswap v2 swap", Gas: 260_000, NeedsToken: true, NeedsApproval: true, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			path := []common.Address{d.ERC20, d.TokenB}
			data, err := router.Call("swapExactTokensForTokens",
				big.NewInt(1_000_000_000_000), new(big.Int), path, w.Addr, big.NewInt(time.Now().Add(time.Hour).Unix()))
			addr := d.Router
			return &addr, new(big.Int), data, err
		}},
		"churn": {Name: "storage churn (20 slots)", Gas: 600_000, Build: func(d *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			data, err := churn.Call("churn", big.NewInt(20))
			addr := d.Churn
			return &addr, new(big.Int), data, err
		}},
		"deploy": {Name: "erc20 deploy", Gas: 1_400_000, Build: func(d *Deployment, a *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
			init, err := a.MustGet("BenchERC20").Deploy(fmt.Sprintf("Bench %d-%d", w.Index, w.Seq), "TMP", big.NewInt(1_000_000))
			return nil, new(big.Int), init, err
		}},
	}
}

// TransferNew pays 1 gwei to an address that has never existed.
func TransferNew(_ *Deployment, _ *Artifacts, w *Wallet) (*common.Address, *big.Int, []byte, error) {
	to := FreshAddress(w.Addr, w.Seq)
	return &to, big.NewInt(1_000_000_000), nil, nil
}

// FreshAddress is an address nobody holds a key for, unique per sender and
// sequence number.
func FreshAddress(from common.Address, seq int) common.Address {
	return common.BytesToAddress(crypto.Keccak256([]byte(fmt.Sprintf("zvm-fresh-%s-%d", from.Hex(), seq))))
}

func ScenarioNames() []string {
	arts, err := LoadArtifacts()
	if err != nil {
		return nil
	}
	names := make([]string, 0, 8)
	for k := range scenarios(arts) {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Run executes one scenario on one lane and returns the measurements.
func Run(ctx context.Context, cfg RunConfig) (*Result, error) {
	arts := cfg.Artifacts
	if arts == nil {
		var err error
		if arts, err = LoadArtifacts(); err != nil {
			return nil, err
		}
		cfg.Artifacts = arts
	}
	sc := cfg.Custom
	if sc == nil {
		var ok bool
		if sc, ok = scenarios(arts)[cfg.Scenario]; !ok {
			return nil, fmt.Errorf("unknown scenario %q (have %s)", cfg.Scenario, strings.Join(ScenarioNames(), ", "))
		}
	}
	if cfg.Scenario == "" {
		cfg.Scenario = sc.Name
	}
	if cfg.Lane != "batch" && cfg.Lane != "native" {
		return nil, fmt.Errorf("lane must be batch or native")
	}
	limit := cfg.NativeLimit
	if limit == 0 && cfg.NativeBase == 0 {
		limit = MaxNativeWallets
	}
	if cfg.Lane == "native" && limit > 0 && cfg.Wallets > limit {
		return nil, fmt.Errorf("the L1 funds %d native wallets from index %d", limit, cfg.nativeBase())
	}

	prepStart := time.Now()
	wallets, err := cfg.prepare(ctx, arts, sc)
	if err != nil {
		return nil, fmt.Errorf("preparing wallets: %w", err)
	}

	startBlock, err := cfg.Eth.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Scenario: cfg.Scenario, Lane: cfg.Lane, Wallets: cfg.Wallets, TargetRate: cfg.Rate,
		Duration: cfg.Duration.String(), StartedAt: time.Now().UTC(), StartBlock: startBlock + 1,
		PrepareMS: time.Since(prepStart).Milliseconds(),
	}
	res.RelayerBefore = fetchStatus(cfg.APIBase, "relayer")

	runCtx, cancel := context.WithTimeout(ctx, cfg.Duration)
	defer cancel()

	var (
		mu      sync.Mutex
		records = map[string]*Record{} // by hash, for the watcher
		all     []*Record              // in submission order, failures included
	)
	addRecord := func(r *Record) {
		mu.Lock()
		if r.Hash != "" {
			records[r.Hash] = r
		}
		all = append(all, r)
		mu.Unlock()
	}

	// Inclusion watcher: follow blocks and stamp the transactions we sent.
	watchCtx, stopWatch := context.WithCancel(ctx)
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		cfg.watchBlocks(watchCtx, startBlock, &mu, records)
	}()

	sampleCtx, stopSamples := context.WithCancel(ctx)
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		cfg.sample(sampleCtx, res)
	}()

	// One rate limiter for every worker, and a queue of idle wallets: a worker
	// takes a tick, then the wallet that has waited longest, so load spreads
	// round-robin across however many wallets there are.
	var ticks <-chan time.Time
	if cfg.Rate > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / cfg.Rate))
		defer t.Stop()
		ticks = t.C
	}
	free := make(chan *Wallet, len(wallets))
	for _, w := range wallets {
		free <- w
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = min(len(wallets), 512)
	}
	if sc.Build == nil {
		workers = 0 // funding only
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg.worker(runCtx, arts, sc, free, ticks, addRecord)
		}()
	}
	wg.Wait()
	res.EndedAt = time.Now().UTC()

	// Let the last transactions land before stopping the watcher.
	drain := cfg.Drain
	if drain == 0 {
		drain = 3 * time.Minute
	}
	deadline := time.Now().Add(drain)
	for time.Now().Before(deadline) {
		mu.Lock()
		pending := 0
		for _, r := range all {
			if r.IncludedMS == 0 && r.Error == "" {
				pending++
			}
		}
		mu.Unlock()
		if pending == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	stopWatch()
	watchWG.Wait()
	stopSamples()
	sampleWG.Wait()

	endBlock, err := cfg.Eth.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	res.EndBlock = endBlock
	res.RelayerAfter = fetchStatus(cfg.APIBase, "relayer")
	mu.Lock()
	for _, r := range all {
		res.Records = append(res.Records, *r)
	}
	mu.Unlock()
	res.Blocks = cfg.blockStats(res.StartBlock, endBlock)
	res.Summary = Summarize(res)
	return res, nil
}

func (cfg RunConfig) nativeBase() uint32 {
	if cfg.NativeBase != 0 {
		return cfg.NativeBase
	}
	return WalletNomBase
}

func (cfg RunConfig) prepare(ctx context.Context, arts *Artifacts, sc *Scenario) ([]*Wallet, error) {
	n := cfg.Wallets
	wallets := make([]*Wallet, n)
	base := cfg.WalletBase
	if base == 0 {
		base = WalletEVMBase
	}
	var feeCap *big.Int
	if cfg.Lane == "batch" {
		head, err := cfg.Eth.HeaderByNumber(ctx, nil)
		if err != nil {
			return nil, err
		}
		// Sized to the chain's own fee market: past the market fork a burst can
		// raise the base fee faster than a flat multiple covers, and a wallet
		// whose cap is passed stops landing — which a benchmark would record as
		// the chain being slow rather than the harness being under-capped.
		feeCap = cfg.Rollup.SuggestedFeeCap(head.BaseFee, senderTip)
	}
	err := Parallel(ctx, n, 64, func(i int) error {
		w := &Wallet{Index: i}
		if cfg.Lane == "batch" {
			key := devnet.EVMKey(base + uint32(i))
			from := crypto.PubkeyToAddress(key.PublicKey)
			nonce, err := startNonce(ctx, cfg, from)
			if err != nil {
				return err
			}
			w.Sender = NewSenderAt(key, cfg.Eth, cfg.Submit, cfg.Rollup.L2ChainID, nonce, feeCap)
			w.Addr = from
		} else {
			key := devnet.Key(cfg.nativeBase() + uint32(i))
			acct := znn.NewAccount(key, cfg.L1, cfg.Rollup.L1ChainID)
			acct.AssumeFused = true
			w.Acct, w.Addr = acct, common.Address(key.Address)
		}
		wallets[i] = w
		return nil
	})
	if err != nil {
		return nil, err
	}
	addrs := make([]common.Address, n)
	for i, w := range wallets {
		addrs[i] = w.Addr
	}

	funder, err := NewSender(ctx, devnet.EVMKey(FunderEVMKey), cfg.Eth, cfg.Submit, cfg.Rollup)
	if err != nil {
		return nil, err
	}
	perWallet := cfg.Fund
	if perWallet == nil {
		perWallet = new(big.Int).Mul(big.NewInt(50), big.NewInt(1e18))
	}
	threshold := new(big.Int).Div(perWallet, big.NewInt(5))
	if cfg.MinBalance != nil && cfg.MinBalance.Cmp(threshold) > 0 {
		threshold = cfg.MinBalance
		if twice := new(big.Int).Lsh(cfg.MinBalance, 1); twice.Cmp(perWallet) > 0 {
			perWallet = twice
		}
	}
	// A wallet must also hold what its calls reserve. A node refuses a
	// transaction whose sender cannot cover gas × fee cap + value — the cap,
	// not the price it would pay — and derivation then leaves it out. Measured
	// 2026-09-17 on chain 70: wallets prepared while the base fee was 836 gwei
	// signed with a 4,300 gwei cap, 0.09 ZNN a transfer; 269 of them held
	// 0.079 ZNN, above the flat fifth-of-funding threshold, so were not topped
	// up, and 502 transfers were refused nine times and discarded. So the
	// threshold is at least what the calls this run expects of each wallet
	// reserve, and a top-up leaves twice that.
	if feeCap != nil && sc.Build != nil {
		gas := sc.Gas
		if gas == 0 {
			gas = max(sc.MinGas, 300_000)
		}
		calls := int64(math.Ceil(cfg.Rate*cfg.Duration.Seconds()/float64(max(n, 1)))) + 2
		need := new(big.Int).Mul(feeCap, new(big.Int).SetUint64(gas))
		need.Mul(need, big.NewInt(calls))
		if need.Cmp(threshold) > 0 {
			threshold = need
		}
		if twice := new(big.Int).Lsh(need, 1); twice.Cmp(perWallet) > 0 {
			perWallet = twice
		}
	}
	short := make([]bool, n)
	if err := Parallel(ctx, n, 64, func(i int) error {
		bal, err := cfg.Eth.BalanceAt(ctx, addrs[i], nil)
		if err != nil {
			return err
		}
		short[i] = bal.Cmp(threshold) < 0
		return nil
	}); err != nil {
		return nil, err
	}
	var needy []common.Address
	for i, s := range short {
		if s {
			needy = append(needy, addrs[i])
		}
	}
	// Gas per recipient is generous on purpose: paying a brand-new address
	// costs 25,000 for the account itself on top of the 9,000 transfer and the
	// 2,600 cold access, and an underestimate here reverts the whole funding
	// transaction rather than one recipient's share. 400 recipients was 20 M
	// gas, which fit before Karst (D3): EIP-7825 now caps one transaction at
	// 16,777,216, so the chunk has to fit under whatever this chain's live
	// MaxTxGas is rather than a constant sized for the old, higher ceiling.
	const perRecipientGas = 50_000
	fundingChunk := 400
	// Dynamic plasma prices data, and a disperse call is data: 331 recipients
	// is ~10.7 KB, 728 k base plasma, and at the 9x fusion price chain 71
	// reaches under this very load that is 6.5 M of an account's 10.5 M — one
	// block in flight, so funding crawls at a block a momentum. Smaller chunks
	// cost more calls and far less plasma each, which that ruleset rewards.
	if m, err := cfg.L1.FrontierMomentum(ctx); err == nil && m != nil && m.NextFusionPrice != nil {
		fundingChunk = 64
	}
	if cap := cfg.Rollup.MaxTxGas; cap > 200_000 {
		if n := int((cap - 200_000) / perRecipientGas); n < fundingChunk {
			fundingChunk = n
		}
	}
	var txs []*types.Transaction
	for i := 0; i < len(needy); i += fundingChunk {
		chunk := needy[i:min(i+fundingChunk, len(needy))]
		data, err := arts.MustGet("Disperse").Call("disperse", chunk, perWallet)
		if err != nil {
			return nil, err
		}
		value := new(big.Int).Mul(perWallet, big.NewInt(int64(len(chunk))))
		tx, err := funder.Send(ctx, &cfg.Deployment.Disperse, value, data, uint64(200_000+len(chunk)*perRecipientGas))
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	if sc.NeedsToken {
		amount := new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e18))
		for i := 0; i < len(addrs); i += fundingChunk {
			chunk := addrs[i:min(i+fundingChunk, len(addrs))]
			data, err := arts.MustGet("BenchERC20").Call("airdrop", chunk, amount)
			if err != nil {
				return nil, err
			}
			tx, err := funder.Send(ctx, &cfg.Deployment.ERC20, new(big.Int), data, uint64(200_000+len(chunk)*perRecipientGas))
			if err != nil {
				return nil, err
			}
			txs = append(txs, tx)
		}
	}
	if len(txs) > 0 {
		cfg.Log.Info("funding benchmark wallets", "transactions", len(txs), "wallets", n, "short", len(needy))
		if _, err := waitAll(ctx, cfg.Eth, txs, 10*time.Minute); err != nil {
			return nil, err
		}
	}

	if sc.NeedsApproval {
		cfg.Log.Info("approving the router from every wallet", "wallets", n)
		data, err := arts.MustGet("BenchERC20").Call("approve", cfg.Deployment.Router,
			new(big.Int).Mul(big.NewInt(1e12), big.NewInt(1e18)))
		if err != nil {
			return nil, err
		}
		var last *types.Transaction
		for _, w := range wallets {
			if cfg.Lane == "batch" {
				tx, err := w.Sender.Send(ctx, &cfg.Deployment.ERC20, new(big.Int), data, 80_000)
				if err != nil {
					return nil, err
				}
				last = tx
			} else {
				// Through PublishCall like every other native publisher: on a
				// chain that prices native calls, a bare send burns nothing and
				// so buys no gas, and the approval never happens.
				call := derive.Call{To: &cfg.Deployment.ERC20, Value: new(big.Int), Gas: 80_000, Data: data}
				if _, _, err := derive.PublishCall(ctx, cfg.Rollup, w.Acct, znn.ZNN, call); err != nil {
					return nil, err
				}
			}
		}
		if last != nil {
			if _, err := WaitReceipt(ctx, cfg.Eth, last, 5*time.Minute); err != nil {
				return nil, err
			}
		} else {
			time.Sleep(45 * time.Second) // native approvals: give derivation a moment
		}
	}
	if sc.Prepare != nil {
		if err := sc.Prepare(ctx, cfg, wallets); err != nil {
			return nil, err
		}
	}
	return wallets, nil
}

func (cfg RunConfig) worker(ctx context.Context, arts *Artifacts, sc *Scenario, free chan *Wallet,
	ticks <-chan time.Time, add func(*Record)) {
	for {
		if ticks != nil {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
			}
		} else if ctx.Err() != nil {
			return
		}
		var w *Wallet
		select {
		case <-ctx.Done():
			return
		case w = <-free:
		}
		// select picks at random when both are ready: a wallet taken as the
		// window closes goes back unused.
		if ctx.Err() != nil {
			free <- w
			return
		}
		cfg.send(ctx, arts, sc, w, add)
		free <- w
	}
}

func (cfg RunConfig) send(ctx context.Context, arts *Artifacts, sc *Scenario, w *Wallet, add func(*Record)) {
	to, value, data, err := sc.Build(cfg.Deployment, arts, w)
	submitted := time.Now()
	if err != nil {
		add(&Record{Wallet: w.Index, SubmitMS: submitted.UnixMilli(), Error: err.Error()})
		return
	}
	w.Seq++
	gas, err := cfg.callGas(ctx, sc, w.Addr, to, value, data)
	if err != nil {
		if ctx.Err() == nil {
			add(&Record{Wallet: w.Index, SubmitMS: submitted.UnixMilli(), Error: err.Error()})
		}
		return
	}
	if cfg.Lane == "batch" {
		tx, err := w.Sender.Send(ctx, to, value, data, gas)
		if err != nil {
			if ctx.Err() == nil {
				add(&Record{Wallet: w.Index, SubmitMS: submitted.UnixMilli(), Error: err.Error()})
			}
			return
		}
		add(&Record{Wallet: w.Index, Hash: tx.Hash().Hex(), SubmitMS: submitted.UnixMilli()})
		return
	}
	call := derive.Call{To: to, Value: value, Gas: gas, Data: data}
	blk, tx, err := derive.PublishCall(ctx, cfg.Rollup, w.Acct, znn.ZNN, call)
	if err != nil {
		if ctx.Err() == nil {
			add(&Record{Wallet: w.Index, SubmitMS: submitted.UnixMilli(), Error: err.Error()})
		}
		return
	}
	add(&Record{Wallet: w.Index, Hash: tx.Hash().Hex(), Nom: blk.Hash.String(), SubmitMS: submitted.UnixMilli()})
}

// callGas is the limit a call declares: the scenario's fixed gas, or an
// estimate padded by 30 % and never below the scenario's floor.
//
// Both lanes estimate. A native call used to pass the scenario's zero straight
// through, which on a chain that prices native calls is refused before it is
// published — "a call must declare its gas" — so every native application
// workload failed once the burned fee was switched on (D9). The estimate is
// made as the call's L2 sender: for a native call that is the Zenon address
// folded to 20 bytes, which is w.Addr.
func (cfg RunConfig) callGas(ctx context.Context, sc *Scenario, from common.Address, to *common.Address, value *big.Int, data []byte) (uint64, error) {
	if sc.Gas != 0 {
		return sc.Gas, nil
	}
	est, err := cfg.Eth.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data})
	if err != nil {
		return 0, fmt.Errorf("estimating gas: %w", err)
	}
	gas := max(est*13/10, sc.MinGas)
	limit := cfg.Rollup.MaxTxGas
	if cfg.Lane != "batch" && cfg.Rollup.MaxNativeGas != 0 {
		limit = cfg.Rollup.MaxNativeGas
	}
	if limit != 0 && gas > limit {
		gas = max(est, min(gas, limit))
	}
	return gas, nil
}

func (cfg RunConfig) watchBlocks(ctx context.Context, from uint64, mu *sync.Mutex, records map[string]*Record) {
	next := from + 1
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		head, err := cfg.Eth.BlockNumber(ctx)
		if err != nil {
			continue
		}
		for ; next <= head; next++ {
			blk, err := cfg.Eth.BlockByNumber(ctx, new(big.Int).SetUint64(next))
			if err != nil {
				break
			}
			seen := time.Now().UnixMilli()
			found := 0
			mu.Lock()
			for _, tx := range blk.Transactions() {
				if r, ok := records[tx.Hash().Hex()]; ok && r.IncludedMS == 0 {
					r.IncludedMS = seen
					r.Block = blk.NumberU64()
					r.BlockTime = int64(blk.Time())
					found++
				}
			}
			mu.Unlock()
			if found == 0 {
				continue
			}
			// Retried, because giving up here is not neutral: the records keep
			// status 0 and are reported as reverts that never happened.
			var receipts []*types.Receipt
			for attempt := 0; attempt < 3; attempt++ {
				if receipts, err = cfg.Eth.BlockReceipts(ctx, rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(next))); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
			}
			if err != nil {
				cfg.Log.Warn("reading a block's receipts", "block", next, "err", err)
				continue
			}
			mu.Lock()
			for _, rc := range receipts {
				if r, ok := records[rc.TxHash.Hex()]; ok {
					r.GasUsed, r.Status, r.Receipt = rc.GasUsed, rc.Status, true
				}
			}
			mu.Unlock()
		}
	}
}

// startNonce is the nonce a wallet should begin a run at: what the **relayer**
// will accept next, not what the chain says.
//
// The relayer is this chain's mempool, so it is the only thing that knows what
// it is still holding for a sender. A nonce read from the execution client
// counts only what has been derived — and `pending` there is no better, because
// these transactions never enter its pool, they go to NoM. A wallet that starts
// from the chain's nonce while the relayer still holds work for it picks a
// nonce that is already queued, and the submission is refused as an underpriced
// replacement. That silently cost three steps of the 2026-09-16 matrix: 239
// such errors on erc721, 173 on erc1155, 2,520 of 2,999 included.
//
// Falls back to the chain when there is no relayer to ask (the native lane, or
// a run pointed straight at the node).
func startNonce(ctx context.Context, cfg RunConfig, addr common.Address) (uint64, error) {
	return senderStartNonce(ctx, cfg.Eth, cfg.Submit, addr)
}

// senderStartNonce is startNonce for callers that hold the two clients rather
// than a RunConfig.
func senderStartNonce(ctx context.Context, eth, submit *ethclient.Client, addr common.Address) (uint64, error) {
	if submit != nil {
		if n, err := submit.PendingNonceAt(ctx, addr); err == nil {
			return n, nil
		}
	}
	return eth.NonceAt(ctx, addr, nil)
}

// runningOnly keeps the containers docker actually has. The caller's list may
// name both execution clients, because either may be the one running (D2), and
// only one of them exists on any given stack.
func runningOnly(ctx context.Context, want []string) []string {
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return want
	}
	have := map[string]bool{}
	for _, n := range strings.Fields(string(out)) {
		have[n] = true
	}
	var keep []string
	for _, w := range want {
		if w = strings.TrimSpace(w); have[w] {
			keep = append(keep, w)
		}
	}
	return keep
}

// sample records container CPU, memory and disk while the run is on.
func (cfg RunConfig) sample(ctx context.Context, res *Result) {
	if len(cfg.Containers) == 0 {
		return
	}
	// The list names both execution clients (D2), and `docker stats` fails the
	// whole call on one name it does not have — which would silently drop
	// every sample rather than the missing container.
	names := runningOnly(ctx, cfg.Containers)
	if len(names) == 0 {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		s := Sample{At: time.Now().UTC().Format(time.RFC3339), CPU: map[string]string{}, Mem: map[string]string{}, DiskKB: map[string]int64{}}
		args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}"}, names...)
		if out, err := exec.CommandContext(ctx, "docker", args...).Output(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				parts := strings.Split(strings.TrimSpace(line), "|")
				if len(parts) == 3 {
					s.CPU[parts[0]] = parts[1]
					s.Mem[parts[0]] = parts[2]
				}
			}
		}
		// znnd keeps its ledger in the home directory; every other container
		// measured here — either execution client included — uses /data.
		for _, name := range names {
			path := "/data"
			if strings.HasPrefix(name, "zvm-l1-") {
				path = "/root/.znn"
			}
			out, err := exec.CommandContext(ctx, "docker", "exec", name, "du", "-sk", path).Output()
			if err != nil {
				continue
			}
			fields := strings.Fields(string(out))
			if len(fields) > 0 {
				var kb int64
				fmt.Sscan(fields[0], &kb)
				s.DiskKB[name] = kb
			}
		}
		res.Samples = append(res.Samples, s)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// blockStats reads the explorer's per-block view, which carries what each
// block cost on NoM.
func (cfg RunConfig) blockStats(from, to uint64) []BlockStat {
	if cfg.APIBase == "" || to < from {
		return nil
	}
	var out []BlockStat
	before := to + 1
	for before > from {
		url := fmt.Sprintf("%s/api/blocks?limit=200&before=%d", cfg.APIBase, before)
		resp, err := http.Get(url)
		if err != nil {
			return out
		}
		var rows []struct {
			Number    uint64 `json:"number"`
			Timestamp int64  `json:"timestamp"`
			UserTxs   int    `json:"userTxs"`
			GasUsed   uint64 `json:"gasUsed"`
			ZvmBytes  int    `json:"zvmBytes"`
			NomBlocks int    `json:"nomBlocks"`
			Messages  int    `json:"messages"`
			BuildMs   int    `json:"buildMs"`
			BaseFee   string `json:"baseFee"`
		}
		err = json.NewDecoder(resp.Body).Decode(&rows)
		resp.Body.Close()
		if err != nil || len(rows) == 0 {
			return out
		}
		for _, r := range rows {
			if r.Number < from {
				continue
			}
			out = append(out, BlockStat{Number: r.Number, Timestamp: r.Timestamp, Txs: r.UserTxs, GasUsed: r.GasUsed,
				ZvmBytes: r.ZvmBytes, NomBlocks: r.NomBlocks, Messages: r.Messages, BuildMs: r.BuildMs, BaseFee: r.BaseFee})
		}
		before = rows[len(rows)-1].Number
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func fetchStatus(apiBase, service string) map[string]any {
	if apiBase == "" {
		return nil
	}
	resp, err := http.Get(apiBase + "/api/info")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil
	}
	if v, ok := info[service].(map[string]any); ok {
		return v
	}
	return nil
}
