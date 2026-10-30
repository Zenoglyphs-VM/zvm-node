//go:build fullclient_recovery

package orderbook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
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
	"github.com/ethereum/go-ethereum/core"
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

const fullClientDefaultsProfile = "full_client_defaults db_cache_mib=2048 trie_clean_mib=614 trie_dirty_mib=1024 snapshot_mib=409 state_scheme=path archive=false engine_api=catalyst authenticated=true"

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
	return newFullClientRecoveryCLIFixture(t, seedMessagePasser, true)
}

// No full service is opened in the parent until all owned children have joined.
func NewFullClientRecoveryProcessFixture(t *testing.T) *FullClientRecoveryCLIFixture {
	return newFullClientRecoveryCLIFixture(t, true, false)
}

func newFullClientRecoveryCLIFixture(t *testing.T, seedMessagePasser, openParent bool) *FullClientRecoveryCLIFixture {
	t.Helper()
	scenario := newExecutionScenarioProfile(t, true, false)
	return newFullClientRecoveryForScenario(t, scenario, seedMessagePasser, openParent)
}

func newFullClientRecoveryForScenario(t *testing.T, scenario *executionScenario, seedMessagePasser, openParent bool) *FullClientRecoveryCLIFixture {
	t.Helper()
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
	raw, err = json.Marshal(scenario.genesis)
	if err != nil {
		t.Fatal(err)
	}
	write("full-client-genesis.json", raw)
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
	if openParent {
		f.openExecutionClient()
	}
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
	t.Log(fullClientDefaultsProfile)
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

// OwnedFullClientCLIArgs starts the actual pinned service in the exact CLI
// child. NoM remains the parent's declared fixture; no Engine reply is adapted.
func OwnedFullClientCLIArgs(t *testing.T, directory string, args []string, phase string) ([]string, func()) {
	t.Helper()
	path := filepath.Join(directory, "full-client-genesis.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		t.Fatalf("owned full-client genesis is missing, nonregular or oversized: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		t.Fatal("owned full-client genesis identity changed before reading")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	current, currentErr := os.Lstat(path)
	if readErr != nil || statErr != nil || closeErr != nil || currentErr != nil || len(raw) > 1<<20 ||
		!os.SameFile(info, after) || !os.SameFile(info, current) || int64(len(raw)) != info.Size() || after.Size() != info.Size() {
		t.Fatal("owned full-client genesis read was incomplete or changed")
	}
	var genesis core.Genesis
	if err := json.Unmarshal(raw, &genesis); err != nil {
		t.Fatal(err)
	}
	config, err := rollup.Load(filepath.Join(directory, "rollup.json"))
	if err != nil || config.L2GenesisHash != genesis.ToBlock().Hash() {
		t.Fatal("owned full-client genesis differs from its loaded configuration")
	}
	if genesis.Alloc[params.OptimismL2ToL1MessagePasser].Storage[common.HexToHash("0x01")] != common.HexToHash("0x01") {
		t.Fatal("owned process case requires the declared synthetic MessagePasser slot")
	}
	r := &executionRun{t: t, path: directory, scenario: &executionScenario{genesis: &genesis, cfg: config}}
	f := &FullClientRecoveryCLIFixture{RecoveryCLIFixture: &RecoveryCLIFixture{r: r}}
	started := false
	t.Cleanup(func() {
		f.CloseExecutionClient()
		if started {
			fmt.Println("owned_full_client_closed graceful=true")
		}
	})
	f.openExecutionClient()
	started = true
	fmt.Println("owned_full_client_started " + fullClientDefaultsProfile)
	args = append([]string(nil), args...)
	args[9], args[11] = f.engineURL, f.ethURL
	return args, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		wantHead := ownedFullClientBoundaryHead(phase)
		head, err := f.r.eth.BlockNumber(ctx)
		direct := f.r.backend.chain.CurrentBlock().Number.Uint64()
		fmt.Printf("owned_full_client_boundary_observation phase=%s expected_engine_head=%d rpc_engine_head=%d direct_engine_head=%d rpc_error=%t\n", phase, wantHead, head, direct, err != nil)
		if err != nil || head != wantHead || direct != wantHead {
			fmt.Println("owned_full_client_boundary_refused engine_head_mismatch=true")
			os.Exit(88)
		}
		if phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit" {
			ancestor, err := readOwnedRewindAncestor(directory)
			if err != nil {
				fmt.Println("owned_full_client_boundary_refused ancestor_input_invalid=true")
				os.Exit(88)
			}
			fromRPC, err := f.r.eth.HeaderByNumber(ctx, new(big.Int).SetUint64(ancestor.Number))
			fromDirect := f.r.backend.chain.GetHeaderByNumber(ancestor.Number)
			want := common.HexToHash(ancestor.Hash)
			if err != nil || fromRPC == nil || fromDirect == nil || fromRPC.Number.Uint64() != ancestor.Number || fromDirect.Number.Uint64() != ancestor.Number || fromRPC.Hash() != want || fromDirect.Hash() != want {
				fmt.Println("owned_full_client_boundary_refused ancestor_header_mismatch=true")
				os.Exit(88)
			}
			fmt.Printf("owned_full_client_rewind_ancestor phase=%s source_momentum=7 execution_block=6 rpc_and_direct_hashes_verified=true\n", phase)
		}
		fmt.Printf("owned_full_client_boundary phase=%s engine_head=%d service_responding=true actual_exit_requested=86\n", phase, wantHead)
	}
}

// CheckExecutionClientDirectory checks the same physical directory after every
// joined child and before the next open. It never changes any client/SQL state.
func (f *FullClientRecoveryCLIFixture) CheckExecutionClientDirectory() {
	f.r.t.Helper()
	info, err := os.Lstat(filepath.Join(f.Directory(), "full-client"))
	if err != nil || !info.IsDir() || f.engineDirectory != nil && !os.SameFile(f.engineDirectory, info) {
		f.r.t.Fatal("owned full-client directory is missing, replaced or linked")
	}
	f.engineDirectory = info
}

// AssertOwnedFullClientProcess validates the bounded output of the exact joined
// CLI child. Exit 86 is accepted only with its actual live Engine/eth boundary;
// successful graceful children must also prove the full service close path.
func AssertOwnedFullClientProcess(t *testing.T, output string, exit int, phase string) {
	t.Helper()
	count := func(marker string) int {
		n := 0
		for _, line := range strings.Split(output, "\n") {
			if line == marker {
				n++
			}
		}
		return n
	}
	started := count("owned_full_client_started " + fullClientDefaultsProfile)
	closed := count("owned_full_client_closed graceful=true")
	boundary := count(fmt.Sprintf("owned_full_client_boundary phase=%s engine_head=%d service_responding=true actual_exit_requested=86", phase, ownedFullClientBoundaryHead(phase)))
	index := count("owned_index_plugin_before_commit block=7 transaction_visible=true real_nft_plugin_completed=true")
	wantIndex := 0
	if exit == 86 && phase == "index-before-commit" {
		wantIndex = 1
	}
	if started != 1 || index != wantIndex || strings.Count(output, "owned_full_client_boundary phase=") != boundary || exit != 0 && exit != 86 || exit == 0 && (closed != 1 || boundary != 0) || exit == 86 && (closed != 0 || boundary != 1) {
		t.Fatalf("owned full-client lifecycle differs: exit=%d started=%d closed=%d boundary=%d output=%s", exit, started, closed, boundary, output)
	}
	if exit == 86 && (phase == "derivation-rewind-before-commit" || phase == "index-rewind-before-commit") {
		if !strings.Contains(output, "NoM reorganised; rewinding svc=deriver from=9 to=7") {
			t.Fatal("owned rewind exit did not observe the production reorg path")
		}
		ancestor := count(fmt.Sprintf("owned_full_client_rewind_ancestor phase=%s source_momentum=7 execution_block=6 rpc_and_direct_hashes_verified=true", phase))
		if ancestor != 1 || strings.Count(output, "owned_full_client_rewind_ancestor phase=") != 1 {
			t.Fatal("owned rewind lacks its independent common-ancestor header proof")
		}
		t.Logf("owned_full_client_rewind_path phase=%s declared_source_reorg_observed=true common_execution_ancestor=6 engine_head_at_exit=%d ancestor_rpc_and_direct_hashes_verified=true", phase, ownedFullClientBoundaryHead(phase))
	}
	t.Logf("owned_default_full_client_child phase=%s actual_exit=%d default_profile_verified=true graceful_close=%t engine_boundary_verified=%t index_plugin_writes_verified=%t output_complete=true colocated_cli_and_client=true", phase, exit, exit == 0, exit == 86, wantIndex == 1)
}

// Only the declared fixture trigger is changed. The index boundary is a
// child-local wrapper over the real NFT plugin, with no parent SQL mutation.
func (f *FullClientRecoveryCLIFixture) OwnedProcessTrigger(phase string, enabled bool) {
	f.r.t.Helper()
	if phase == "index-before-commit" {
		return
	}
	if phase == "derivation-before-commit" {
		f.Trigger(enabled)
		return
	}
	if phase != "book-before-commit" && phase != "derivation-rewind-before-commit" && phase != "index-rewind-before-commit" {
		f.r.t.Fatal("unexpected full-client process write boundary")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(f.Directory(), "node.db")}
	q := uri.Query()
	q.Set("mode", "rw")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		f.r.t.Fatal(err)
	}
	defer db.Close()
	statement := "DROP TRIGGER execution_cli_exit"
	if enabled {
		switch phase {
		case "book-before-commit":
			statement = `CREATE TRIGGER execution_cli_exit BEFORE INSERT ON nft_order_meta WHEN NEW.key LIKE 'scanned:%' AND NEW.value=7 BEGIN SELECT zvm_owned_cli_exit(); END`
		case "derivation-rewind-before-commit":
			statement = `CREATE TRIGGER execution_cli_exit AFTER UPDATE ON chunks WHEN OLD.consumed_at=8 AND NEW.consumed_at IS NULL BEGIN SELECT zvm_owned_cli_exit(); END`
		case "index-rewind-before-commit":
			statement = `CREATE TRIGGER execution_cli_exit AFTER UPDATE ON nft_order_meta WHEN NEW.key LIKE 'scanned:%' AND OLD.value=8 AND NEW.value=6 BEGIN SELECT zvm_owned_cli_exit(); END`
		}
	}
	if _, err := db.Exec(statement); err != nil {
		f.r.t.Fatal(err)
	}
}

// Only the declared NoM observation source changes; no canonical state is edited.
func (f *FullClientRecoveryCLIFixture) SourceBranch(branch int) {
	f.r.t.Helper()
	if branch < 0 || branch >= len(f.r.scenario.branches) {
		f.r.t.Fatal("undeclared full-client source branch")
	}
	f.r.nomMu.Lock()
	f.r.branch = branch
	f.r.nomMu.Unlock()
}

func ownedFullClientBoundaryHead(phase string) uint64 {
	switch phase {
	case "derivation-rewind-before-commit", "index-rewind-before-commit":
		// Requesting canonical ancestor 6 does not lower the live catalyst head.
		return 8
	default:
		return 7
	}
}

type ownedRewindAncestor struct {
	Momentum uint64 `json:"momentum"`
	Number   uint64 `json:"number"`
	Hash     string `json:"hash"`
}

// DeclareRewindAncestor records only a read-only committed source observation.
// This fixture input never updates engine, derivation, index or book state.
func (f *FullClientRecoveryCLIFixture) DeclareRewindAncestor() {
	f.r.t.Helper()
	var number uint64
	var source, hash string
	if err := f.read.QueryRow("SELECT l2_number,hash,l2_hash FROM momentums WHERE height=7").Scan(&number, &source, &hash); err != nil {
		f.r.t.Fatal(err)
	}
	a, b := f.r.scenario.branches[0][7], f.r.scenario.branches[1][7]
	if a == nil || b == nil || a.Momentum.Hash != b.Momentum.Hash || source != a.Momentum.Hash.String() || number != 6 || common.HexToHash(hash) == (common.Hash{}) || common.HexToHash(hash).Hex() != hash {
		f.r.t.Fatal("declared rewind ancestor differs from the committed common source")
	}
	raw, err := json.Marshal(ownedRewindAncestor{Momentum: 7, Number: number, Hash: hash})
	if err != nil {
		f.r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Directory(), "owned-rewind-ancestor.json"), raw, 0600); err != nil {
		f.r.t.Fatal(err)
	}
}

func readOwnedRewindAncestor(directory string) (ownedRewindAncestor, error) {
	var value ownedRewindAncestor
	path := filepath.Join(directory, "owned-rewind-ancestor.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1024 {
		return value, fmt.Errorf("missing, nonregular or oversized declared ancestor")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, err
	}
	opened, openErr := file.Stat()
	raw, readErr := io.ReadAll(io.LimitReader(file, 1025))
	after, statErr := file.Stat()
	closeErr := file.Close()
	current, currentErr := os.Lstat(path)
	if openErr != nil || readErr != nil || statErr != nil || closeErr != nil || currentErr != nil || len(raw) > 1024 || int64(len(raw)) != info.Size() || !os.SameFile(info, opened) || !os.SameFile(info, after) || !os.SameFile(info, current) || after.Size() != info.Size() {
		return value, fmt.Errorf("declared ancestor changed or was incompletely read")
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, err
	}
	if value.Momentum != 7 || value.Number != 6 || common.HexToHash(value.Hash) == (common.Hash{}) || common.HexToHash(value.Hash).Hex() != value.Hash {
		return value, fmt.Errorf("undeclared common ancestor")
	}
	return value, nil
}
