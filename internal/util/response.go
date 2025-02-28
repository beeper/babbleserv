package util

import (
	"encoding/json"
	"fmt"
	"net/http"

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
	mautrix.MInvalidParam.ErrCode: {400, ""},

	mautrix.MMissingToken.ErrCode: {401, ""},
	mautrix.MUnknownToken.ErrCode: {401, ""},
	MUnauthorized.ErrCode:         {401, ""},
	mautrix.MForbidden.ErrCode:    {403, ""},
	MUnprocessableContent.ErrCode: {422, ""},

	mautrix.MNotFound.ErrCode: {404, "Nothing found here"},
	MMethodNotAllowed.ErrCode: {405, "Wrong HTTP method"},

	MUnknown.ErrCode:        {500, "An unknown error occurred"},
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
	if httpErr, ok := err.(gomatrix.HTTPError); ok {
		hlog.FromRequest(r).Err(httpErr.WrappedError).Msg("Matrix error processing request")
		ResponseRawJSON(w, r, httpErr.Code, httpErr.Contents)
		return
	}
	hlog.FromRequest(r).Err(err).Msg("Unknown error processing request")
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
	hlog.FromRequest(r).Error().
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
