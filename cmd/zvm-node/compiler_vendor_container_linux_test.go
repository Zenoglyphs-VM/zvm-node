package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/params"
)

//go:embed testdata/compiler_vendor_builds.json
var compilerVendorManifest []byte

type compilerVendorBuild struct {
	Path        string `json:"path"`
	Version     string `json:"version"`
	LongVersion string `json:"longVersion"`
	SHA256      string `json:"sha256"`
}

// Stream one explicit read-only vendor file to this fresh bounded tmpfs cache.
// A matching filename or release list is never accepted instead of its bytes.
func compilerVendorSeed(t *testing.T, build compilerVendorBuild, dir string) int64 {
	t.Helper()
	path := "/qualified/vendor-" + build.Version
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 32<<20 {
		t.Fatal("pinned vendor input is absent, nonregular or oversized")
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal("pinned vendor input could not be opened")
	}
	defer input.Close()
	output, err := os.OpenFile(filepath.Join(dir, "solc-"+build.LongVersion), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal("fresh vendor cache entry could not be created")
	}
	h := sha256.New()
	count, copyErr := io.CopyBuffer(io.MultiWriter(output, h), io.LimitReader(input, (32<<20)+1), make([]byte, 32<<10))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || count != info.Size() || count > 32<<20 || hex.EncodeToString(h.Sum(nil)) != build.SHA256 {
		t.Fatal("actual vendor bytes do not match the declared production pin")
	}
	return count
}

func compilerVendorOutput(t *testing.T, raw []byte, version string) (string, string) {
	t.Helper()
	var output struct {
		Errors []struct {
			Severity string `json:"severity"`
		} `json:"errors"`
		Contracts map[string]map[string]struct {
			ABI      json.RawMessage `json:"abi"`
			Metadata string          `json:"metadata"`
			EVM      struct {
				Bytecode struct {
					Object string `json:"object"`
				} `json:"bytecode"`
				DeployedBytecode struct {
					Object string `json:"object"`
				} `json:"deployedBytecode"`
			} `json:"evm"`
		} `json:"contracts"`
	}
	if len(raw) == 0 || len(raw) > 64<<10 || json.Unmarshal(raw, &output) != nil {
		t.Fatal("actual standard-JSON response is absent or invalid")
	}
	for _, diagnostic := range output.Errors {
		if diagnostic.Severity == "error" {
			t.Fatal("real vendor rejected the declared source")
		}
	}
	contract, ok := output.Contracts["Qualification.sol"]["VendorQualification"]
	if !ok {
		t.Fatal("actual vendor contract output is missing")
	}
	var metadata struct {
		Compiler struct {
			Version string `json:"version"`
		} `json:"compiler"`
	}
	if json.Unmarshal([]byte(contract.Metadata), &metadata) != nil || metadata.Compiler.Version != version {
		t.Fatal("actual compiler metadata revision differs from the selected vendor")
	}
	contractABI, err := abi.JSON(bytes.NewReader(contract.ABI))
	if err != nil {
		t.Fatal("actual vendor ABI is invalid")
	}
	method, ok := contractABI.Methods["value"]
	if !ok || len(contractABI.Methods) != 1 || len(method.Inputs) != 0 || len(method.Outputs) != 1 || method.Outputs[0].Type.String() != "uint256" || method.StateMutability != "pure" {
		t.Fatal("actual vendor ABI differs from the declared contract")
	}
	initCode, err := hex.DecodeString(contract.EVM.Bytecode.Object)
	if err != nil || len(initCode) == 0 || len(initCode) > 16<<10 {
		t.Fatal("vendor creation bytecode is invalid or oversized")
	}
	runtimeCode, err := hex.DecodeString(contract.EVM.DeployedBytecode.Object)
	if err != nil || len(runtimeCode) == 0 || len(runtimeCode) > 16<<10 {
		t.Fatal("vendor runtime bytecode is invalid or oversized")
	}
	db, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	chain := *params.MergedTestChainConfig
	chain.ChainID = big.NewInt(7340470)
	cfg := &runtime.Config{State: db, GasLimit: 1_000_000, Value: new(big.Int), ChainConfig: &chain, BlockNumber: big.NewInt(1), Time: 1_800_000_000}
	deployed, address, _, err := runtime.Create(initCode, cfg)
	if err != nil || !bytes.Equal(deployed, runtimeCode) {
		t.Fatal("actual vendor creation code did not install its declared runtime")
	}
	input, err := contractABI.Pack("value")
	if err != nil {
		t.Fatal(err)
	}
	returned, _, err := runtime.Call(address, input, cfg)
	if err != nil {
		t.Fatal("actual vendor runtime call failed")
	}
	values, err := contractABI.Unpack("value", returned)
	if err != nil || len(values) != 1 {
		t.Fatal("actual vendor runtime returned an invalid ABI value")
	}
	number, ok := values[0].(*big.Int)
	if !ok || number.Cmp(big.NewInt(7)) != 0 {
		t.Fatal("actual vendor runtime did not return the declared value")
	}
	if _, _, err := runtime.Call(address, []byte{0xff, 0xff, 0xff, 0xff}, cfg); err == nil {
		t.Fatal("undeclared vendor ABI selector succeeded")
	}
	initSum, runtimeSum := sha256.Sum256(initCode), sha256.Sum256(runtimeCode)
	return hex.EncodeToString(initSum[:]), hex.EncodeToString(runtimeSum[:])
}

func TestCompilerVendorContainerProfile(t *testing.T) {
	if os.Getenv("ZVM_N01_VENDOR_PROFILE") != "1" {
		t.Skip("requires the explicit pinned real-vendor compiler kernel profile")
	}
	if os.Getenv("ZVM_N01_SOLCD_BINARY") != "/qualified/zvm-node" {
		t.Fatal("declared production binary is missing")
	}
	compilerProfileKernel(t)
	before := compilerProfileOOM(t)
	var builds []compilerVendorBuild
	decoder := json.NewDecoder(bytes.NewReader(compilerVendorManifest))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&builds) != nil || decoder.Decode(new(any)) != io.EOF || len(builds) != 3 {
		t.Fatal("embedded vendor manifest is invalid")
	}
	want := []string{"0.5.16+commit.9c3226ce", "0.6.6+commit.6c089d02", "0.8.30+commit.73712a01"}
	dir := t.TempDir()
	var retained int64
	sizes := map[string]int64{}
	for index, build := range builds {
		if build.LongVersion != want[index] || build.Path != "solc-linux-amd64-v"+build.LongVersion || !strings.HasPrefix(build.LongVersion, build.Version+"+commit.") || len(build.SHA256) != 64 {
			t.Fatal("selected production vendor manifest changed")
		}
		size := compilerVendorSeed(t, build, dir)
		sizes[build.LongVersion] = size
		retained += size
	}
	if retained > 48<<20 {
		t.Fatal("selected vendor cache exceeds its declared byte budget")
	}
	var lists, downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/linux-amd64/list.json" {
			lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": builds})
			return
		}
		downloads.Add(1)
		http.Error(w, "unexpected fixture request", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	fixture := &compilerProfileFixture{t: t, dir: dir, url: server.URL}
	process := fixture.startBudgeted("/qualified/zvm-node", 3, 48<<20, 10*time.Second)
	client := &verify.RemoteRunner{URL: process.url, MaxOutputBytes: 64 << 10}
	source := "pragma solidity >=0.5.0 <0.9.0; contract VendorQualification { function value() external pure returns (uint256) { return 7; } }"
	input, err := json.Marshal(map[string]any{"language": "Solidity", "sources": map[string]any{"Qualification.sol": map[string]string{"content": source}},
		"settings": map[string]any{"evmVersion": "byzantium", "outputSelection": map[string]any{"*": map[string]any{"*": []string{"abi", "metadata", "evm.bytecode.object", "evm.deployedBytecode.object"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, build := range builds {
		t.Run(build.Version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			raw, err := client.Compile(ctx, build.LongVersion, input)
			if err != nil {
				t.Fatal("actual pinned vendor compile failed", err)
			}
			initHash, runtimeHash := compilerVendorOutput(t, raw, build.LongVersion)
			outputHash := sha256.Sum256(raw)
			compilerProfileRecord(t, map[string]any{"case": "actual_vendor_standard_json", "version": build.Version, "long_version": build.LongVersion,
				"binary_sha256": build.SHA256, "binary_bytes": sizes[build.LongVersion], "response_sha256": hex.EncodeToString(outputHash[:]),
				"creation_bytecode_sha256": initHash, "runtime_bytecode_sha256": runtimeHash, "same_Node_RemoteRunner_and_production_loader": true,
				"actual_vendor_standard_JSON_compiled": true, "compiler_metadata_version_matches": true, "actual_creation_runtime_match": true,
				"in_memory_EVM_returned_7": true, "undeclared_selector_reverted": true, "service_binary_downloads": downloads.Load(),
				"live_chain_or_real_wallet_qualified": false, "full_vendor_workloads_or_hostile_descendants_qualified": false})
		})
	}
	process.stop(t)
	if lists.Load() != 1 || downloads.Load() != 0 {
		t.Fatal("vendor compiles did not reuse one release list and the original pinned cache")
	}
	after := compilerProfileOOM(t)
	if after["oom"] != before["oom"] || after["oom_kill"] != before["oom_kill"] {
		t.Fatal("real vendor profile caused an OOM event")
	}
	compilerProfileRecord(t, map[string]any{"case": "compiler_vendor_kernel_profile", "actual_vendor_versions": 3, "actual_compiler_invocations": 3,
		"actual_production_service_lifetimes": 1, "joined_services": 1, "retained_cache_bytes": retained, "cache_byte_limit": 48 << 20, "cache_file_limit": 3,
		"release_list_requests": 1, "service_binary_downloads": 0, "memory_max_bytes": 512 << 20, "swap_max_bytes": 0, "CPUs": 2, "pids_max": 64, "tmpfs_bytes": 64 << 20,
		"uid": 10001, "gid": 10001, "read_only_root": true, "network": "none", "effective_capabilities_zero": true, "no_new_privileges": true, "OOM_events": 0,
		"current_production_service_and_empty_compiler_environment_used": true, "broad_operator_platform_or_per_invocation_containment_qualified": false})
}
