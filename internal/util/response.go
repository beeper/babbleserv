package util

import (
	"encoding/json"
	"fmt"
	"net/http"

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
)

type errorMeta struct {
	statusCode int
	defaultMsg string
}

var errorToMeta = map[string]errorMeta{
	mautrix.MNotJSON.ErrCode:      {400, "Request body is not valid JSON"},
	mautrix.MMissingToken.ErrCode: {401, ""},
	mautrix.MUnknownToken.ErrCode: {401, ""},
	mautrix.MForbidden.ErrCode:    {403, ""},
	mautrix.MNotFound.ErrCode:     {404, "Nothing found here"},
	MUnknown.ErrCode:              {500, "An unknown error occurred"},
	MMethodNotAllowed.ErrCode:     {405, "Wrong HTTP method"},
}

type errorData struct {
	Code  string `json:"errcode"`
	Error string `json:"error"`
}

func MakeMatrixError(error mautrix.RespError, message string) mautrix.RespError {
	error.Err = message
	return error
}

func ResponseErrorMessageJSON(w http.ResponseWriter, r *http.Request, error mautrix.RespError, message string) {
	meta, found := errorToMeta[error.ErrCode]
	if !found {
		panic(fmt.Errorf("missing http status meta for error: %w", error))
	}
	if message == "" {
		message = meta.defaultMsg
	}
	ResponseJSON(w, r, meta.statusCode, errorData{error.ErrCode, message})
}

func ResponseErrorJSON(w http.ResponseWriter, r *http.Request, error mautrix.RespError) {
	ResponseErrorMessageJSON(w, r, error, "")
}

func ResponseErrorUnknownJSON(w http.ResponseWriter, r *http.Request, err error) {
	hlog.FromRequest(r).Err(err).Msg("Error processing request")
	ResponseErrorJSON(w, r, MUnknown)
}

func ResponseJSON(w http.ResponseWriter, r *http.Request, statusCode int, data any) {
	log := hlog.FromRequest(r)

	addCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Err(err).Msgf("Failed to marshal output to JSON: %T", data)
	}
}

func addCORSHeaders(w http.ResponseWriter) {
	// Recommended CORS headers can be found in https://spec.matrix.org/v1.3/client-server-api/#web-browser-clients
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Requested-With, Content-Type, Authorization")
}
