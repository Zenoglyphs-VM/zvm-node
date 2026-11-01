package verify

import (
	"context"
	"sync"
)

// compilerLock serializes one cache operation without making a canceled caller
// wait for another caller's network deadline. Its zero value is ready for use.
// It belongs to one Compilers instance and does not lock a shared disk cache
// across instances or processes.
type compilerLock struct {
	once sync.Once
	held chan struct{}
}

func (l *compilerLock) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.once.Do(func() { l.held = make(chan struct{}, 1) })
	select {
	case l.held <- struct{}{}:
		// If release and cancellation become ready together, canceled work
		// must not start another network request or consume a cache slot.
		if err := ctx.Err(); err != nil {
			<-l.held
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *compilerLock) unlock() { <-l.held }
