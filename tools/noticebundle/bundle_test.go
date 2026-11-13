//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"go/format"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPublicPaths(t *testing.T) {
	for _, good := range []string{"github.com/Example/project/file.go", "src/unsafe/unsafe.go", "LICENSE-MIT"} {
		if !portable(good) {
			t.Fatal("valid public path refused")
		}
	}
	for _, bad := range []string{"", "/fixture/private", "../outside", "a/../b", "a//b", "a\\b", "a\npath"} {
		if portable(bad) {
			t.Fatal("unsafe path accepted")
		}
	}
}

func TestJSONRejectsDuplicatesUnknownFieldsAndTrailingValues(t *testing.T) {
	var target struct{ Value string }
	for _, raw := range []string{`{"Value":"a","Value":"b"}`, `{"Other":"a"}`, `{"Value":"a"} {}`, `{"Value":`, strings.Repeat("[", 70) + strings.Repeat("]", 70)} {
		if err := uniqueJSON([]byte(raw), &target); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
	if err := uniqueJSON([]byte(`{"Value":"ok"}`), &target); err != nil || target.Value != "ok" {
		t.Fatal("valid JSON refused")
	}
}

func TestBoundedBufferPreservesCapturedPrefix(t *testing.T) {
	b := cappedBuffer{limit: 4}
	if _, err := b.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Write([]byte("cdef")); n != 2 || err == nil {
		t.Fatal("stream cap not enforced")
	}
	if string(b.data) != "abcd" || !b.pin().Truncated || b.pin().SHA256 != hash([]byte("abcd")) {
		t.Fatal("capture pins differ")
	}
}

func TestRegularFileAndByteBudget(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "LICENSE")
	put(t, name, "original\n")
	raw, err := readRegular(root, name, 32)
	if err != nil || string(raw) != "original\n" {
		t.Fatal("regular source differs")
	}
	if _, err := readRegular(root, name, 1); err == nil {
		t.Fatal("oversize file accepted")
	}
	if _, err := readRegular(root, root, 32); err == nil {
		t.Fatal("directory accepted as notice")
	}
}

func TestLinkedNoticeAndAncestorAreRefused(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "original")
	put(t, source, "original")
	link := filepath.Join(root, "LICENSE")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(root, link, 32); err == nil {
		t.Fatal("symbolic file link accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "real/NOTICE"), "notice")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(root, filepath.Join(root, "alias/NOTICE"), 32); err == nil {
		t.Fatal("symbolic ancestor accepted")
	}
}

func TestHardLinkedAndOutsideFilesAreRefused(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "original")
	put(t, name, "original")
	if err := os.Link(name, filepath.Join(root, "LICENSE")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(root, name, 32); err == nil {
		t.Fatal("hard linked notice accepted")
	}
	if _, err := relative(root, filepath.Join(root, "../outside")); err == nil {
		t.Fatal("escaping relative name accepted")
	}
}

func TestInitialCopyrightAndSPDXTextIsNotRewritten(t *testing.T) {
	header := "//go:build example\n\n// Copyright Example Authors\n// SPDX-License-Identifier: MIT\n\n"
	if got := initialNotice([]byte(header + "package example\n")); string(got) != header {
		t.Fatal("notice prefix changed")
	}
	block := "/* Copyright Example Authors */"
	if got := initialNotice([]byte(block + "package example\n")); string(got) != block {
		t.Fatal("code after comment entered notice text")
	}
	if initialNotice([]byte("// ordinary documentation\npackage example\n")) != nil {
		t.Fatal("ordinary documentation selected")
	}
	if initialNotice([]byte("/* Copyright incomplete")) != nil {
		t.Fatal("incomplete comment selected")
	}
}

func TestIdenticalNoticePayloadIsRetainedOnce(t *testing.T) {
	c := newCollector()
	first, err := c.object([]byte("original notice\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.object([]byte("original notice\n"))
	if err != nil || first != second || len(c.objects) != 1 {
		t.Fatal("payload duplicated")
	}
	if _, err := c.object(bytes.Repeat([]byte("x"), maxNotice+1)); err == nil {
		t.Fatal("oversize notice accepted")
	}
}

func TestModuleIdentityKeepsNominalAndEffectiveVersions(t *testing.T) {
	project := t.TempDir()
	value := &selection{Path: "example.org/upstream", Version: "v1.2.3", Dir: filepath.Join(project, "cache"),
		Replace: &selection{Path: "example.org/fork", Version: "v1.100200.3", Dir: filepath.Join(project, "fork")}}
	id, root, err := identity(value, project)
	if err != nil || id.Path != value.Path || id.EffectivePath != "example.org/fork" || root != value.Replace.Dir {
		t.Fatal("replacement identity differs")
	}
	value.Replace.Version = ""
	if _, _, err := identity(value, project); err == nil {
		t.Fatal("unversioned replacement accepted")
	}
}

func TestCoreLocalSelectionIsExplicit(t *testing.T) {
	project := t.TempDir()
	root := filepath.Join(project, "zvm-core")
	value := &selection{Path: coreModule, Version: "v0.0.0", Dir: root, Replace: &selection{Path: "./zvm-core", Dir: root}}
	id, _, err := identity(value, project)
	if err != nil || id.EffectivePath != "./zvm-core" || id.EffectiveVersion != "" {
		t.Fatal("known local Core refused")
	}
	value.Replace.Dir = project
	if _, _, err := identity(value, project); err == nil {
		t.Fatal("changed Core source root accepted")
	}
	value.Replace.Dir = root
	value.Replace.Replace = &selection{}
	if _, _, err := identity(value, project); err == nil {
		t.Fatal("nested replacement accepted")
	}
}

func packageFixture(t *testing.T) ([]packageInput, string, string) {
	t.Helper()
	project := t.TempDir()
	sdk := t.TempDir()
	core := filepath.Join(project, "zvm-core")
	put(t, filepath.Join(project, "LICENSE"), "shared notice\n")
	put(t, filepath.Join(project, "cmd/zvm-node/main.go"), "package main\n")
	put(t, filepath.Join(project, "cmd/zvm-node/artifact.json"), "{}")
	put(t, filepath.Join(core, "LICENSE"), "shared notice\n")
	put(t, filepath.Join(core, "amount/amount.go"), "package amount\n")
	put(t, filepath.Join(sdk, "LICENSE"), "SDK notice\n")
	put(t, filepath.Join(sdk, "src/unsafe/unsafe.go"), "package unsafe\n")
	return []packageInput{
		{ImportPath: rootModule + "/cmd/zvm-node", Name: "main", Dir: filepath.Join(project, "cmd/zvm-node"),
			Module: &selection{Path: rootModule, Main: true, Dir: project}, Files: []sourceInput{{"GoFiles", "main.go"}}, EmbedFiles: []string{"artifact.json"}},
		{ImportPath: coreModule + "/amount", Name: "amount", Dir: filepath.Join(core, "amount"),
			Module: &selection{Path: coreModule, Version: "v0.0.0", Dir: core, Replace: &selection{Path: "./zvm-core", Dir: core}}, Files: []sourceInput{{"GoFiles", "amount.go"}}},
		{ImportPath: "unsafe", Name: "unsafe", Standard: true, Dir: filepath.Join(sdk, "src/unsafe"), Files: []sourceInput{{"GoFiles", "unsafe.go"}}},
	}, project, sdk
}

func TestSelectedPackageAndEmbeddedFileInventory(t *testing.T) {
	packages, project, sdk := packageFixture(t)
	c := newCollector()
	rows, err := c.collect(packages, project, sdk, "go1.26.11")
	if err != nil || len(rows) != 3 || len(c.modules) != 3 || len(c.objects) != 2 {
		t.Fatal("fixture inventory differs", err)
	}
	for _, row := range rows {
		if row.Path == rootModule+"/cmd/zvm-node" && (len(row.Embedded) != 1 || row.Embedded[0].SHA256 != hash([]byte("{}"))) {
			t.Fatal("embedded payload not bound")
		}
	}
}

func TestIncompleteDuplicateAndPrivatePackageInputIsRefused(t *testing.T) {
	for _, bad := range []string{"", `{"ImportPath":"a","ImportPath":"b"}` + "\n", `{"ImportPath":"/fixture/private","Name":"x","Dir":"/fixture"}` + "\n"} {
		if _, err := parsePackages([]byte(bad)); err == nil {
			t.Fatal("ambiguous package stream accepted")
		}
	}
	packages, _, _ := packageFixture(t)
	raw, _ := json.Marshal(packages[0])
	raw = append(raw, '\n')
	raw = append(raw, raw...)
	if _, err := parsePackages(raw); err == nil {
		t.Fatal("duplicate package accepted")
	}
}

func TestSelectedSourceCannotEscapeItsModule(t *testing.T) {
	packages, project, sdk := packageFixture(t)
	packages[0].Files[0].Path = "../outside"
	if _, err := newCollector().collect(packages, project, sdk, "go1.26.11"); err == nil {
		t.Fatal("escaping selected file accepted")
	}
}

func TestBinaryDependencySetAndCoreDebugVersion(t *testing.T) {
	modules := map[string]*moduleObservation{rootModule: {Identity: moduleIdentity{Path: rootModule, Main: true}},
		coreModule: {Identity: moduleIdentity{Path: coreModule, Version: "v0.0.0", EffectivePath: "./zvm-core"}}, "std": {}}
	info := &debug.BuildInfo{Path: rootModule + "/cmd/zvm-node", Main: debug.Module{Path: rootModule, Version: "(devel)"},
		Deps: []*debug.Module{{Path: coreModule, Version: "v0.0.0", Replace: &debug.Module{Path: "./zvm-core", Version: "(devel)"}}}}
	if err := binaryModules(info, modules); err != nil {
		t.Fatal(err)
	}
	info.Deps[0].Version = "v0.0.1"
	if err := binaryModules(info, modules); err == nil {
		t.Fatal("binary version mismatch accepted")
	}
	info.Deps = nil
	if err := binaryModules(info, modules); err == nil {
		t.Fatal("missing binary dependency accepted")
	}
}

func fakeGo(t *testing.T, script string) {
	t.Helper()
	directory := t.TempDir()
	name := filepath.Join(directory, "go")
	put(t, name, "#!/bin/sh\n"+script+"\n")
	if err := os.Chmod(name, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+":"+os.Getenv("PATH"))
}

func TestMetadataCommandRecordsFailureWithCompleteStreams(t *testing.T) {
	fakeGo(t, "printf output; printf diagnostic >&2; exit 7")
	_, pin, err := commandBudget("fixture", time.Second, 1024, 1024, "fixture")
	if err == nil || pin.ActualExit == nil || *pin.ActualExit != 7 || !pin.OutputComplete || pin.Stdout.Bytes != 6 {
		t.Fatal("actual command outcome was lost")
	}
}

func TestMetadataCommandTimeoutStopsTheProcessGroup(t *testing.T) {
	fakeGo(t, "sleep 30 & wait")
	start := time.Now()
	_, pin, err := commandBudget("fixture", 100*time.Millisecond, 1024, 1024, "fixture")
	if err == nil || pin.OutputComplete || time.Since(start) > 4*time.Second {
		t.Fatal("command deadline not enforced")
	}
}

func TestNewOutputNeverOverwritesAnExistingFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "manifest.json")
	put(t, name, "original")
	if err := writeNew(name, []byte("replacement")); err == nil {
		t.Fatal("existing output was overwritten")
	}
	raw, _ := os.ReadFile(name)
	if string(raw) != "original" {
		t.Fatal("original output changed")
	}
}

func TestGoSourcesUseCanonicalFormatting(t *testing.T) {
	for _, name := range []string{"main.go", "model.go", "collect.go", "bundle_test.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		formatted, err := format.Source(raw)
		if err != nil || !bytes.Equal(raw, formatted) {
			t.Fatalf("Go source formatting differs: %s", name)
		}
	}
}
