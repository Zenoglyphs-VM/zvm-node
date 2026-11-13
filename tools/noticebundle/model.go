//go:build unix

// Package main creates bounded source-notice observations for one CLI build.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	rootModule  = "github.com/Zenoglyphs-VM/zvm-node"
	coreModule  = "github.com/Zenoglyphs-VM/zvm-core"
	maxCommand  = 8 << 20
	maxSource   = 32 << 20
	maxSources  = 256 << 20
	maxNotice   = 128 << 10
	maxObjects  = 8 << 20
	maxManifest = 8 << 20
)

var (
	publicPath = regexp.MustCompile(`^[A-Za-z0-9._~!+/-]+$`)
	version    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	digest40   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	noticeFile = regexp.MustCompile(`(?i)^(licen[cs]es?|copying|notice|copyright|authors|contributors|patents|legal)(?:[._-].*)?$`)
	spdx       = regexp.MustCompile(`(?m)SPDX-License-Identifier:\s*([A-Za-z0-9.+() -]+)`)
)

type selection struct {
	Path    string
	Version string
	Main    bool
	Dir     string
	Replace *selection
}

type packageInput struct {
	ImportPath string
	Name       string
	Dir        string
	Standard   bool
	Incomplete bool
	Module     *selection
	Files      []sourceInput
	EmbedFiles []string
}

type sourceInput struct {
	Group string
	Path  string
}

type moduleIdentity struct {
	Path             string `json:"path"`
	Version          string `json:"version"`
	EffectivePath    string `json:"effectivePath"`
	EffectiveVersion string `json:"effectiveVersion"`
	Main             bool   `json:"main"`
}

type filePin struct {
	Path          string   `json:"path"`
	Bytes         int64    `json:"bytes"`
	SHA256        string   `json:"sha256"`
	SPDX          []string `json:"spdxIdentifiersObserved"`
	InitialNotice string   `json:"initialNoticeObject,omitempty"`
	Group         string   `json:"selectionGroup"`
}

type noticePin struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Scope  string `json:"selectionScope"`
}

type moduleObservation struct {
	Identity           moduleIdentity `json:"identity"`
	Notices            []noticePin    `json:"notices"`
	ReviewRequired     bool           `json:"reviewRequired"`
	MissingNoticeFiles bool           `json:"noCandidateNoticeFileObserved"`
}

type packageObservation struct {
	Path     string    `json:"path"`
	Module   string    `json:"module"`
	Sources  []filePin `json:"selectedSourceFiles"`
	Embedded []filePin `json:"embeddedFiles"`
}

type streamPin struct {
	Bytes     int    `json:"bytesCaptured"`
	SHA256    string `json:"sha256"`
	Limit     int    `json:"limit"`
	Truncated bool   `json:"truncated"`
}

type commandPin struct {
	Label          string    `json:"label"`
	ActualExit     *int      `json:"actualExit"`
	OutputComplete bool      `json:"outputComplete"`
	Stdout         streamPin `json:"stdout"`
	Stderr         streamPin `json:"stderr"`
}

type manifest struct {
	Schema                               int                  `json:"schema"`
	Status                               string               `json:"status"`
	Entry                                string               `json:"entryPackage"`
	Profile                              map[string]string    `json:"profile"`
	CallerSourceCommit                   string               `json:"callerSourceCommit,omitempty"`
	CallerCoreCommit                     string               `json:"callerCoreCommit,omitempty"`
	SourceCommitAuthenticated            bool                 `json:"sourceCommitAuthenticated"`
	RootLocks                            map[string]string    `json:"rootLockSha256"`
	CoreLocks                            map[string]string    `json:"coreLockSha256"`
	BinarySHA256                         string               `json:"binarySha256"`
	BinaryBytes                          int64                `json:"binaryBytes"`
	Modules                              []moduleObservation  `json:"modules"`
	Packages                             []packageObservation `json:"packages"`
	Objects                              map[string]int       `json:"objects"`
	Commands                             []commandPin         `json:"commands"`
	PrivatePathsExported                 bool                 `json:"privatePathsExported"`
	LegalReviewComplete                  bool                 `json:"legalReviewComplete"`
	CorrespondingSourceOrRelinkingReview string               `json:"correspondingSourceOrRelinkingReview"`
	APKLicenseTextReview                 string               `json:"runtimeAPKLicenseTextReview"`
	CompleteSBOM                         bool                 `json:"completeSBOM"`
	ReleaseAccepted                      bool                 `json:"releaseAccepted"`
	BinaryModulesMatched                 bool                 `json:"binaryModuleIdentitiesMatched"`
	SourceScope                          string               `json:"sourceSelectionScope"`
}

func hash(raw []byte) string {
	value := sha256.Sum256(raw)
	return hex.EncodeToString(value[:])
}

func portable(value string) bool {
	if len(value) == 0 || len(value) > 1024 || !publicPath.MatchString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func regular(info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	data := reflect.ValueOf(info.Sys())
	if data.Kind() != reflect.Ptr || data.IsNil() || data.Elem().Kind() != reflect.Struct {
		return false
	}
	links := data.Elem().FieldByName("Nlink")
	return links.IsValid() && links.CanUint() && links.Uint() == 1
}

func relative(root, name string) (string, error) {
	rel, err := filepath.Rel(root, name)
	if err != nil || !portable(filepath.ToSlash(rel)) {
		return "", errors.New("file is outside its selected source root")
	}
	return filepath.ToSlash(rel), nil
}

func noLinks(root, name string) error {
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes selected source root")
	}
	current := root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := 0; i <= len(parts); i++ {
		if i > 0 {
			current = filepath.Join(current, parts[i-1])
		}
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("selected source path is missing or linked")
		}
		if i < len(parts) && !info.IsDir() {
			return errors.New("selected source ancestor is not a directory")
		}
	}
	return nil
}

func readRegular(root, name string, limit int64) ([]byte, error) {
	if err := noLinks(root, name); err != nil {
		return nil, err
	}
	before, err := os.Lstat(name)
	if err != nil || !regular(before) || before.Size() > limit {
		return nil, errors.New("selected file is not an independent bounded regular file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("selected file could not be opened")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !regular(opened) {
		return nil, errors.New("selected file identity changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	last, pathErr := os.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || len(raw) > int(limit) || int64(len(raw)) != before.Size() ||
		!os.SameFile(before, after) || !os.SameFile(before, last) || !regular(last) ||
		before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.ModTime() != last.ModTime() {
		return nil, errors.New("selected file changed during observation")
	}
	return raw, nil
}

func uniqueJSON(raw []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds the bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errors.New("duplicate or invalid JSON member")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		} else {
			return errors.New("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("extra JSON value")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}

type cappedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(raw []byte) (int, error) {
	room := b.limit - len(b.data)
	if len(raw) > room {
		b.data = append(b.data, raw[:room]...)
		b.truncated = true
		return room, errors.New("command stream exceeded its cap")
	}
	b.data = append(b.data, raw...)
	return len(raw), nil
}

func (b *cappedBuffer) pin() streamPin {
	return streamPin{len(b.data), hash(b.data), b.limit, b.truncated}
}

func command(label string, arguments ...string) ([]byte, commandPin, error) {
	return commandBudget(label, 120*time.Second, maxCommand, 256<<10, arguments...)
}

func commandBudget(label string, duration time.Duration, outLimit, errLimit int, arguments ...string) ([]byte, commandPin, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	out, stderr := &cappedBuffer{limit: outLimit}, &cappedBuffer{limit: errLimit}
	cmd := exec.CommandContext(ctx, "go", arguments...)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "GOFLAGS=-mod=readonly",
		"CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=none", "GONOSUMDB=none")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	pin := commandPin{Label: label, Stdout: out.pin(), Stderr: stderr.pin()}
	if cmd.ProcessState != nil {
		exit := cmd.ProcessState.ExitCode()
		pin.ActualExit = &exit
	}
	var exitError *exec.ExitError
	pin.OutputComplete = cmd.ProcessState != nil && ctx.Err() == nil && !out.truncated && !stderr.truncated &&
		(err == nil || errors.As(err, &exitError))
	if err != nil || !pin.OutputComplete {
		return nil, pin, errors.New("bounded Go metadata command failed or is incomplete")
	}
	return out.data, pin, nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeNew(name string, raw []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return errors.New("new bundle output already exists or cannot be opened")
	}
	written, writeErr := file.Write(raw)
	closeErr := file.Close()
	if written != len(raw) || writeErr != nil || closeErr != nil {
		return errors.New("bundle output write did not complete")
	}
	return nil
}

func identity(module *selection, project string) (moduleIdentity, string, error) {
	if module == nil || !portable(module.Path) || !filepath.IsAbs(module.Dir) {
		return moduleIdentity{}, "", errors.New("invalid selected module identity")
	}
	id := moduleIdentity{Path: module.Path, Version: module.Version, EffectivePath: module.Path, EffectiveVersion: module.Version, Main: module.Main}
	directory := module.Dir
	if module.Main {
		if module.Path != rootModule || module.Version != "" || module.Replace != nil || filepath.Clean(directory) != filepath.Clean(project) {
			return id, "", errors.New("main module differs from the selected project")
		}
	} else if !version.MatchString(module.Version) {
		return id, "", errors.New("dependency does not have a versioned nominal identity")
	}
	if replacement := module.Replace; replacement != nil {
		if replacement.Replace != nil || replacement.Main {
			return id, "", errors.New("nested replacement is unsupported")
		}
		if module.Path == coreModule {
			if module.Version != "v0.0.0" || replacement.Path != "./zvm-core" || replacement.Version != "" ||
				filepath.Clean(replacement.Dir) != filepath.Join(project, "zvm-core") {
				return id, "", errors.New("unexpected local Core replacement")
			}
		} else if !portable(replacement.Path) || !version.MatchString(replacement.Version) {
			return id, "", errors.New("unknown local or unversioned replacement")
		}
		id.EffectivePath, id.EffectiveVersion = replacement.Path, replacement.Version
		directory = replacement.Dir
	}
	if !filepath.IsAbs(directory) {
		return id, "", errors.New("missing effective module source root")
	}
	return id, directory, nil
}

func moduleKey(id moduleIdentity) string { return id.Path }

func safeError(stage string) error { return fmt.Errorf("notice collection failed at %s", stage) }
