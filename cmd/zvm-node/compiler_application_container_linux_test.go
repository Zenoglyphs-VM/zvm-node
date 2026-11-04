package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zenoglyphs-VM/zvm-node/apps"
	"github.com/Zenoglyphs-VM/zvm-node/verify"
	"github.com/ethereum/go-ethereum/accounts/abi"
)

//go:embed testdata/compiler_application_manifest.json
var compilerApplicationManifest []byte

type compilerApplicationSource struct {
	Unit   string `json:"unit"`
	Origin string `json:"origin"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type compilerApplicationTarget struct {
	Name                string          `json:"name"`
	ABI                 string          `json:"abiSha256"`
	Creation            string          `json:"creationSha256"`
	Runtime             string          `json:"runtimeTemplateSha256"`
	EVM                 string          `json:"evmSha256"`
	StorageLayout       string          `json:"storageLayoutSha256"`
	Metadata            string          `json:"metadataSha256"`
	ImmutableReferences json.RawMessage `json:"immutableReferences"`
	LinkReferences      json.RawMessage `json:"linkReferences"`
}

type compilerApplicationExport struct {
	compilerApplicationTarget
	ArtifactPath string `json:"artifact_path"`
}

type compilerApplicationGroup struct {
	Name        string                      `json:"name"`
	Version     string                      `json:"version"`
	Sources     []compilerApplicationSource `json:"sources"`
	Settings    json.RawMessage             `json:"settings"`
	InputSHA256 string                      `json:"input_sha256"`
	InputBytes  int                         `json:"input_bytes"`
	Targets     []compilerApplicationTarget `json:"targets"`
	Exports     []compilerApplicationExport `json:"exports"`
}

type compilerApplicationArtifact struct {
	ABI      json.RawMessage `json:"abi"`
	Metadata string          `json:"metadata"`
	EVM      json.RawMessage `json:"evm"`
	Layout   json.RawMessage `json:"storageLayout"`
}

func compilerApplicationHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func compilerApplicationCanonicalHash(t *testing.T, raw []byte) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("compiler application JSON material is invalid")
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		t.Fatal("compiler application JSON material could not be canonicalized")
	}
	return compilerApplicationHash(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
}

func compilerApplicationCode(t *testing.T, raw json.RawMessage) ([]byte, []byte) {
	t.Helper()
	var evm struct {
		Creation struct {
			Object string `json:"object"`
		} `json:"bytecode"`
		Runtime struct {
			Object string `json:"object"`
		} `json:"deployedBytecode"`
	}
	if json.Unmarshal(raw, &evm) != nil || evm.Creation.Object != strings.ToLower(evm.Creation.Object) || evm.Runtime.Object != strings.ToLower(evm.Runtime.Object) {
		t.Fatal("compiler application EVM material is invalid")
	}
	creation, first := hex.DecodeString(evm.Creation.Object)
	runtime, second := hex.DecodeString(evm.Runtime.Object)
	if first != nil || second != nil {
		t.Fatal("compiler application has unresolved or invalid executable bytes")
	}
	return creation, runtime
}

func compilerApplicationOutput(t *testing.T, raw []byte, group compilerApplicationGroup) map[string]compilerApplicationArtifact {
	t.Helper()
	var output struct {
		Contracts map[string]map[string]compilerApplicationArtifact `json:"contracts"`
		Sources   map[string]json.RawMessage                        `json:"sources"`
		Errors    []struct {
			Severity string `json:"severity"`
		} `json:"errors"`
	}
	if len(raw) == 0 || int64(len(raw)) > verify.DefaultCompileOutputBytes || json.Unmarshal(raw, &output) != nil {
		t.Fatal("complete application compiler response is absent, invalid or oversized")
	}
	for _, diagnostic := range output.Errors {
		if diagnostic.Severity == "error" {
			t.Fatal("real vendor rejected a complete application workload")
		}
	}
	if len(output.Sources) != len(group.Sources) {
		t.Fatal("actual compiled source inventory differs from the complete input")
	}
	for _, source := range group.Sources {
		if _, ok := output.Sources[source.Unit]; !ok {
			t.Fatal("an expected compiled source is missing")
		}
	}
	observed := make(map[string]compilerApplicationArtifact)
	for unit, contracts := range output.Contracts {
		for name, artifact := range contracts {
			observed[unit+":"+name] = artifact
		}
	}
	if len(observed) != len(group.Targets) {
		t.Fatalf("actual compiled target inventory differs from the complete manifest: group=%s actual=%d expected=%d", group.Name, len(observed), len(group.Targets))
	}
	for _, expected := range group.Targets {
		artifact, ok := observed[expected.Name]
		if !ok || compilerApplicationCanonicalHash(t, artifact.ABI) != expected.ABI {
			t.Fatal("actual target ABI differs from the original artifact or locked compiler material")
		}
		creation, runtime := compilerApplicationCode(t, artifact.EVM)
		if compilerApplicationHash(creation) != expected.Creation || expected.Runtime != "" && compilerApplicationHash(runtime) != expected.Runtime {
			t.Fatal("actual executable or metadata bytes differ from the original complete artifact")
		}
		var metadata struct {
			Compiler struct {
				Version string `json:"version"`
			} `json:"compiler"`
			Settings struct {
				Target map[string]string `json:"compilationTarget"`
			} `json:"settings"`
		}
		at := strings.LastIndexByte(expected.Name, ':')
		if at < 1 || json.Unmarshal([]byte(artifact.Metadata), &metadata) != nil || metadata.Compiler.Version != group.Version ||
			len(metadata.Settings.Target) != 1 || metadata.Settings.Target[expected.Name[:at]] != expected.Name[at+1:] {
			t.Fatal("actual target compiler metadata or compilation target differs")
		}
		if expected.EVM != "" && compilerApplicationCanonicalHash(t, artifact.EVM) != expected.EVM {
			t.Fatal("complete legacy EVM, source map or reference map differs from its published lock", expected.Name)
		}
		if expected.StorageLayout != "" && compilerApplicationCanonicalHash(t, artifact.Layout) != expected.StorageLayout ||
			expected.Metadata != "" && compilerApplicationHash([]byte(artifact.Metadata)) != expected.Metadata {
			t.Fatal("complete target layout or metadata differs from its original lock", expected.Name)
		}
		if len(expected.ImmutableReferences) > 0 || len(expected.LinkReferences) > 0 {
			var references struct {
				Creation struct {
					Links json.RawMessage `json:"linkReferences"`
				} `json:"bytecode"`
				Runtime struct {
					Links      json.RawMessage `json:"linkReferences"`
					Immutables json.RawMessage `json:"immutableReferences"`
				} `json:"deployedBytecode"`
			}
			if json.Unmarshal(artifact.EVM, &references) != nil {
				t.Fatal("complete target reference material is invalid", expected.Name)
			}
			empty := func(raw json.RawMessage) json.RawMessage {
				if len(raw) == 0 {
					return json.RawMessage(`{}`)
				}
				return raw
			}
			links, err := json.Marshal(map[string]json.RawMessage{"creation": empty(references.Creation.Links), "runtime": empty(references.Runtime.Links)})
			if err != nil || len(expected.LinkReferences) > 0 && compilerApplicationCanonicalHash(t, links) != compilerApplicationCanonicalHash(t, expected.LinkReferences) ||
				len(expected.ImmutableReferences) > 0 && compilerApplicationCanonicalHash(t, empty(references.Runtime.Immutables)) != compilerApplicationCanonicalHash(t, expected.ImmutableReferences) {
				t.Fatal("complete target immutable or link references differ from their original lock", expected.Name)
			}
		}
	}
	return observed
}

// This fresh service uses the production output/stderr defaults. The original
// small-fixture launcher and every original profile remain unchanged.
func compilerApplicationStart(t *testing.T, dir, listURL string) *compilerProfileProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	cmd := exec.Command("/qualified/zvm-node", "solcd", "-listen", address, "-solc-dir", dir, "-solc-url", listURL,
		"-workers", strconv.Itoa(verify.DefaultCompileConcurrent), "-timeout", (2 * time.Minute).String(),
		"-max-output-bytes", strconv.FormatInt(verify.DefaultCompileOutputBytes, 10),
		"-max-stderr-bytes", strconv.FormatInt(verify.DefaultCompileStderrBytes, 10), "-max-cache-bytes", strconv.Itoa(48<<20), "-max-cache-files", "3")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=/tmp", "GOMAXPROCS=2", "GOMEMLIMIT=128MiB", "N01_PARENT_SENTINEL=synthetic-parent-only"}
	logs := new(compilerProfileLog)
	cmd.Stdout, cmd.Stderr = logs, logs
	process := &compilerProfileProcess{command: cmd, done: make(chan struct{}), url: "http://" + address, logs: logs}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { process.err = cmd.Wait(); close(process.done) }()
	t.Cleanup(func() { process.stop(t) })
	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.done:
			t.Fatal("actual application compiler service exited before readiness")
		default:
		}
		response, err := client.Get(process.url + "/status")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("actual application compiler service did not become ready")
	return nil
}

func TestCompilerApplicationContainerProfile(t *testing.T) {
	if os.Getenv("ZVM_N01_APPLICATION_PROFILE") != "1" {
		t.Skip("requires the explicit complete application compiler kernel profile")
	}
	if os.Getenv("ZVM_N01_SOLCD_BINARY") != "/qualified/zvm-node" {
		t.Fatal("declared production binary is missing")
	}
	compilerProfileKernel(t)
	before := compilerProfileOOM(t)
	var manifest struct {
		Schema        int                        `json:"schema"`
		SourceParent  string                     `json:"source_parent"`
		Packages      []json.RawMessage          `json:"packages"`
		Groups        []compilerApplicationGroup `json:"groups"`
		ArtifactFiles []json.RawMessage          `json:"artifact_files"`
		Provenance    json.RawMessage            `json:"provenance"`
	}
	decoder := json.NewDecoder(bytes.NewReader(compilerApplicationManifest))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.Schema != 1 || len(manifest.Groups) != 5 || len(manifest.Packages) != 5 || len(manifest.ArtifactFiles) != 6 {
		t.Fatal("complete embedded application workload manifest is invalid")
	}
	var bundle struct {
		Schema   int    `json:"schema"`
		Manifest string `json:"manifest_sha256"`
		Groups   []struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"groups"`
	}
	info, err := os.Lstat("/qualified/compiler-applications.json")
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 8<<20 {
		t.Fatal("explicit complete literal workload bundle is absent or oversized")
	}
	rawBundle, err := os.ReadFile("/qualified/compiler-applications.json")
	if err != nil || int64(len(rawBundle)) != info.Size() {
		t.Fatal("complete literal workload bundle could not be read")
	}
	decoder = json.NewDecoder(bytes.NewReader(rawBundle))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&bundle) != nil || decoder.Decode(new(any)) != io.EOF || bundle.Schema != 1 || len(bundle.Groups) != 5 || bundle.Manifest != compilerApplicationHash(compilerApplicationManifest) {
		t.Fatal("complete literal workload bundle differs from its embedded manifest")
	}
	var builds []compilerVendorBuild
	if json.Unmarshal(compilerVendorManifest, &builds) != nil || len(builds) != 3 {
		t.Fatal("original selected vendor manifest is invalid")
	}
	dir := t.TempDir()
	var retained int64
	for _, build := range builds {
		retained += compilerVendorSeed(t, build, dir)
	}
	if retained > 48<<20 {
		t.Fatal("complete application workload exceeds the declared vendor cache budget")
	}
	var lists, downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/linux-amd64/list.json" {
			lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": builds})
			return
		}
		downloads.Add(1)
		http.Error(w, "unexpected fixture request", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	process := compilerApplicationStart(t, dir, server.URL)
	client := &verify.RemoteRunner{URL: process.url, MaxOutputBytes: verify.DefaultCompileOutputBytes}
	groups := []string{"apps", "bench", "wznn", "core", "periphery"}
	observed := make(map[string]map[string]compilerApplicationArtifact)
	targets, exports, runtimes := 0, 0, 0
	for index, group := range manifest.Groups {
		if group.Name != groups[index] || bundle.Groups[index].Name != group.Name || group.InputBytes != len(bundle.Groups[index].Input) || compilerApplicationHash(bundle.Groups[index].Input) != group.InputSHA256 {
			t.Fatal("a complete standard-JSON input or group identity differs")
		}
		if !t.Run(group.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			started := time.Now()
			raw, err := client.Compile(ctx, group.Version, bundle.Groups[index].Input)
			if err != nil {
				t.Fatal("complete actual vendor application compile failed", err)
			}
			observed[group.Name] = compilerApplicationOutput(t, raw, group)
			compilerProfileRecord(t, map[string]any{"case": "actual_application_vendor_compile", "group": group.Name, "compiler_version": group.Version,
				"input_sha256": group.InputSHA256, "input_bytes": group.InputBytes, "source_units": len(group.Sources), "compiled_target_records": len(group.Targets),
				"tracked_export_records": len(group.Exports), "response_sha256": compilerApplicationHash(raw), "response_bytes": len(raw),
				"elapsed_milliseconds": time.Since(started).Milliseconds(), "complete_ABI_creation_and_declared_runtime_comparisons": true,
				"legacy_complete_EVM_layout_and_metadata_comparisons": group.Name == "core" || group.Name == "periphery",
				"metadata_or_immutable_byte_masks":                    0, "same_Node_RemoteRunner_and_production_loader": true, "service_binary_downloads": downloads.Load(),
				"constructor_release_real_chain_or_operator_acceptance": false})
		}) {
			t.Fatal("complete application workload failed; later groups were not attempted")
		}
		targets += len(group.Targets)
		exports += len(group.Exports)
		for _, artifact := range group.Exports {
			if artifact.Runtime != "" {
				runtimes++
			}
		}
	}
	if targets != 122 || exports != 82 || runtimes != 81 {
		t.Fatal("the complete original output inventory was not qualified")
	}
	// Reproduce the production loader's deliberate name overwrites. Every
	// per-file export is checked above; a shared name is bound once after the
	// final application layer is added, exactly as apps.LoadArtifacts does.
	loaded, err := apps.LoadArtifacts()
	if err != nil {
		t.Fatal("actual production application artifact loader failed")
	}
	final := make(map[string]compilerApplicationArtifact)
	for _, name := range []string{"bench", "wznn", "core", "periphery", "apps"} {
		for _, group := range manifest.Groups {
			if group.Name != name {
				continue
			}
			for _, exported := range group.Exports {
				at := strings.LastIndexByte(exported.Name, ':')
				if at < 1 {
					t.Fatal("production export target identity is invalid")
				}
				final[exported.Name[at+1:]] = observed[name][exported.Name]
			}
		}
	}
	if len(final) != 63 {
		t.Fatal("the complete production loader binding inventory differs")
	}
	for name, native := range final {
		artifact, err := loaded.Get(name)
		parsed, parseErr := abi.JSON(bytes.NewReader(native.ABI))
		creation, _ := compilerApplicationCode(t, native.EVM)
		if err != nil || parseErr != nil || !bytes.Equal(artifact.Bin, creation) || !reflect.DeepEqual(artifact.ABI, parsed) {
			t.Fatal("the actual production loader differs from its native-compiled final binding", name)
		}
	}
	process.stop(t)
	if lists.Load() != 1 || downloads.Load() != 0 {
		t.Fatal("complete application workloads did not reuse one list and all original cached binaries")
	}
	after := compilerProfileOOM(t)
	if after["oom"] != before["oom"] || after["oom_kill"] != before["oom_kill"] {
		t.Fatal("complete application workloads caused an OOM event")
	}
	compilerProfileRecord(t, map[string]any{"case": "application_production_loader_binding", "compiled_target_records": targets, "tracked_export_records": exports,
		"complete_export_runtime_records": runtimes, "missing_original_WZNN_runtime_not_inferred": true, "unique_final_production_loader_bindings": len(final),
		"all_final_ABI_and_creation_bindings_equal": true, "bundle_sha256": compilerApplicationHash(rawBundle), "manifest_sha256": bundle.Manifest,
		"compiler_input_and_runtime_data_not_rewritten": true})
	compilerProfileRecord(t, map[string]any{"case": "compiler_application_kernel_profile", "actual_vendor_versions": 3, "actual_compiler_invocations": 5,
		"actual_production_service_lifetimes": 1, "joined_services": 1, "retained_cache_bytes": retained, "cache_byte_limit": 48 << 20, "cache_file_limit": 3,
		"release_list_requests": 1, "service_binary_downloads": 0, "memory_max_bytes": 512 << 20, "swap_max_bytes": 0, "CPUs": 2, "pids_max": 64, "tmpfs_bytes": 64 << 20,
		"uid": 10001, "gid": 10001, "read_only_root": true, "network": "none", "effective_capabilities_zero": true, "no_new_privileges": true, "OOM_events": 0,
		"input_byte_limit": verify.DefaultCompileInputBytes, "output_byte_limit": verify.DefaultCompileOutputBytes, "stderr_byte_limit": verify.DefaultCompileStderrBytes,
		"compiler_timeout_seconds": 120, "configured_workers": verify.DefaultCompileConcurrent, "real_application_compiles_serialized_for_exact_artifact_accounting": true, "concurrent_real_application_capacity_qualified": false, "original_small_contract_profiles_unchanged": true,
		"operator_platform_per_invocation_containment_or_release_acceptance": false})
}
