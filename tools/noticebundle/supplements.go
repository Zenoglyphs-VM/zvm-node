//go:build unix

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const supplementRegistryPath = "tools/noticebundle/notice-supplements.json"
const supplementStatus = "UPSTREAM_SOURCE_BYTES_MATCHED_REVIEW_REQUIRED"
const supplementScope = "PINNED_UPSTREAM_SUPPLEMENT"

var upstreamRepository = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var digest64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type upstreamPin struct {
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	GitBlob string `json:"gitBlob"`
}

type upstreamNotice struct {
	Path       string `json:"path"`
	SourcePath string `json:"sourcePath"`
	upstreamPin
}

type supplement struct {
	Module                         moduleIdentity         `json:"module"`
	Repository                     string                 `json:"repository"`
	Commit                         string                 `json:"commit"`
	ModuleSubdirectory             string                 `json:"moduleSubdirectory"`
	ModuleGoMod                    upstreamPin            `json:"moduleGoMod"`
	SelectedFiles                  map[string]upstreamPin `json:"selectedFiles"`
	Notice                         upstreamNotice         `json:"notice"`
	Status                         string                 `json:"status"`
	LegalReviewComplete            *bool                  `json:"legalReviewComplete"`
	IndependentOriginAuthenticated *bool                  `json:"independentOriginAuthenticated"`
	CompleteSourceCoverage         *bool                  `json:"completeSourceCoverage"`
}

type supplementOrigin struct {
	RegistryPath   string     `json:"registryPath"`
	RegistrySHA256 string     `json:"registrySha256"`
	Entry          supplement `json:"entry"`
}

func gitBlob(raw []byte) string {
	value := sha1.New()
	fmt.Fprintf(value, "blob %d\x00", len(raw))
	value.Write(raw)
	return hex.EncodeToString(value.Sum(nil))
}

func validUpstreamPin(pin upstreamPin, limit int64) bool {
	return pin.Bytes >= 0 && pin.Bytes <= limit && digest64.MatchString(pin.SHA256) && digest40.MatchString(pin.GitBlob)
}

func validSupplement(entry supplement) bool {
	id := entry.Module
	if !portable(id.Path) || !portable(id.EffectivePath) || !version.MatchString(id.Version) ||
		!version.MatchString(id.EffectiveVersion) || id.Main || id.Path == rootModule || id.Path == coreModule || id.Path == "std" ||
		!upstreamRepository.MatchString(entry.Repository) || !digest40.MatchString(entry.Commit) ||
		(entry.ModuleSubdirectory != "" && !portable(entry.ModuleSubdirectory)) ||
		!validUpstreamPin(entry.ModuleGoMod, 65536) || entry.ModuleGoMod.Bytes == 0 || len(entry.SelectedFiles) == 0 || len(entry.SelectedFiles) > 4096 ||
		!validUpstreamPin(entry.Notice.upstreamPin, maxNotice) || entry.Notice.Bytes == 0 || !portable(entry.Notice.Path) ||
		!noticeFile.MatchString(filepath.Base(entry.Notice.Path)) || !portable(entry.Notice.SourcePath) ||
		!strings.HasPrefix(entry.Notice.SourcePath, "tools/noticebundle/supplements/") || entry.Status != supplementStatus {
		return false
	}
	for _, flag := range []*bool{entry.LegalReviewComplete, entry.IndependentOriginAuthenticated, entry.CompleteSourceCoverage} {
		if flag == nil || *flag {
			return false
		}
	}
	var total int64
	for name, pin := range entry.SelectedFiles {
		if !portable(name) || !validUpstreamPin(pin, maxSource) {
			return false
		}
		total += pin.Bytes
	}
	return total <= maxSources
}

func matchesUpstreamPin(raw []byte, pin upstreamPin) bool {
	return int64(len(raw)) == pin.Bytes && hash(raw) == pin.SHA256 && gitBlob(raw) == pin.GitBlob
}

func (c *collector) supplements(project string, raw []byte) error {
	var registry struct {
		Schema  int          `json:"schema"`
		Entries []supplement `json:"entries"`
	}
	if len(raw) == 0 || len(raw) > 1<<20 || uniqueJSON(raw, &registry) != nil || registry.Schema != 1 ||
		registry.Entries == nil || len(registry.Entries) > 64 {
		return errors.New("source supplement registry is malformed or oversized")
	}
	seen := map[string]bool{}
	for _, entry := range registry.Entries {
		key := entry.Module.Path + "\x00" + entry.Notice.Path
		if !validSupplement(entry) || seen[key] {
			return errors.New("source supplement identity is invalid or duplicate")
		}
		seen[key] = true
		module := c.modules[entry.Module.Path]
		if module == nil {
			continue // A source supplement is packaged only for a selected runtime module.
		}
		if module.Identity != entry.Module {
			return errors.New("selected module differs from its pinned source supplement")
		}
		selected := map[string]filePin{}
		for key, pin := range c.seenSource {
			if strings.HasPrefix(key, entry.Module.Path+"\x00") {
				selected[pin.Path] = pin
			}
		}
		if len(selected) != len(entry.SelectedFiles) {
			return errors.New("supplement does not bind the complete selected file set")
		}
		root := c.moduleRoots[entry.Module.Path]
		for name, want := range entry.SelectedFiles {
			actual, ok := selected[name]
			if !ok || actual.Bytes != want.Bytes || actual.SHA256 != want.SHA256 {
				return errors.New("selected source differs from its upstream supplement")
			}
			data, err := readRegular(root, filepath.Join(root, filepath.FromSlash(name)), maxSource)
			if err != nil || !matchesUpstreamPin(data, want) {
				return errors.New("upstream source blob does not match selected bytes")
			}
		}
		data, err := readRegular(root, filepath.Join(root, "go.mod"), 65536)
		if err != nil || !matchesUpstreamPin(data, entry.ModuleGoMod) {
			return errors.New("supplement module metadata differs")
		}
		data, err = readRegular(project, filepath.Join(project, filepath.FromSlash(entry.Notice.SourcePath)), maxNotice)
		if err != nil || !matchesUpstreamPin(data, entry.Notice.upstreamPin) {
			return errors.New("retained upstream notice bytes differ")
		}
		for _, previous := range module.Notices {
			if previous.Path == entry.Notice.Path {
				return errors.New("source supplement conflicts with an observed notice path")
			}
		}
		digest, err := c.object(data)
		if err != nil {
			return err
		}
		module.Notices = append(module.Notices, noticePin{Path: entry.Notice.Path, Bytes: int64(len(data)), SHA256: digest,
			Scope: supplementScope, Origin: &supplementOrigin{RegistryPath: supplementRegistryPath, RegistrySHA256: hash(raw), Entry: entry}})
	}
	return nil
}
