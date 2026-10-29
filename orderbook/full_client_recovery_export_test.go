//go:build fullclient_recovery

package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/ethclient"
	gethnode "github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// FullClientRecoveryCLIFixture owns the actual pinned op-geth service, catalyst
// Engine API, JWT listener and eth API. Only the NoM observation port is synthetic.
// A public test-only P2P key avoids key generation; no peer socket, dial, discovery,
// account backend, wallet, transaction submission API or public endpoint is used.
type FullClientRecoveryCLIFixture struct {
	*RecoveryCLIFixture
	client            *gethnode.Node
	ethURL, engineURL string
	engineDirectory   os.FileInfo
}

func NewFullClientRecoveryCLIFixture(t *testing.T, seedMessagePasser bool) *FullClientRecoveryCLIFixture {
	t.Helper()
	scenario := newExecutionScenarioProfile(t, true, false)
	path := t.TempDir()
	if seedMessagePasser {
		// Explicitly different synthetic genesis, solely for storage/reopen
		// qualification. Do not change shipped genesis or repair any RPC payload.
		// The empty shipped genesis has its own required refusal control below.
		account := scenario.genesis.Alloc[params.OptimismL2ToL1MessagePasser]
		if len(account.Code) == 0 || len(account.Storage) != 0 {
			t.Fatal("expected the shipped empty MessagePasser account")
		}
		account.Storage = map[common.Hash]common.Hash{common.HexToHash("0x01"): common.HexToHash("0x01")}
		scenario.genesis.Alloc[params.OptimismL2ToL1MessagePasser] = account
		scenario.cfg.L2GenesisHash = scenario.genesis.ToBlock().Hash()
	}
	r := &executionRun{t: t, scenario: scenario, path: path, limit: 7}
	r.nom = httptest.NewServer(http.HandlerFunc(r.nomReply))
	f := &FullClientRecoveryCLIFixture{RecoveryCLIFixture: &RecoveryCLIFixture{r: r}}
	t.Cleanup(func() {
		f.CloseExecutionClient()
		if f.read != nil {
			f.read.Close()
		}
		r.nom.Close()
	})
	write := func(name string, value []byte) {
		if err := os.WriteFile(filepath.Join(path, name), value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(scenario.cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("rollup.json", raw)
	loaded, err := rollup.Load(filepath.Join(path, "rollup.json"))
	if err != nil || loaded.L2GenesisHash != scenario.genesis.ToBlock().Hash() {
		t.Fatalf("full-client synthetic configuration did not round-trip: %v", err)
	}
	scenario.cfg = loaded
	write("jwt.hex", []byte(strings.Repeat("0", 64)))
	raw, err = json.Marshal(map[string]any{"chainId": scenario.cfg.L2ChainID, "exchange": scenario.exchange, "collectionFactory": scenario.factory})
	if err != nil {
		t.Fatal(err)
	}
	write("apps.json", raw)
	uri := url.URL{Scheme: "file", Path: filepath.Join(path, "node.db")}
	q := uri.Query()
	q.Set("mode", "ro")
	uri.RawQuery = q.Encode()
	f.read, err = sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	f.read.SetMaxOpenConns(1)
	f.openExecutionClient()
	return f
}

func (f *FullClientRecoveryCLIFixture) openExecutionClient() {
	t := f.r.t
	t.Helper()
	if f.client != nil {
		t.Fatal("owned full client is already open")
	}
	config := gethnode.DefaultConfig
	config.Name = "zvm-owned-recovery"
	config.DataDir = filepath.Join(f.Directory(), "full-client")
	config.IPCPath = ""
	config.HTTPHost, config.HTTPPort = "127.0.0.1", 0
	config.HTTPModules = []string{"eth"}
	config.HTTPVirtualHosts = []string{"127.0.0.1", "localhost"}
	config.AuthAddr, config.AuthPort = "127.0.0.1", 0
	config.AuthVirtualHosts = []string{"127.0.0.1", "localhost"}
	config.JWTSecret = filepath.Join(f.Directory(), "jwt.hex")
	// Public scalar one is a test transport identity, never an account backend.
	key, err := crypto.HexToECDSA(strings.Repeat("0", 63) + "1")
	if err != nil {
		t.Fatal(err)
	}
	config.P2P = p2p.Config{PrivateKey: key, NoDial: true, NoDiscovery: true, MaxPeers: 0}
	stack, err := gethnode.New(&config)
	if err != nil {
		t.Fatal(err)
	}
	f.client = stack // Every later failure now has an owned close path.
	options := ethconfig.Defaults
	// FullSync avoids waiting for absent peers; persistence/cache/history values
	// remain the pinned service defaults, including the automatic state scheme.
	options.Genesis, options.SyncMode = f.r.scenario.genesis, ethconfig.FullSync
	options.RollupDisableTxPoolAdmission, options.RollupDisableTxPoolGossip = true, true
	if options.DatabaseCache != 2048 || options.TrieCleanCache != 614 || options.TrieDirtyCache != 1024 ||
		options.SnapshotCache != 409 || options.NoPruning || options.StateScheme != "" ||
		options.TrieTimeout != 60*time.Minute {
		t.Fatal("pinned full-client persistence defaults changed")
	}
	service, err := eth.New(stack, &options)
	if err != nil {
		t.Fatal(err)
	}
	// Match the pinned CLI's RegisterFilterAPI assembly. The real API uses
	// service subscriptions, which the normal stack/chain close path closes.
	filterSystem := filters.NewFilterSystem(service.APIBackend, filters.Config{
		LogCacheSize:  options.FilterLogCacheSize,
		LogQueryLimit: options.LogQueryLimit,
		RangeLimit:    options.RangeLimit,
	})
	stack.RegisterAPIs([]rpc.API{{Namespace: "eth", Service: filters.NewFilterAPI(filterSystem)}})
	if err := catalyst.Register(stack, service); err != nil {
		t.Fatal(err)
	}
	if err := stack.Start(); err != nil {
		t.Fatal(err)
	}
	service.SetSynced()
	if service.BlockChain().TrieDB().Scheme() != rawdb.PathScheme || service.ArchiveMode() {
		t.Fatal("full service did not select its default buffered path state")
	}
	if len(stack.AccountManager().Wallets()) != 0 {
		t.Fatal("owned full-client fixture acquired a wallet")
	}
	info, err := os.Stat(config.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if f.engineDirectory != nil && !os.SameFile(f.engineDirectory, info) {
		t.Fatal("owned full-client directory identity changed across reopen")
	}
	f.engineDirectory = info
	f.ethURL, f.engineURL = stack.HTTPEndpoint(), stack.HTTPAuthEndpoint()
	f.r.eth, err = ethclient.Dial(f.ethURL)
	if err != nil {
		t.Fatal(err)
	}
	// Direct chain access is confined to the existing read-only state oracle.
	// All production CLI derivation, indexing and book calls use the real APIs.
	f.r.backend = &executionEngine{chain: service.BlockChain()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := f.r.eth.ChainID(ctx)
	if err != nil || id.Uint64() != f.r.scenario.cfg.L2ChainID {
		t.Fatalf("actual full-client chain ID differs: %v", err)
	}
	genesisHash := f.r.scenario.cfg.L2GenesisHash
	logs, err := f.r.eth.FilterLogs(ctx, ethereum.FilterQuery{BlockHash: &genesisHash})
	if err != nil || len(logs) != 0 {
		t.Fatalf("actual full-client genesis log API is unavailable or unexpected: %v", err)
	}
	t.Log("full_client_defaults db_cache_mib=2048 trie_clean_mib=614 trie_dirty_mib=1024 snapshot_mib=409 state_scheme=path archive=false engine_api=catalyst authenticated=true")
}

func (f *FullClientRecoveryCLIFixture) CloseExecutionClient() {
	f.r.t.Helper()
	if f.r.eth != nil {
		f.r.eth.Close()
		f.r.eth = nil
	}
	if f.client != nil {
		err := f.client.Close()
		f.client, f.r.backend = nil, nil
		if err != nil {
			f.r.t.Errorf("owned full-client close failed: %v", err)
		}
		runtime.GC() // Sequential profiles never retain several default caches.
	}
}

// RestartExecutionClient must be called only after its exact CLI child joined.
// Graceful service closure exercises its real disk journal and reopen path. It
// is not a kernel-kill, daemon-process crash or hardware power-loss experiment.
func (f *FullClientRecoveryCLIFixture) RestartExecutionClient() {
	f.r.t.Helper()
	f.CloseExecutionClient()
	if f.r.t.Failed() {
		f.r.t.FailNow()
	}
	f.openExecutionClient()
}

func (f *FullClientRecoveryCLIFixture) Args() []string {
	path := func(name string) string { return filepath.Join(f.Directory(), name) }
	return []string{"-config", path("rollup.json"), "-jwt", path("jwt.hex"), "-db", path("node.db"),
		"-l1", f.r.nom.URL, "-engine", f.engineURL, "-eth", f.ethURL, "-apps", path("apps.json"),
		"-listen=", "-verify=false", "-prices=off", "-mainnet-rpc=off"}
}

func (f *FullClientRecoveryCLIFixture) Snapshot() map[string][]string {
	f.r.t.Helper()
	f.r.assertExpectedFrom(f.read)
	return f.r.stateFrom(f.read)
}

// AssertFullClientEmptyMessagePasserRefused freezes the real pinned-client
// limitation. It does not normalize or replace the payload returned by catalyst.
func AssertFullClientEmptyMessagePasserRefused(t *testing.T) {
	t.Helper()
	f := NewFullClientRecoveryCLIFixture(t, false)
	defer f.CloseExecutionClient()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	e, err := l2.DialEngine(ctx, f.engineURL, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	genesis := f.r.scenario.genesis.ToBlock()
	hash, root, gas := genesis.Hash(), common.Hash{}, f.r.scenario.cfg.GasLimit
	fc := engine.ForkchoiceStateV1{HeadBlockHash: hash, SafeBlockHash: hash, FinalizedBlockHash: hash}
	attrs := &engine.PayloadAttributes{Timestamp: genesis.Time() + 10, Withdrawals: []*types.Withdrawal{},
		BeaconRoot: &root, NoTxPool: true, Transactions: [][]byte{}, GasLimit: &gas,
		EIP1559Params: l2.ExtraData(f.r.scenario.cfg)[1:]}
	built, err := e.ForkchoiceUpdated(ctx, fc, attrs)
	if err != nil || built == nil || built.PayloadStatus.Status != engine.VALID || built.PayloadID == nil {
		t.Fatalf("actual full client did not build the empty-root control: %v, %v", built, err)
	}
	envelope, err := e.GetPayload(ctx, *built.PayloadID)
	if err != nil || envelope == nil || envelope.ExecutionPayload == nil {
		t.Fatalf("actual full-client payload is missing: %v", err)
	}
	if envelope.ExecutionPayload.WithdrawalsRoot != nil {
		t.Fatal("pinned empty MessagePasser encoding changed; review this compatibility gate")
	}
	status, err := e.NewPayload(ctx, envelope.ExecutionPayload, root)
	if err != nil || status == nil || status.Status != engine.INVALID || status.ValidationError == nil ||
		*status.ValidationError != "nil withdrawalsRoot post-Isthmus" {
		t.Fatalf("expected the exact unmodified-client empty-root refusal: %v, %v", status, err)
	}
	if f.r.backend.chain.CurrentBlock().Hash() != hash {
		t.Fatal("refused full-client payload advanced the canonical head")
	}
	t.Log("actual_catalyst_empty_root_refusal=true payload_rewritten=false shipped_genesis_supported=false")
}
