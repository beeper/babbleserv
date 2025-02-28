package middleware

import (
	"context"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

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

func NewUserAuthMiddleware(
	serverName string,
	getUserDeviceForAuthToken func(context.Context, string) (types.UserDevice, error),
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			authHeader := r.Header.Get("Authorization")

			if len(authHeader) > 7 {
				token := authHeader[7:]
				userDevice, err := getUserDeviceForAuthToken(ctx, token)
				if err == nil {
					ctx = context.WithValue(ctx, requestUserKey, &userDevice)
				}
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func getRequestUserDevice(r *http.Request) *types.UserDevice {
	u := r.Context().Value(requestUserKey)
	if u == nil {
		return nil
	}
	return u.(*types.UserDevice)
}

func RequireUserAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := getRequestUserDevice(r)
		if u == nil {
			util.ResponseErrorJSON(w, r, mautrix.MMissingToken)
			return
		}
		log := hlog.FromRequest(r)
		log.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("user_id", u.UserID.String())
		})
		next(w, r)
	}
}

// Panics if there's no request user
func GetRequestUserID(r *http.Request) id.UserID {
	return getRequestUserDevice(r).UserID
}

// Panics if there's no request user
func GetRequestDeviceID(r *http.Request) id.DeviceID {
	return getRequestUserDevice(r).DeviceID
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
