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
	for _, good := range []string{"github.com/Example/project/file.go", "src/unsafe/unsafe.go", "LICENSE-MIT", strings.Repeat("a", 1024)} {
		if !portable(good) {
			t.Fatal("valid public path refused")
		}
	}
	for _, bad := range []string{"", "/fixture/private", "../outside", "a/../b", "a//b", "a\\b", "a\npath", strings.Repeat("a", 1025)} {
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
	for _, name := range []string{"main.go", "model.go", "collect.go", "supplements.go", "bundle_test.go"} {
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

func TestPrefixedAndUnlicenseNoticeNamesAreSelected(t *testing.T) {
	for _, name := range []string{"MIT-LICENSE.txt", "BSD-3-Clause-LICENSE", "UNLICENSE", "LICENSE", "COPYING.LESSER", "NOTICE.md"} {
		if !noticeFile.MatchString(name) {
			t.Fatal("notice name not selected", name)
		}
	}
	for _, name := range []string{"main.go", "noticeable.go", "licensedata.go", "README.md"} {
		if noticeFile.MatchString(name) {
			t.Fatal("unrelated file selected", name)
		}
	}
}

func falseFlag() *bool { value := false; return &value }

func pinnedSource(raw string) upstreamPin {
	return upstreamPin{Bytes: int64(len(raw)), SHA256: hash([]byte(raw)), GitBlob: gitBlob([]byte(raw))}
}

func supplementFixture(t *testing.T) (*collector, string, supplement) {
	t.Helper()
	project, root := t.TempDir(), t.TempDir()
	identity := moduleIdentity{Path: "example.org/library", Version: "v1.2.3", EffectivePath: "example.org/library", EffectiveVersion: "v1.2.3"}
	module := "module example.org/library\n\ngo 1.18\n"
	source, notice := "package library\n", "Copyright Public Fixture Authors\nOriginal notice\n"
	retained := "tools/noticebundle/supplements/fixture/MIT-LICENSE.txt"
	put(t, filepath.Join(root, "go.mod"), module)
	put(t, filepath.Join(root, "library.go"), source)
	put(t, filepath.Join(root, "empty.dat"), "")
	put(t, filepath.Join(project, retained), notice)
	c := newCollector()
	c.modules[identity.Path] = &moduleObservation{Identity: identity, Notices: []noticePin{}, ReviewRequired: true}
	c.moduleRoots[identity.Path] = root
	for name, group := range map[string]string{"library.go": "GoFiles", "empty.dat": "EmbedFiles"} {
		if _, err := c.pinFile(identity.Path, root, filepath.Join(root, name), group); err != nil {
			t.Fatal(err)
		}
	}
	entry := supplement{Module: identity, Repository: "https://github.com/example/library", Commit: strings.Repeat("3", 40), ModuleSubdirectory: "v2",
		ModuleGoMod: pinnedSource(module), SelectedFiles: map[string]upstreamPin{"library.go": pinnedSource(source), "empty.dat": pinnedSource("")},
		Notice: upstreamNotice{Path: "MIT-LICENSE.txt", SourcePath: retained, upstreamPin: pinnedSource(notice)}, Status: supplementStatus,
		LegalReviewComplete: falseFlag(), IndependentOriginAuthenticated: falseFlag(), CompleteSourceCoverage: falseFlag()}
	return c, project, entry
}

func registryBytes(t *testing.T, entries ...supplement) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Schema  int          `json:"schema"`
		Entries []supplement `json:"entries"`
	}{1, entries})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPinnedSupplementBindsSelectedBytesAndPreservesNotice(t *testing.T) {
	c, project, entry := supplementFixture(t)
	raw := registryBytes(t, entry)
	if err := c.supplements(project, raw); err != nil {
		t.Fatal(err)
	}
	rows := c.modules[entry.Module.Path].Notices
	if len(rows) != 1 || rows[0].Scope != supplementScope || rows[0].Origin == nil ||
		rows[0].Origin.RegistrySHA256 != hash(raw) || rows[0].Origin.Entry.Commit != entry.Commit ||
		gitBlob(c.objects[rows[0].SHA256]) != entry.Notice.GitBlob || *rows[0].Origin.Entry.LegalReviewComplete {
		t.Fatal("supplement attribution or binding differs")
	}
}

func TestSupplementRefusesModuleSourceAndMetadataDrift(t *testing.T) {
	for _, change := range []string{"version", "source_digest", "source_blob", "source_set", "source_disk", "go_mod"} {
		t.Run(change, func(t *testing.T) {
			c, project, entry := supplementFixture(t)
			switch change {
			case "version":
				entry.Module.Version = "v1.2.4"
			case "source_digest":
				pin := entry.SelectedFiles["library.go"]
				pin.SHA256 = strings.Repeat("a", 64)
				entry.SelectedFiles["library.go"] = pin
			case "source_blob":
				pin := entry.SelectedFiles["library.go"]
				pin.GitBlob = strings.Repeat("a", 40)
				entry.SelectedFiles["library.go"] = pin
			case "source_set":
				delete(entry.SelectedFiles, "empty.dat")
			case "source_disk":
				put(t, filepath.Join(c.moduleRoots[entry.Module.Path], "library.go"), "package changed\n")
			case "go_mod":
				entry.ModuleGoMod = pinnedSource("module example.org/other\n")
			}
			if c.supplements(project, registryBytes(t, entry)) == nil {
				t.Fatal("stale source supplement accepted")
			}
		})
	}
}

func TestSupplementRefusesChangedLinkedAndConflictingNotice(t *testing.T) {
	for _, change := range []string{"contents", "symlink", "path_collision"} {
		t.Run(change, func(t *testing.T) {
			c, project, entry := supplementFixture(t)
			name := filepath.Join(project, entry.Notice.SourcePath)
			switch change {
			case "contents":
				put(t, name, "rewritten notice\n")
			case "symlink":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(project, "absent"), name); err != nil {
					t.Fatal(err)
				}
			case "path_collision":
				c.modules[entry.Module.Path].Notices = []noticePin{{Path: entry.Notice.Path}}
			}
			if c.supplements(project, registryBytes(t, entry)) == nil {
				t.Fatal("ambiguous notice supplement accepted")
			}
		})
	}
}

func TestSupplementRegistryRejectsOverstatedAndUnpinnedEntries(t *testing.T) {
	for _, change := range []string{"legal", "origin", "coverage", "missing_flag", "commit", "repository", "source_path", "upstream_path", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			c, project, entry := supplementFixture(t)
			switch change {
			case "legal":
				*entry.LegalReviewComplete = true
			case "origin":
				*entry.IndependentOriginAuthenticated = true
			case "coverage":
				*entry.CompleteSourceCoverage = true
			case "missing_flag":
				entry.LegalReviewComplete = nil
			case "commit":
				entry.Commit = "main"
			case "repository":
				entry.Repository = "https://example.invalid/private"
			case "source_path":
				entry.Notice.SourcePath = "../outside"
			case "upstream_path":
				entry.Notice.Path = "/fixture/private"
			}
			entries := []supplement{entry}
			if change == "duplicate" {
				entries = append(entries, entry)
			}
			if c.supplements(project, registryBytes(t, entries...)) == nil {
				t.Fatal("invalid source supplement accepted")
			}
		})
	}
	for _, raw := range []string{`{"schema":1,"entries":[] ,"extra":true}`, `{"schema":1,"schema":1,"entries":[]}`, `{"schema":1,"entries":null}`} {
		if newCollector().supplements(t.TempDir(), []byte(raw)) == nil {
			t.Fatal("malformed registry accepted")
		}
	}
}

func TestUnselectedSupplementDoesNotCopyItsNotice(t *testing.T) {
	c, project, entry := supplementFixture(t)
	delete(c.modules, entry.Module.Path)
	if err := c.supplements(project, registryBytes(t, entry)); err != nil || len(c.objects) != 0 {
		t.Fatal("unselected notice copied", err)
	}
}
