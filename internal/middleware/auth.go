package middleware

import (
	"context"
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type contextKey string

const requestUserKey contextKey = "user"
const requestServerKey contextKey = "server"

// User auth (CS API)
//

func NewUserAuthMiddleware(serverName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			ctx := r.Context()
			if authHeader != "" && len(authHeader) > 7 {
				user := types.User{Username: authHeader[7:], ServerName: serverName}
				ctx = context.WithValue(ctx, requestUserKey, &user)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetRequestUser(r *http.Request) *types.User {
	u := r.Context().Value(requestUserKey)
	if u == nil {
		return nil
	}
	return u.(*types.User)
}

func RequireUserAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := GetRequestUser(r)
		if u == nil {
			util.ResponseErrorJSON(w, r, mautrix.MMissingToken)
			return
		}
		log := hlog.FromRequest(r)
		log.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("user", u.Username)
		})
		next(w, r)
	}
}

// Server auth (SS API)
//

func NewServerAuthMiddleware(
	serverName string,
	keyStore *util.KeyStore,
) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if serverName, err := util.VerifyFederatonRequest(r.Context(), serverName, keyStore, r); err != nil {
				util.ResponseErrorMessageJSON(w, r, util.MUnauthorized, err.Error())
				return
			} else {
				ctx := context.WithValue(r.Context(), requestServerKey, serverName)
				next.ServeHTTP(w, r.WithContext(ctx))
			}
		})
	}
}

func GetRequestServer(r *http.Request) string {
	s := r.Context().Value(requestServerKey)
	return s.(string)
}
