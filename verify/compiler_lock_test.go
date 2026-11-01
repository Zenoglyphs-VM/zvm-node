package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual List and Path APIs while an independent request owns
// their network operation. Every fixture joins both callers before cleanup.
func TestCompilerCacheWaitsRespectCallerCancellation(t *testing.T) {
	for _, operation := range []string{"release_list", "binary_download"} {
		for _, reason := range []string{"already_canceled", "deadline"} {
			t.Run(operation+"/"+reason, func(t *testing.T) {
				entered, released := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(released) }) }
				var requests atomic.Int32
				binary := []byte("controlled immutable compiler fixture")
				response := binary
				if operation == "release_list" {
					response = []byte(`{"builds":[{"path":"first-fixture","version":"0.0.2","longVersion":"0.0.2+commit.wait"}]}`)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if requests.Add(1) == 1 {
						close(entered)
						select {
						case <-released:
						case <-r.Context().Done():
							return
						}
					}
					_, _ = w.Write(response)
				}))
				defer server.Close()
				defer release()
				compilers := &Compilers{Dir: t.TempDir(), BaseURL: server.URL, MaxCacheBytes: 1024, MaxCacheFiles: 2}
				digest := sha256.Sum256(binary)
				first := Build{Path: "first-fixture", Version: "0.0.2", LongVersion: "0.0.2+commit.wait", SHA256: hex.EncodeToString(digest[:])}
				second := Build{Path: "second-fixture", Version: "0.0.3", LongVersion: "0.0.3+commit.wait", SHA256: first.SHA256}
				call := func(ctx context.Context, build Build) error {
					if operation == "release_list" {
						list, err := compilers.List(ctx)
						if err == nil && (list == nil || len(list.Builds) != 1) {
							return errors.New("release list changed")
						}
						return err
					}
					path, err := compilers.Path(ctx, build)
					if err == nil {
						raw, readErr := os.ReadFile(path)
						if readErr != nil || !bytes.Equal(raw, binary) {
							return errors.New("retained compiler changed")
						}
					}
					return err
				}
				ownerCtx, ownerCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer ownerCancel()
				ownerDone := make(chan struct{})
				var ownerErr error
				go func() { ownerErr = call(ownerCtx, first); close(ownerDone) }()
				defer func() {
					release()
					select {
					case <-ownerDone:
					case <-time.After(2 * time.Second):
						t.Error("cache owner did not join")
					}
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("independent cache owner did not enter its controlled request")
				}
				var waiterCtx context.Context
				var waiterCancel context.CancelFunc
				want := context.Canceled
				if reason == "deadline" {
					waiterCtx, waiterCancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					want = context.DeadlineExceeded
				} else {
					waiterCtx, waiterCancel = context.WithCancel(context.Background())
					waiterCancel()
				}
				defer waiterCancel()
				waiterDone := make(chan struct{})
				var waiterErr error
				go func() { waiterErr = call(waiterCtx, second); close(waiterDone) }()
				defer func() {
					waiterCancel()
					release()
					select {
					case <-waiterDone:
					case <-time.After(2 * time.Second):
						t.Error("canceled cache waiter did not join")
					}
				}()
				select {
				case <-waiterDone:
					if !errors.Is(waiterErr, want) {
						t.Errorf("cache wait lost its caller cancellation: got %v, want %v", waiterErr, want)
					}
				case <-time.After(time.Second):
					t.Error("canceled caller waited for another request's network operation")
				}
				if requests.Load() != 1 {
					t.Error("canceled waiter started another network request")
				}
				select {
				case <-ownerDone:
					t.Error("canceling the waiter stopped the independent cache owner")
				default:
				}
				release()
				select {
				case <-ownerDone:
					if ownerErr != nil {
						t.Fatalf("independent cache owner failed: %v", ownerErr)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cache owner did not finish after release")
				}
				select {
				case <-waiterDone:
				case <-time.After(2 * time.Second):
					t.Fatal("canceled cache waiter did not finish after release")
				}
				fresh, freshCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer freshCancel()
				if err := call(fresh, second); err != nil {
					t.Fatalf("a canceled waiter disabled a fresh cache operation: %v", err)
				}
				wantRequests := int32(2)
				if operation == "release_list" {
					wantRequests = 1
				}
				if requests.Load() != wantRequests {
					t.Fatalf("cache requests changed: got %d, want %d", requests.Load(), wantRequests)
				}
			})
		}
	}
}
