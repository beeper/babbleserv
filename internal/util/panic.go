package util

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/rs/zerolog"
)

const panicRetries = 3

func logPanic(log *zerolog.Logger, r any) {
	log.Error().
		Any("recover", r).
		Type("recover_type", r).
		Msgf("PANIC! %s", debug.Stack())
}

func PanicRetryLoop(ctx context.Context, log zerolog.Logger, f func()) {
	var lastPanic any
	for i := 0; i <= panicRetries; i++ {
		if ctx.Err() != nil {
			return
		}
		if ok := func() bool {
			defer func() {
				if r := recover(); r != nil {
					lastPanic = r
					logPanic(&log, r)
				}
			}()
			f()
			return true
		}(); ok {
			return
		}
	}
	panic(lastPanic)
}

// Deferred by goroutines outside a request's recovery, so a panic is logged instead of taking the
// process down. The panic is returned through err unless it is nil.
func RecoverPanic(log *zerolog.Logger, err *error) {
	if r := recover(); r != nil {
		logPanic(log, r)
		if err != nil {
			*err = fmt.Errorf("panic: %v", r)
		}
	}
}
