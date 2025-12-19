package util

import (
	"context"
	"runtime/debug"

	"github.com/rs/zerolog"
)

const panicRetries = 3

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
					log.Error().
						Any("recover", r).
						Type("recover_type", r).
						Msgf("PANIC! %s", debug.Stack())
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
