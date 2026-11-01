package verify

// Compilers are solc's own release builds from binaries.soliditylang.org,
// fetched the first time a version is needed and checked against the SHA-256
// the release list publishes — what Sourcify and Blockscout's verifier do. A
// fetched binary is kept, so each version is downloaded once.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Compilers struct {
	Dir     string
	BaseURL string // https://binaries.soliditylang.org
	HTTP    *http.Client
	// A single service instance owns this cache. Existing files count toward
	// both limits and are never evicted to make room for another version.
	MaxCacheBytes int64
	MaxCacheFiles int

	mu        sync.Mutex // protects the short checked-map operations
	listGuard compilerLock
	list      *BuildList
	listAt    time.Time

	dl compilerLock
	// checked holds the pinned binaries on disk this process has hashed.
	checked map[string]bool
}

// pinned are the SHA-256s of the compilers this chain's own contracts were
// built with — the Uniswap V2 core and periphery, and everything in
// contracts/ — on the platform the verifier service runs on. They were read
// on 2026-09-23 from two independently hosted copies of the release list,
// binaries.soliditylang.org's and the ethereum/solc-bin repository's on
// GitHub, which agreed.
//
// A download is otherwise checked only against list.json from the host it
// came from, which proves the bytes arrived intact and nothing about the host
// (D34): a host serving a different binary with a matching list would pass.
// For these versions it no longer does.
var pinned = map[string]string{
	"linux-amd64/0.5.16+commit.9c3226ce": "a15f01700ec7e02f91bbdfd4b6ff4450b3c2decae173e4f41910a3cfbaf5d3d3",
	"linux-amd64/0.6.6+commit.6c089d02":  "5d8cd4e0cc02e9946497db68c06d56326a78ff95a21c9265cfedb819a10a539d",
	"linux-amd64/0.8.30+commit.73712a01": "f3e987dc6ecebd4bd350c48edcbc320b46cf9e3109bd3fc3d88f1acaf4c428f7",
}

// pinFor is a build's pinned SHA-256 on this platform, or "".
func pinFor(b Build) string {
	plat, err := platform()
	if err != nil {
		return ""
	}
	return pinned[plat+"/"+b.LongVersion]
}

type Build struct {
	Path        string `json:"path"`
	Version     string `json:"version"`
	LongVersion string `json:"longVersion"`
	SHA256      string `json:"sha256"`
}

type BuildList struct {
	Builds        []Build `json:"builds"`
	LatestRelease string  `json:"latestRelease"`
}

const (
	listTTL                   = time.Hour
	maxSolcSize               = 256 << 20
	DefaultCompilerCacheBytes = 1 << 30
	DefaultCompilerCacheFiles = 16
)

var (
	ErrCompilerCacheFull = errors.New("compiler cache budget exhausted")
	compilerFileName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)
)

// platform is the directory soliditylang.org publishes this machine's builds in.
func platform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux-amd64", nil
	case "windows/amd64":
		return "windows-amd64", nil
	case "darwin/amd64", "darwin/arm64":
		return "macosx-amd64", nil
	}
	return "", fmt.Errorf("%w: soliditylang.org publishes no solc for %s/%s", ErrInvalid, runtime.GOOS, runtime.GOARCH)
}

func (c *Compilers) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (c *Compilers) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://binaries.soliditylang.org"
}

// List is the release list for this platform, re-read hourly. A failed
// refresh keeps the last list it had.
func (c *Compilers) List(ctx context.Context) (*BuildList, error) {
	if err := c.listGuard.lock(ctx); err != nil {
		return nil, err
	}
	defer c.listGuard.unlock()
	if c.list != nil && time.Since(c.listAt) < listTTL {
		return c.list, nil
	}
	plat, err := platform()
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, c.base()+"/"+plat+"/list.json", 16<<20)
	if err == nil {
		var list BuildList
		if err = json.Unmarshal(body, &list); err == nil {
			c.list, c.listAt = &list, time.Now()
			return c.list, nil
		}
	}
	if c.list != nil {
		return c.list, nil
	}
	return nil, fmt.Errorf("reading solc releases: %w", err)
}

// Resolve accepts 0.8.30, 0.8.30+commit.73712a01, or Etherscan's
// v0.8.30+commit.73712a01.
func (c *Compilers) Resolve(ctx context.Context, version string) (Build, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" {
		return Build{}, fmt.Errorf("%w: no compiler version given", ErrInvalid)
	}
	list, err := c.List(ctx)
	if err != nil {
		return Build{}, err
	}
	for _, b := range list.Builds {
		if b.LongVersion == v || b.Version == v || strings.HasPrefix(v, b.LongVersion+".") {
			return b, nil
		}
	}
	return Build{}, fmt.Errorf("%w: solc %s is not a release soliditylang.org publishes", ErrInvalid, v)
}

// Path is where build's binary is on disk, fetching and checking it first if
// this node has never used it.
func (c *Compilers) Path(ctx context.Context, b Build) (string, error) {
	if c.Dir == "" || !compilerFileName.MatchString(b.LongVersion) || !compilerFileName.MatchString(b.Path) {
		return "", fmt.Errorf("%w: invalid compiler cache directory or release filename", ErrInvalid)
	}
	if sum, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(b.SHA256), "0x")); err != nil || len(sum) != sha256.Size {
		return "", fmt.Errorf("%w: compiler release needs a SHA-256", ErrInvalid)
	}
	name := "solc-" + b.LongVersion
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(c.Dir, name)
	// A compiler already on disk needs no lock: holding it here made every
	// compile wait behind whichever download was in progress, and anyone can
	// start one by naming a version this node has never used.
	if info, err := os.Lstat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("compiler cache entry is not a regular file")
		}
		if err := c.checkPinnedOnDisk(b, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	if err := c.dl.lock(ctx); err != nil {
		return "", err
	}
	defer c.dl.unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dst); err == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("compiler cache entry is not a regular file")
		}
		if err := c.checkPinnedOnDisk(b, dst); err != nil {
			return "", err
		}
		return dst, nil
	}
	remaining, err := c.cacheRemaining()
	if err != nil {
		return "", err
	}
	plat, err := platform()
	if err != nil {
		return "", err
	}
	body, err := c.get(ctx, c.base()+"/"+plat+"/"+b.Path, min(int64(maxSolcSize), remaining))
	if err != nil {
		return "", fmt.Errorf("fetching solc %s: %w", b.LongVersion, err)
	}
	sum := sha256.Sum256(body)
	if want := strings.TrimPrefix(strings.ToLower(b.SHA256), "0x"); hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("solc %s: SHA-256 %x does not match the published %s", b.LongVersion, sum, want)
	}
	if pin := pinFor(b); pin != "" && hex.EncodeToString(sum[:]) != pin {
		return "", fmt.Errorf("solc %s: SHA-256 %x is not the %s this node pins; the release host served a different binary", b.LongVersion, sum, pin)
	}
	if strings.HasSuffix(b.Path, ".zip") {
		if body, err = unzipSolc(body, min(int64(maxSolcSize), remaining)); err != nil {
			return "", fmt.Errorf("solc %s: %w", b.LongVersion, err)
		}
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(c.Dir, name+".*.part")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return dst, nil
}

func (c *Compilers) cacheRemaining() (int64, error) {
	limit, files := c.MaxCacheBytes, c.MaxCacheFiles
	if limit == 0 {
		limit = DefaultCompilerCacheBytes
	}
	if files == 0 {
		files = DefaultCompilerCacheFiles
	}
	if limit < 0 || files < 0 {
		return 0, fmt.Errorf("%w: compiler cache limits must be positive", ErrInvalid)
	}
	info, err := os.Lstat(c.Dir)
	if os.IsNotExist(err) {
		return limit, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("compiler cache root is not a directory")
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return 0, err
	}
	if len(entries) >= files {
		return 0, ErrCompilerCacheFull
	}
	remaining := limit
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("compiler cache contains an unaccounted directory or link")
		}
		if info.Size() >= remaining {
			return 0, ErrCompilerCacheFull
		}
		remaining -= info.Size()
	}
	return remaining, nil
}

// checkPinnedOnDisk hashes a pinned compiler already on disk, once per
// process: one fetched before the pin existed has to meet it too. A binary
// that does not is removed, so the next use fetches it again.
func (c *Compilers) checkPinnedOnDisk(b Build, path string) error {
	pin := pinFor(b)
	if pin == "" {
		return nil
	}
	c.mu.Lock()
	done := c.checked[path]
	c.mu.Unlock()
	if done {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != pin {
		os.Remove(path)
		return fmt.Errorf("solc %s on disk has SHA-256 %s, not the %s this node pins; removed", b.LongVersion, got, pin)
	}
	c.mu.Lock()
	if c.checked == nil {
		c.checked = map[string]bool{}
	}
	c.checked[path] = true
	c.mu.Unlock()
	return nil
}

// unzipSolc takes solc.exe out of a Windows release archive, which carries
// only the compiler.
func unzipSolc(archive []byte, limit int64) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Base(f.Name), "solc.exe") {
			if !f.Mode().IsRegular() || f.UncompressedSize64 > uint64(limit) {
				return nil, fmt.Errorf("compiler archive exceeds its byte limit or is not a regular file")
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			body, err := io.ReadAll(io.LimitReader(rc, limit+1))
			if err != nil {
				return nil, err
			}
			if int64(len(body)) > limit {
				return nil, fmt.Errorf("compiler archive exceeds its byte limit")
			}
			return body, nil
		}
	}
	return nil, fmt.Errorf("no solc.exe in the release archive")
}

func (c *Compilers) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return body, nil
}
