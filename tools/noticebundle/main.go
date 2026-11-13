//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const readme = `Source notice observations for this exact CLI binary

manifest.json binds the binary digest, selected runtime packages, effective Go
module identities, observed source/embedded-file digests and copied notice text.
objects/<sha256> retains each selected notice payload once, without rewriting
its contents or attribution. Header candidates are initial source comments that
mention copyright or licensing; their selection is a mechanical observation.
Package ancestor notices and module notice directories can include material
beyond this build. A missing candidate is unresolved, not an exemption.

Versioned source supplements retain upstream notice text from outside a module
archive. Each supplement binds the exact selected module, source-file and go.mod
bytes, upstream commit/blob identifiers and the checked-in notice/registry bytes.
Those observations do not authenticate the upstream provider or approve a license.

This packet does not establish complete license obligations, classification,
compatibility, corresponding-source or relinking requirements, runtime APK
notice texts, compiler-intermediate coverage, independent input origin or legal
approval. Distribution and release review remain required. The source commit
fields are caller supplied; qualification must independently bind them to the
checked source and image. No host/cache paths or raw Go metadata are exported.
`

func collectBundle(output, binary, sourceCommit, coreCommit string) (manifest, error) {
	result := manifest{Schema: 2, Status: "SOURCE_NOTICE_BUNDLE_FAILED_OR_INCOMPLETE", Entry: rootModule + "/cmd/zvm-node",
		RootLocks: map[string]string{}, CoreLocks: map[string]string{}, Objects: map[string]int{}, Commands: []commandPin{},
		Modules: []moduleObservation{}, Packages: []packageObservation{},
		CorrespondingSourceOrRelinkingReview: "NOT_ASSESSED", APKLicenseTextReview: "NOT_ASSESSED",
		SourceScope: "Selected non-test CLI source inputs and embedded files; compiler intermediates are not exported."}
	if sourceCommit != "" && !digest40.MatchString(sourceCommit) || coreCommit != "" && !digest40.MatchString(coreCommit) {
		return result, errors.New("caller source revision is not a public commit identifier")
	}
	result.CallerSourceCommit, result.CallerCoreCommit = sourceCommit, coreCommit
	if err := os.Mkdir(output, 0755); err != nil {
		return result, safeError("new_output_directory")
	}
	project, err := os.Getwd()
	if err != nil {
		return result, safeError("project_directory")
	}
	run := func(label string, args ...string) ([]byte, error) {
		raw, pin, err := command(label, args...)
		result.Commands = append(result.Commands, pin)
		return raw, err
	}
	raw, err := run("profile", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOROOT", "GOMOD")
	if err != nil {
		return result, safeError("profile_command")
	}
	var privateProfile map[string]string
	if err := uniqueJSON(raw, &privateProfile); err != nil {
		return result, safeError("profile_json")
	}
	if privateProfile["GOVERSION"] != "go1.26.11" || privateProfile["GOOS"] != "linux" ||
		privateProfile["CGO_ENABLED"] != "0" || !portable(privateProfile["GOARCH"]) ||
		!filepath.IsAbs(privateProfile["GOROOT"]) || privateProfile["GOMOD"] != filepath.Join(project, "go.mod") {
		return result, safeError("selected_build_profile")
	}
	result.Profile = map[string]string{}
	for _, name := range []string{"GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED"} {
		result.Profile[name] = privateProfile[name]
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := readRegular(project, filepath.Join(project, name), 16<<20)
		if err != nil {
			return result, safeError("root_locks")
		}
		result.RootLocks[name] = hash(data)
		core := filepath.Join(project, "zvm-core")
		data, err = readRegular(core, filepath.Join(core, name), 16<<20)
		if err != nil {
			return result, safeError("core_locks")
		}
		result.CoreLocks[name] = hash(data)
	}
	if _, err := run("verify_before", "mod", "verify"); err != nil {
		return result, safeError("module_verification_before")
	}
	raw, err = run("selected_packages", "list", "-deps", "-f", listTemplate(), "./cmd/zvm-node")
	if err != nil {
		return result, safeError("selected_packages_command")
	}
	packages, err := parsePackages(raw)
	if err != nil {
		return result, safeError("package_projection")
	}
	c := newCollector()
	result.Packages, err = c.collect(packages, project, privateProfile["GOROOT"], privateProfile["GOVERSION"])
	if err != nil {
		return result, safeError("source_notice_collection")
	}
	for _, key := range []string{rootModule, coreModule} {
		if _, exists := c.modules[key]; !exists {
			return result, safeError("required_source_module")
		}
		for _, name := range []string{"LICENSE", "NOTICE.md"} {
			root := c.moduleRoots[key]
			if err := c.notice(key, root, filepath.Join(root, name), "REQUIRED_PROJECT_NOTICE"); err != nil {
				return result, safeError("required_project_notices")
			}
		}
	}
	if _, exists := c.modules["std"]; !exists {
		return result, safeError("missing_sdk_packages")
	}
	if err := c.notice("std", privateProfile["GOROOT"], filepath.Join(privateProfile["GOROOT"], "LICENSE"), "REQUIRED_SDK_NOTICE"); err != nil {
		return result, safeError("required_sdk_notice")
	}
	registry, err := readRegular(project, filepath.Join(project, supplementRegistryPath), 1<<20)
	if err != nil {
		return result, safeError("source_supplement_registry")
	}
	if err := c.supplements(project, registry); err != nil {
		return result, safeError("source_supplement_binding")
	}
	result.BinarySHA256, result.BinaryBytes, err = readBinary(binary, result.Profile, c.modules)
	if err != nil {
		return result, safeError("binary_module_binding")
	}
	result.BinaryModulesMatched = true
	if _, err := run("verify_after", "mod", "verify"); err != nil {
		return result, safeError("module_verification_after")
	}
	for _, pair := range []struct {
		root string
		pins map[string]string
	}{{project, result.RootLocks}, {filepath.Join(project, "zvm-core"), result.CoreLocks}} {
		for name, want := range pair.pins {
			data, err := readRegular(pair.root, filepath.Join(pair.root, name), 16<<20)
			if err != nil || hash(data) != want {
				return result, safeError("source_locks_after")
			}
		}
	}
	for _, key := range sortedKeys(c.modules) {
		module := c.modules[key]
		sort.Slice(module.Notices, func(i, j int) bool { return module.Notices[i].Path < module.Notices[j].Path })
		module.MissingNoticeFiles = len(module.Notices) == 0
		result.Modules = append(result.Modules, *module)
	}
	for digest, raw := range c.objects {
		result.Objects[digest] = len(raw)
	}
	result.Status = "SOURCE_NOTICE_BUNDLE_RECORDED_REVIEW_REQUIRED"
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil || len(encoded) > maxManifest {
		return result, safeError("manifest_output_cap")
	}
	if err := os.Mkdir(filepath.Join(output, "objects"), 0755); err != nil {
		return result, safeError("object_directory")
	}
	for _, digest := range sortedKeys(c.objects) {
		if err := writeNew(filepath.Join(output, "objects", digest), c.objects[digest]); err != nil {
			return result, safeError("object_write")
		}
	}
	if err := writeNew(filepath.Join(output, "README.txt"), []byte(readme)); err != nil {
		return result, safeError("readme_write")
	}
	if err := writeNew(filepath.Join(output, "manifest.json"), append(encoded, '\n')); err != nil {
		return result, safeError("manifest_write")
	}
	return result, nil
}

func main() {
	output := flag.String("output", "", "new notice output directory")
	binary := flag.String("binary", "", "already built CLI binary")
	source := flag.String("source-commit", "", "caller-selected source commit, when known")
	core := flag.String("core-commit", "", "caller-selected Core commit, when known")
	flag.Parse()
	if *output == "" || *binary == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "output and binary are required")
		os.Exit(2)
	}
	result, err := collectBundle(*output, *binary, *source, *core)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		data, _ := json.Marshal(struct {
			Status   string       `json:"status"`
			Commands []commandPin `json:"commands"`
			Legal    bool         `json:"legalReviewComplete"`
		}{
			"SOURCE_NOTICE_BUNDLE_FAILED_OR_INCOMPLETE", result.Commands, false})
		fmt.Println(string(data))
		os.Exit(2)
	}
	data, _ := json.Marshal(map[string]any{"status": result.Status, "packages": len(result.Packages), "modules": len(result.Modules),
		"noticeObjects": len(result.Objects), "legalReviewComplete": false, "releaseAccepted": false})
	fmt.Println(string(data))
}
