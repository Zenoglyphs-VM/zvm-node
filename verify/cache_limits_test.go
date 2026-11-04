package verify

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCompilerCacheBudgetPreservesExistingFilesAndBoundsNewDownloads(t *testing.T) {
	for _, fixture := range []string{"byte budget full", "file budget full", "incoming too large", "exact byte boundary", "unknown directory"} {
		t.Run(fixture, func(t *testing.T) {
			dir := t.TempDir()
			retained := []byte("keep")
			if err := os.WriteFile(filepath.Join(dir, "retained"), retained, 0o600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			body := []byte("binary")
			sum := sha256.Sum256(body)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write(body) }))
			defer srv.Close()
			c := &Compilers{Dir: dir, BaseURL: srv.URL, MaxCacheBytes: 10, MaxCacheFiles: 2}
			b := Build{LongVersion: "0.0.3+commit.fixture", Path: "solc-fixture", SHA256: hex.EncodeToString(sum[:])}
			switch fixture {
			case "byte budget full":
				c.MaxCacheBytes = int64(len(retained))
			case "file budget full":
				c.MaxCacheFiles = 1
			case "incoming too large":
				c.MaxCacheBytes--
			case "unknown directory":
				if err := os.Mkdir(filepath.Join(dir, "unknown"), 0o700); err != nil {
					t.Fatal(err)
				}
				c.MaxCacheFiles = 3
			}
			path, err := c.Path(context.Background(), b)
			if fixture == "exact byte boundary" {
				if err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
					t.Fatalf("exact-fit compiler changed: bytes=%q err=%v", got, err)
				}
			} else if err == nil || path != "" {
				t.Fatalf("exhausted or unaccounted cache accepted a new compiler: path=%q err=%v", path, err)
			}
			if (fixture == "byte budget full" || fixture == "file budget full") && (!errors.Is(err, ErrCompilerCacheFull) || requests.Load() != 0) {
				t.Fatalf("full cache performed transport or lost refusal: requests=%d err=%v", requests.Load(), err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "retained")); err != nil || !bytes.Equal(got, retained) {
				t.Fatal("existing cache bytes were removed or changed to make room")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if fixture == "exact byte boundary" || fixture == "unknown directory" {
				want = 2
			}
			if len(entries) != want {
				t.Fatalf("download left extra cache files: %d, want %d", len(entries), want)
			}
		})
	}
}

func TestConcurrentCompilerDownloadsCannotSpendOneCacheSlotTwice(t *testing.T) {
	var requests atomic.Int32
	body := []byte("binary")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write(body) }))
	defer srv.Close()
	c := &Compilers{Dir: t.TempDir(), BaseURL: srv.URL, MaxCacheFiles: 1}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, version := range []string{"0.0.3+commit.fixture", "0.0.4+commit.fixture"} {
		wg.Add(1)
		go func(i int, version string) {
			defer wg.Done()
			_, errs[i] = c.Path(context.Background(), Build{LongVersion: version, Path: "solc-fixture", SHA256: hex.EncodeToString(sum[:])})
		}(i, version)
	}
	wg.Wait()
	if requests.Load() != 1 || !((errs[0] == nil && errors.Is(errs[1], ErrCompilerCacheFull)) || (errs[1] == nil && errors.Is(errs[0], ErrCompilerCacheFull))) {
		t.Fatalf("cache slot was overspent: requests=%d errors=%v", requests.Load(), errs)
	}
}

func TestCompilerArchiveCannotSilentlyTruncateItsExecutable(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("solc.exe")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("123456789"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if body, err := unzipSolc(archive.Bytes(), 8); err == nil || body != nil {
		t.Fatalf("oversized executable was accepted or truncated: %q err=%v", body, err)
	}
	if body, err := unzipSolc(archive.Bytes(), 9); err != nil || string(body) != "123456789" {
		t.Fatalf("exact-size executable was refused: %q err=%v", body, err)
	}
}

func TestCompilerReleasePathsCannotEscapeTheCache(t *testing.T) {
	for _, name := range []string{"../outside", "nested/file", `nested\file`, "name with spaces"} {
		c := &Compilers{Dir: t.TempDir(), BaseURL: "http://unused.invalid"}
		if _, err := c.Path(context.Background(), Build{LongVersion: name, Path: "solc-fixture"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid release filename reached transport: %q err=%v", name, err)
		}
		if _, err := c.Path(context.Background(), Build{LongVersion: "0.0.3+commit.fixture", Path: name}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid download filename reached transport: %q err=%v", name, err)
		}
	}
}

func TestCompilerCacheLinksAreRefusedWithoutChangingTheirTargets(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "protected-fixture")
	contents := []byte("keep this fixture unchanged")
	if err := os.WriteFile(outside, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const version = "0.0.3+commit.fixture"
	link := filepath.Join(dir, "solc-"+version)
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	c := &Compilers{Dir: dir, BaseURL: "http://unused.invalid"}
	if _, err := c.Path(context.Background(), Build{LongVersion: version, Path: "solc-fixture", SHA256: hex.EncodeToString(sum[:])}); err == nil {
		t.Fatal("cache link was treated as an owned executable")
	}
	if got, err := os.ReadFile(outside); err != nil || !bytes.Equal(got, contents) {
		t.Fatal("cache refusal altered the link target")
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("cache refusal removed or replaced the link")
	}
}
