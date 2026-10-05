package util

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/go-chi/chi/v5"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Chi URL params

func urlParam(r *http.Request, field string) string {
	p := chi.URLParam(r, field)
	if r.URL.RawPath == "" {
		return p
	} else if parsed, err := url.PathUnescape(p); err == nil {
		return parsed
	}
	return p
}

func RoomIDFromRequestURLParam(r *http.Request, field string) id.RoomID {
	return id.RoomID(urlParam(r, field))
}

func RoomAliasFromRequestURLParam(r *http.Request, field string) id.RoomAlias {
	return id.RoomAlias(urlParam(r, field))
}

func EventIDFromRequestURLParam(r *http.Request, field string) id.EventID {
	return id.EventID(urlParam(r, field))
}

func UserIDFromRequestURLParam(r *http.Request, field string) id.UserID {
	return id.UserID(urlParam(r, field))
}

func StateKeyFromRequestURLParam(r *http.Request, field string) string {
	return urlParam(r, field)
}

func EventTypeFromRequestURLParam(r *http.Request, field string) event.Type {
	return event.NewEventType(urlParam(r, field))
}

func HomeserverForRoomID(roomID id.RoomID) string {
	parts := strings.Split(string(roomID), ":")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

// Query string

func IntFromRequestQuery(r *http.Request, field string, def int) (int, error) {
	str := r.URL.Query().Get(field)
	if str == "" {
		return def, nil
	}
	return strconv.Atoi(str)
}

func VersionMapToString(vMap types.VersionMap) string {
	tokens := make([]string, 0, len(vMap))

	for key, version := range vMap {
		b := types.MustVersionstampToBytes(version)
		token := string(key) + Base32HexEncode(b)
		tokens = append(tokens, token)
	}

	return strings.Join(tokens, ".")
}

func StringToVersionMap(s string) (types.VersionMap, error) {
	versions := make(types.VersionMap, 3) // we currently have 3 known versions (above)

	parts := strings.Split(s, ".")

	if s == "" {
		return versions, nil
	}

	for _, part := range parts {
		if len(part) < 2 {
			return nil, fmt.Errorf("invalid version component %q", part)
		}
		key, value := part[0], part[1:]

		bytes, err := Base32HexDecode(value)
		if err != nil {
			return nil, err
		}
		// Only unpack one complete versionstamp tuple (type byte + 12 bytes).
		// The FDB tuple decoder can panic on malformed values of other types.
		if len(bytes) != 13 || bytes[0] != 0x33 {
			return nil, types.ErrInvalidVersion
		}
		version, err := types.BytesToVersionstamp(bytes)
		if err != nil {
			return nil, err
		}
		if types.IsIncompleteVersionstamp(version) {
			return nil, types.ErrInvalidVersion
		}

		vKey := types.VersionKey(key)
		switch vKey {
		case types.RoomsVersionKey:
			versions[vKey] = version
		case types.AccountsVersionKey:
			versions[vKey] = version
		case types.TransientVersionKey:
			versions[vKey] = version
		default:
			return nil, fmt.Errorf("invalid versions key: %s", string(key))
		}
	}

	return versions, nil
}

func VersionMapFromRequestQuery(r *http.Request, field types.VersionKey) (types.VersionMap, error) {
	return StringToVersionMap(r.URL.Query().Get(string(field)))
}

func VersionFromRequestQuery(r *http.Request, field, versionKey types.VersionKey) (tuple.Versionstamp, error) {
	if versionMap, err := VersionMapFromRequestQuery(r, field); err != nil {
		return types.ZeroVersionstamp, err
	} else {
		return versionMap[versionKey], nil
	}
}

// Request body

func ParseRequestJSON[T any](r *http.Request) (T, *mautrix.RespError) {
	return parseRequestJSON[T](r, false)
}

func ParseOptionalRequestJSON[T any](r *http.Request) (T, *mautrix.RespError) {
	return parseRequestJSON[T](r, true)
}

func parseRequestJSON[T any](r *http.Request, optional bool) (T, *mautrix.RespError) {
	var req T
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return req, requestReadError(err)
	}
	if optional && len(body) == 0 {
		return req, nil
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		if optional {
			return req, nil
		}
		return req, &mautrix.MBadJSON
	}
	// We want to match how Python handles duplicate keys in JSON - go's jsonv2 merges nested values
	// when allowing duplicates whereas Python overwrites. Catch the duplicate error from default
	// decode and then use our own function that behaves like Python.
	err = jsonv2.Unmarshal(body, &req, jsontext.AllowInvalidUTF8(true))
	if errors.Is(err, jsontext.ErrDuplicateName) {
		body, err = collapseDuplicateJSONKeys(body)
		if err == nil {
			var zero T
			req = zero
			err = jsonv2.Unmarshal(body, &req, jsontext.AllowInvalidUTF8(true))
		}
	}
	var semanticErr *jsonv2.SemanticError
	if errors.As(err, &semanticErr) {
		return req, &mautrix.MBadJSON
	} else if err != nil {
		return req, &mautrix.MNotJSON
	}
	return req, nil
}

// collapseDuplicateKeys reads JSON as Python does, the last value of a duplicate key winning
func collapseDuplicateJSONKeys(b []byte) ([]byte, error) {
	// Decoding into a struct would merge the duplicates of a map field
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, err
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func requestReadError(err error) *mautrix.RespError {
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		return &mautrix.MTooLarge
	}
	return &mautrix.MNotJSON
}

// Federation request authentication
// https://matrix.org/docs/spec/server_server/unstable.html#request-authentication

type federationRequest struct {
	Method  string          `json:"method"`
	URI     string          `json:"uri"`
	Content json.RawMessage `json:"content,omitempty"`

	Destination spec.ServerName `json:"destination"`
	Origin      spec.ServerName `json:"origin"`

	Signatures map[spec.ServerName]map[gomatrixserverlib.KeyID]string `json:"signatures,omitempty"`
}

func VerifyFederatonRequest(
	ctx context.Context,
	serverName string,
	keyStore *KeyStore,
	r *http.Request,
) (string, error) {
	if r.Host == "localhost:5000" {
		return serverName, nil
	}

	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}

	// Close the original body and replace with the bytes we just read in, making
	// the request look like the original to the handler.
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewBuffer(b))

	req := federationRequest{
		Method:  r.Method,
		URI:     r.URL.RequestURI(),
		Content: b,
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", errors.New("missing auth header")
	}

	scheme, origin, destination, keyID, signature := fclient.ParseAuthorization(authHeader)
	if scheme != "X-Matrix" {
		return "", fmt.Errorf("invalid auth scheme: %s", scheme)
	} else if origin == "" || keyID == "" || signature == "" {
		return "", errors.New("invalid X-Matrix auth header")
	}

	if destination != "" && string(destination) != serverName {
		return "", fmt.Errorf("invalid auth destination: %s", destination)
	}

	req.Origin = origin
	req.Destination = destination
	req.Signatures = map[spec.ServerName]map[gomatrixserverlib.KeyID]string{origin: {keyID: signature}}

	reqB, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	if err := keyStore.VerifyJSONFromServer(ctx, string(origin), reqB); err != nil {
		return "", err
	}

	return string(origin), nil
}
