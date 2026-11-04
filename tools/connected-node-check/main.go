// Command connected-node-check drives the production Node binary against a
// complete OP client and declared synthetic NoM inputs, with independent oracles.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func check(ok bool, reason string) {
	if !ok {
		panic(reason)
	}
}
func jsonBytes(value any) ([]byte, error) { return json.Marshal(value) }
func digest(raw []byte) string            { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func fmtNumber(name string, number uint64) string {
	return fmt.Sprintf("offline-node/%s/%d", name, number)
}
func output(label string, value any) {
	raw, err := json.Marshal(value)
	must(err)
	prefix := "CONNECTED-NODE"
	if record, ok := value.(map[string]any); ok {
		if profile, ok := record["profile"].(string); ok && strings.HasPrefix(profile, selectedPrefix) {
			prefix = "CONSTRUCTOR-NODE"
		}
	}
	fmt.Printf("%s-%s %s\n", prefix, label, raw)
}

var depositor = common.HexToAddress("0x00000000000000000000000000000000000a11ce")

func filePin(path string) (int64, string) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 512<<20))
	must(err)
	check(n > 0 && n < 512<<20, "executable byte bound")
	return n, hex.EncodeToString(h.Sum(nil))
}
func checkNodeBinary(source string) {
	info, err := buildinfo.ReadFile("/qualified-node")
	must(err)
	check(info.Path == "github.com/Zenoglyphs-VM/zvm-node/cmd/zvm-node" && info.GoVersion == "go1.25.0", "production Node build identity differs")
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	check(settings["vcs.revision"] == source && settings["vcs.modified"] == "false", "production Node source revision differs")
	n, hash := filePin("/qualified-node")
	output("NODE-BINARY", map[string]any{"bytes": n, "sha256": hash, "source": source, "embeddedCleanSourceVerified": true, "productionCLI": true})
}
func checkAPI(ctx context.Context, node *nodeProcess, height uint64) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		node.alive()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:18080/healthz", nil)
		must(err)
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 16385))
			must(response.Body.Close())
			check(len(raw) <= 16384, "production Node health byte bound")
			var health struct {
				OK       bool   `json:"ok"`
				Stalled  bool   `json:"stalled"`
				Lag      uint64 `json:"lagMomentums"`
				Derived  uint64 `json:"derivedMomentum"`
				Frontier uint64 `json:"l1Frontier"`
			}
			if readErr == nil && json.Unmarshal(raw, &health) == nil && response.StatusCode == 200 && health.OK && !health.Stalled && health.Lag == 0 && health.Derived == height && health.Frontier == height {
				return
			}
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	panic("production Node health did not reach its exact declared frontier")
}
func waitIndex(ctx context.Context, n *nodeProcess, dir string, p *derive.Plan, number uint64, hash, expectedParent common.Hash) {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		n.alive()
		db, err := readOnlyDatabase(filepath.Join(dir, "node.db"))
		if err == nil {
			var got, parent string
			var timestamp, momentum, txs uint64
			err = db.QueryRowContext(ctx, "SELECT hash,parent,timestamp,momentum,txs FROM blocks WHERE number=?", number).Scan(&got, &parent, &timestamp, &momentum, &txs)
			if err == nil && got == hash.Hex() {
				check(parent == expectedParent.Hex() && timestamp == p.Momentum.Timestamp && momentum == p.Momentum.Height && txs == 2, "indexed source record differs")
				rows, err := db.QueryContext(ctx, "SELECT hash,idx,status FROM txs WHERE block=? ORDER BY idx", number)
				must(err)
				count := 0
				for rows.Next() {
					var h string
					var index, status uint64
					must(rows.Scan(&h, &index, &status))
					check(count < 2 && h == p.Txs[count].Tx.Hash().Hex() && index == uint64(count) && status == 1, "indexed receipt/hash/order differs")
					count++
				}
				must(rows.Err())
				must(rows.Close())
				must(db.Close())
				check(count == 2, "indexed transaction inventory differs")
				return
			}
			must(db.Close())
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	fmt.Fprint(os.Stderr, n.log.text())
	panic("production index did not reach the accepted Node head")
}

func exercise(ctx context.Context, name, root string) {
	selected := strings.HasPrefix(name, selectedPrefix)
	sourceName := strings.TrimPrefix(name, selectedPrefix)
	path := sourceName
	if sourceName == "supported-isthmus" {
		path = "l2"
	}
	raw, err := os.ReadFile("chains/" + path + "/rollup.json")
	must(err)
	cfg, err := rollup.Parse(raw)
	must(err)
	if sourceName == "supported-isthmus" {
		cfg.EVM = "isthmus"
	}
	if selected {
		cfg.PredeployState = rollup.PredeployStateConstructorsV1
	}
	g, err := l2.BuildGenesis(cfg, cfg.L2GenesisTimestamp)
	must(err)
	if sourceName == "supported-isthmus" || selected {
		cfg.L2GenesisHash = g.ToBlock().Hash()
	}
	check(g.ToBlock().Hash() == cfg.L2GenesisHash && len(g.Alloc) == 12, "source genesis differs")
	dir := filepath.Join(root, name)
	must(os.Mkdir(dir, 0700))
	genesis, err := json.Marshal(g)
	must(err)
	must(os.WriteFile(filepath.Join(dir, "genesis.json"), genesis, 0600))
	writeNodeConfig(dir, cfg)
	var secret [32]byte // Public, ephemeral fixture; not an operator credential.
	must(os.WriteFile(filepath.Join(dir, "jwt.hex"), []byte(hex.EncodeToString(secret[:])), 0600))
	model := initial(g)
	head := cfg.L2GenesisHash
	client := startClient(ctx, dir)
	defer func() { client.stop() }()
	c, err := rpc.DialContext(ctx, "http://127.0.0.1:8545")
	must(err)
	defer c.Close()
	eth := ethclient.NewClient(c)
	e, err := l2.DialEngineForConfig(ctx, "http://127.0.0.1:8551", secret, cfg)
	must(err)
	defer e.Close()
	checkGenesis(ctx, c, g)
	if selected {
		constructorCheckpoint(ctx, c, cfg, model, name, "genesis", head, 0)
	}
	unauthenticatedRefused(ctx)
	nom := startNoM(ctx, cfg, name)
	defer nom.stop()
	node := startNode(ctx, dir, nom)
	defer func() { node.stop() }()
	waitNodeReady(ctx, node, nom)
	roots := []common.Hash{}
	hashes := []common.Hash{}
	txs := 0
	for number := uint64(1); number <= 8; number++ {
		nom.frontier.Store(number)
		p := nom.plans[number]
		waitNode(ctx, node, nom, dir, p, number)
		block, err := eth.BlockByNumber(ctx, new(big.Int).SetUint64(number))
		must(err)
		check(block.NumberU64() == number && block.ParentHash() == head && block.Time() == p.Momentum.Timestamp && block.GasLimit() == cfg.GasLimit, "production Node connected block fields differ")
		attrs, err := p.Attributes()
		must(err)
		actual := block.Transactions()
		check(len(actual) == len(attrs.Transactions) && len(actual) == 2, "production Node transaction count differs")
		for i, tx := range actual {
			encoded, err := tx.MarshalBinary()
			must(err)
			check(bytes.Equal(encoded, attrs.Transactions[i]), "production Node transaction bytes/order differ")
		}
		predicted := advance(model, p, head, number, nom.mints[number])
		check(block.Root() == predicted, "independent complete Node post-state root differs")
		payload := engine.BlockToExecutableData(block, new(big.Int), nil, [][]byte{}).ExecutionPayload
		check(payload != nil && block.WithdrawalsRoot() != nil, "stored OP header commitment is missing")
		// Bind only the observed complete canonical header for this negative control.
		// The production Node built/imported the positive payload without this helper.
		payload.WithdrawalsRoot = block.WithdrawalsRoot()
		checkStored(ctx, c, payload, p, model)
		_, got, _, persistedHash, _, _, gas, _, err := readNodeHead(ctx, filepath.Join(dir, "node.db"))
		must(err)
		check(got == number && persistedHash == block.Hash().Hex() && gas == block.GasUsed(), "Node SQL and actual client head differ")
		waitIndex(ctx, node, dir, p, number, block.Hash(), block.ParentHash())
		checkAPI(ctx, node, p.Momentum.Height)
		altered := *payload
		altered.StateRoot = common.Hash{31: 1}
		bad, err := e.NewPayload(ctx, &altered, *attrs.BeaconRoot)
		must(err)
		check(bad.Status == "INVALID" || bad.Status == "INVALID_BLOCK_HASH", "altered Node payload was not refused")
		tip, err := eth.HeaderByNumber(ctx, nil)
		must(err)
		check(tip.Hash() == block.Hash(), "negative import changed canonical head")
		head = block.Hash()
		roots = append(roots, predicted)
		hashes = append(hashes, head)
		txs += len(actual)
		output("BLOCK", map[string]any{"profile": name, "number": number, "parentHash": block.ParentHash(), "blockHash": head, "stateRoot": predicted,
			"forcedTransactions": 2, "completeStateAccounts": 14, "productionNodePersisted": true, "productionIndexMatched": true, "invalidStateRootRefused": true})
		if number == 4 {
			node.stop()
			client.stop()
			client = startClient(ctx, dir)
			restored, err := eth.HeaderByNumber(ctx, nil)
			must(err)
			check(restored.Hash() == head && restored.Root == predicted, "client persisted head after restart differs")
			checkAccounts(ctx, c, model)
			node = startNode(ctx, dir, nom)
			waitNodeReady(ctx, node, nom)
			waitNode(ctx, node, nom, dir, p, number)
			waitIndex(ctx, node, dir, p, number, head, block.ParentHash())
			checkAPI(ctx, node, p.Momentum.Height)
			if selected {
				constructorCheckpoint(ctx, c, cfg, model, name, "persisted-restart", head, number)
			}
			output("RESTART", map[string]any{"profile": name, "number": number, "blockHash": head, "stateRoot": predicted, "completeStateAccounts": 14, "NodeAndClientRestarted": true, "readOnlySQLAndIndexMatched": true})
		}
	}
	check(len(roots) == 8 && txs == 16, "incomplete production Node cohort")
	if selected {
		constructorCheckpoint(ctx, c, cfg, model, name, "final", head, 8)
	}
	node.stop()
	client.stop()
	record := map[string]any{"profile": name, "rules": cfg.EVM, "chainId": cfg.L2ChainID, "sourceRollupSha256": digest(raw), "genesisHash": cfg.L2GenesisHash,
		"connectedBlocks": 8, "exactPlannedTransactions": 16, "completeStateRoots": roots, "blockHashes": hashes, "completeStateAccountChecks": 126,
		"receiptChecks": 16, "readOnlyNodeRecords": 8, "indexedBlocks": 8, "indexedTransactions": 16, "persistedNodeAndClientRestarts": 1,
		"invalidPayloadRefusals": 8, "unauthenticatedEngineRefusals": 1, "productionNodeCLIStartsAndJoins": 2, "nodeCancellationExit": 1,
		"actualProductionNodeAndCompleteClientExecuted": true, "indexEnabled": true, "outboundPricesAndCompilerDisabled": true, "NoMCanonicalityOrReleaseAcceptance": false}
	if selected {
		selectedRaw, err := json.Marshal(cfg)
		must(err)
		record["genesisStateRoot"] = g.ToBlock().Root()
		record["predeployState"] = cfg.PredeployState
		record["selectedRollupSha256"] = digest(selectedRaw)
		record["constructorCheckpoints"] = 3
		record["anchoredConstructorAccountChecks"] = 12
		record["constructorStatusSlotReads"] = 3
		record["constructorGetterChecks"] = 9
		record["explicitCanonicalBlockHashAnchors"] = true
	}
	output("PROFILE", record)
}
func main() {
	check(len(os.Args) == 3 && len(os.Args[2]) == 40, "one fresh directory and exact Node source required")
	verifySandbox()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	version, err := exec.CommandContext(ctx, "/usr/local/bin/op-reth", "--version").CombinedOutput()
	must(err)
	check(len(version) < 16<<10 && strings.Contains(string(version), "2.6.1") && strings.Contains(string(version), "e16bffa"), "image version differs")
	n, hash := filePin("/usr/local/bin/op-reth")
	check(n == 101744960 && hash == "3dc4a63aa83daa5ee015386b5e961ce09d01bb54da309af8401ccb4739cfdbc1", "pinned client executable differs")
	output("BINARY", map[string]any{"version": string(version), "sha256": hash, "bytes": n, "independentBuildProvenance": false})
	checkNodeBinary(os.Args[2])
	root := os.Args[1]
	must(os.Mkdir(root, 0700))
	for _, name := range []string{"supported-isthmus", "l2", "l2-69", "l2-dp"} {
		exercise(ctx, name, root)
	}
	fmt.Println("CONNECTED-NODE-COMPLETE profiles=4 connectedBlocks=32 exactPlannedTransactions=64 completeStateRoots=32 indexedBlocks=32 persistedNodeAndClientRestarts=4 invalidPayloadRefusals=32 productionAccepted=false")
	for _, name := range []string{"supported-isthmus", "l2", "l2-69", "l2-dp"} {
		exercise(ctx, selectedPrefix+name, root)
	}
	fmt.Println("CONSTRUCTOR-NODE-COMPLETE profiles=4 connectedBlocks=32 exactPlannedTransactions=64 completeStateRoots=32 indexedBlocks=32 persistedNodeAndClientRestarts=4 invalidPayloadRefusals=32 checkpoints=12 anchoredConstructorAccounts=48 statusSlotReads=12 getters=36 productionAccepted=false")
}
