package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/matrix-org/gomatrix"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
)

var (
	MUnknown = mautrix.RespError{
		ErrCode: "M_UNKNOWN",
	}
	MNotImplemented = mautrix.RespError{
		ErrCode: "M_NOT_IMPLEMENTED",
	}
	MMethodNotAllowed = mautrix.RespError{
		ErrCode: "M_METHOD_NOT_ALLOWED",
	}
	MUnauthorized = mautrix.RespError{
		ErrCode: "M_UNAUTHORIZED",
	}
	MNotYetUploaded = mautrix.RespError{
		ErrCode: "M_NOT_YET_UPLOADED",
	}
	MUnprocessableContent = mautrix.RespError{
		ErrCode: "M_UNPROCESSABLE",
	}
)

type errorMeta struct {
	statusCode int
	defaultMsg string
}

var errorToMeta = map[string]errorMeta{
	mautrix.MNotJSON.ErrCode:      {400, "Request body is not valid JSON"},
	mautrix.MBadJSON.ErrCode:      {400, "Request body is JSON but not match schema"},
	mautrix.MBadState.ErrCode:     {400, ""},
	mautrix.MInvalidParam.ErrCode: {400, ""},

	mautrix.MUnsupportedRoomVersion.ErrCode: {400, "Room version not supported"},
	mautrix.MRoomInUse.ErrCode:              {409, "Room alias taken"},

	mautrix.MMissingToken.ErrCode: {401, ""},
	mautrix.MUnknownToken.ErrCode: {401, ""},
	MUnauthorized.ErrCode:         {401, ""},
	mautrix.MForbidden.ErrCode:    {403, ""},
	MUnprocessableContent.ErrCode: {422, ""},

	mautrix.MNotFound.ErrCode: {404, "Nothing found here"},
	MMethodNotAllowed.ErrCode: {405, "Wrong HTTP method"},

	MUnknown.ErrCode:        {500, "An unknown error occurred"},
	MNotImplemented.ErrCode: {501, "Not implemented"},
	MNotYetUploaded.ErrCode: {504, "File not yet uploaded"},
}

var EmptyJSON struct{}

type errorData struct {
	Code  string `json:"errcode"`
	Error string `json:"error,omitempty"`
}

func MakeMatrixError(error mautrix.RespError, message string) mautrix.RespError {
	error.Err = message
	return error
}

func ResponseErrorUnknownJSON(w http.ResponseWriter, r *http.Request, err error) {
	var httpErr gomatrix.HTTPError
	if errors.As(err, &httpErr) {
		hlog.FromRequest(r).Error().
			Err(httpErr.WrappedError).
			Int("code", httpErr.Code).
			Str("message", httpErr.Message).
			Str("contents", string(httpErr.Contents)).
			Msgf("Unknown Matrix error processing request: %s", debug.Stack())
		ResponseRawJSON(w, r, httpErr.Code, httpErr.Contents)
		return
	}

	hlog.FromRequest(r).Err(err).
		Type("type", err).
		Msgf("Unknown error processing request: %s", debug.Stack())
	ResponseErrorJSON(w, r, MUnknown)
}

func ResponseErrorJSON(w http.ResponseWriter, r *http.Request, error mautrix.RespError) {
	ResponseErrorMessageJSON(w, r, error, "")
}

func ResponseErrorMessageJSON(w http.ResponseWriter, r *http.Request, error mautrix.RespError, message string) {
	meta, found := errorToMeta[error.ErrCode]
	if !found {
		panic(fmt.Errorf("missing http status meta for error: %w", error))
	}
	if message == "" {
		message = meta.defaultMsg
	}
	logEv := hlog.FromRequest(r).Error()
	if meta.statusCode < 500 {
		logEv = hlog.FromRequest(r).Warn()
	}
	logEv.
		Str("error_code", error.ErrCode).
		Str("error_message", message).
		Msg("Send response error")
	ResponseJSON(w, r, meta.statusCode, errorData{error.ErrCode, message})
}

func ResponseJSON(w http.ResponseWriter, r *http.Request, statusCode int, data any) {
	addCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		hlog.FromRequest(r).Err(err).Msgf("Failed to marshal output to JSON: %T", data)
	}
}

func ResponseRawJSON(w http.ResponseWriter, r *http.Request, statusCode int, data []byte) {
	addCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	w.Write(data)
}

func addCORSHeaders(w http.ResponseWriter) {
	// Recommended CORS headers can be found in https://spec.matrix.org/v1.3/client-server-api/#web-browser-clients
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Requested-With, Content-Type, Authorization")
}
