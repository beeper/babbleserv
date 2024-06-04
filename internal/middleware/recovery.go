package middleware

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/util"
)

const httpStatusClientClosed = 499

func NewRecoveryMiddleware(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				err := recover()
				if err == nil {
					return
				}

				if err := r.Context().Err(); errors.Is(err, context.Canceled) {
					log.Debug().Msg("Context canceled during request")
					w.WriteHeader(httpStatusClientClosed)
					return
				}

				log.Error().
					Str("method", r.Method).
					Stringer("url", r.URL).
					Msgf("Panic in route: %v %s", err, debug.Stack())

				util.ResponseErrorJSON(w, r, util.MUnknown)
			}()

			next.ServeHTTP(w, r)
		})
	}
}
