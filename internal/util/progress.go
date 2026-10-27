package util

import "context"

type progressKey struct{}

// WithProgress has work done under ctx report its progress to fn, for a job whose lease is refreshed
// on progress and whose watchdog stops it without any
func WithProgress(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress tells the job ctx belongs to, if any, that a bounded unit of its work completed,
// such as a staging transaction or a renewed read transaction
func ReportProgress(ctx context.Context) {
	if fn, ok := ctx.Value(progressKey{}).(func()); ok {
		fn()
	}
}
