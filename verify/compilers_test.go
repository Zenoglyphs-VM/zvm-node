package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseHost serves one build and a list.json that vouches for it — which
// is all a compromised release host has to do to pass the list's own check.
func releaseHost(t *testing.T, body []byte, longVersion string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/list.json") {
			fmt.Fprintf(w, `{"builds":[{"path":"solc-test","version":"0.0.1","longVersion":%q,"sha256":"0x%x"}]}`, longVersion, sum)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A pinned version must be the pinned bytes, whatever the release host's list
// says about the ones it served (D34).
func TestPinnedCompilersAreCheckedAgainstThePin(t *testing.T) {
	plat, err := platform()
	if err != nil {
		t.Skip(err)
	}
	const version = "0.0.1+commit.test"
	honest := []byte("the compiler everyone agrees on")
	pin := sha256.Sum256(honest)
	defer delete(pinned, plat+"/"+version)
	pinned[plat+"/"+version] = hex.EncodeToString(pin[:])

	ctx := context.Background()
	bad := &Compilers{Dir: t.TempDir(), BaseURL: releaseHost(t, []byte("a different binary"), version).URL}
	b, err := bad.Resolve(ctx, version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Path(ctx, b); err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("a binary whose list vouched for it but the pin did not was accepted: %v", err)
	}

	good := &Compilers{Dir: t.TempDir(), BaseURL: releaseHost(t, honest, version).URL}
	b, _ = good.Resolve(ctx, version)
	path, err := good.Path(ctx, b)
	if err != nil {
		t.Fatalf("the pinned binary was refused: %v", err)
	}

	// One already on disk from before the pin has to meet it too.
	stale := &Compilers{Dir: filepath.Dir(path), BaseURL: good.BaseURL}
	if err := os.WriteFile(path, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Path(ctx, b); err == nil {
		t.Fatal("a tampered pinned compiler on disk was used")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the tampered compiler was left on disk")
	}
}
