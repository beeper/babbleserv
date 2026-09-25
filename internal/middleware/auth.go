package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type contextKey string

const requestUserKey contextKey = "user"
const requestUserAuthErrorKey contextKey = "user_auth_error"
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
			token := ""
			credentialsProvided := authHeader != ""
			if authHeader != "" {
				parts := strings.Fields(authHeader)
				if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
					token = parts[1]
				}
			} else if queryToken := r.URL.Query().Get("access_token"); queryToken != "" {
				token = queryToken
				credentialsProvided = true
			}

			if token != "" {
				userDevice, err := getUserDeviceForAuthToken(ctx, token)
				if err == nil {
					ctx = context.WithValue(ctx, requestUserKey, &userDevice)
				} else {
					ctx = context.WithValue(ctx, requestUserAuthErrorKey, err)
				}
			} else if credentialsProvided {
				ctx = context.WithValue(ctx, requestUserAuthErrorKey, mautrix.MUnknownToken)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetRequestUserDevice(r *http.Request) *types.UserDevice {
	u := r.Context().Value(requestUserKey)
	if u == nil {
		return nil
	}
	return u.(*types.UserDevice)
}

func RequireUserAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := GetRequestUserDevice(r)
		if u == nil {
			if err, _ := r.Context().Value(requestUserAuthErrorKey).(error); errors.Is(err, types.ErrTokenExpired) {
				util.ResponseJSON(w, r, http.StatusUnauthorized, map[string]any{
					"errcode":     mautrix.MUnknownToken.ErrCode,
					"error":       "Access token has expired",
					"soft_logout": true,
				})
			} else if err, _ := r.Context().Value(requestUserAuthErrorKey).(error); err != nil {
				if !errors.Is(err, types.ErrUserNotFound) && !errors.Is(err, mautrix.MUnknownToken) {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				}
				util.ResponseErrorJSON(w, r, mautrix.MUnknownToken)
			} else {
				util.ResponseErrorJSON(w, r, mautrix.MMissingToken)
			}
			return
		}
		log := hlog.FromRequest(r)
		log.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("request_ud", u.UserID.String()+"/"+u.DeviceID.String())
		})
		next(w, r)
	}
}

// Panics if there's no request user
func GetRequestUserID(r *http.Request) id.UserID {
	return GetRequestUserDevice(r).UserID
}

// Panics if there's no request user
func GetRequestDeviceID(r *http.Request) id.DeviceID {
	return GetRequestUserDevice(r).DeviceID
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
