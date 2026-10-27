package util

import (
	"context"
	"sync"
	"time"
)

type progressKey struct{}

// WithProgress sends progress reported under ctx to fn, for a job whose watchdog stops it without any
func WithProgress(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress marks a bounded unit of work done, such as a staging or renewed read transaction
func ReportProgress(ctx context.Context) {
	if fn, ok := ctx.Value(progressKey{}).(func()); ok {
		fn()
	}
}

// ReportProgressEvery is for a wait its own timeout bounds rather than the job's watchdog
func ReportProgressEvery(ctx context.Context, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ReportProgress(ctx)
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}
