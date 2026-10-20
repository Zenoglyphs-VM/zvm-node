package bench

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Zenoglyphs-VM/zvm-core/ethrpc"
)

// PooledTransport, PooledHTTP and DialPooled are internal/ethrpc's, kept here
// because every caller in this package already uses these names.
func PooledTransport() *http.Transport { return ethrpc.PooledTransport() }

func PooledHTTP(timeout time.Duration) *http.Client { return ethrpc.PooledHTTP(timeout) }

func DialPooled(ctx context.Context, url string) (*ethclient.Client, error) {
	return ethrpc.Dial(ctx, url)
}

// Parallel runs fn for 0..n-1 on up to width goroutines. It stops handing out
// work at the first error and returns it.
func Parallel(ctx context.Context, n, width int, fn func(i int) error) error {
	if n <= 0 {
		return nil
	}
	width = max(1, min(width, n))
	var (
		next   atomic.Int64
		failed atomic.Bool
		once   sync.Once
		first  error
		wg     sync.WaitGroup
	)
	next.Store(-1)
	for g := 0; g < width; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !failed.Load() && ctx.Err() == nil {
				i := int(next.Add(1))
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					once.Do(func() { first = err })
					failed.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()
	if first == nil {
		first = ctx.Err()
	}
	return first
}
