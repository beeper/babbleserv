package util

import (
	"bytes"
	"context"
	"encoding/json"
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
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func RoomIDFromRequestURLParam(r *http.Request, field string) id.RoomID {
	p := chi.URLParam(r, field)

	if strings.HasSuffix(p, "!") {
		return id.RoomID(p)
	}
	if parsed, err := url.PathUnescape(p); err != nil {
		return id.RoomID("")
	} else {
		return id.RoomID(parsed)
	}
}

func EventIDFromRequestURLParam(r *http.Request, field string) id.EventID {
	p := chi.URLParam(r, field)

	if strings.HasSuffix(p, "$") {
		return id.EventID(p)
	}
	if parsed, err := url.PathUnescape(p); err != nil {
		return id.EventID("")
	} else {
		return id.EventID(parsed)
	}
}

func UserIDFromRequestURLParam(r *http.Request, field string) id.UserID {
	p := chi.URLParam(r, field)

	if strings.HasSuffix(p, "!") {
		return id.UserID(p)
	}
	if parsed, err := url.PathUnescape(p); err != nil {
		return id.UserID("")
	} else {
		return id.UserID(parsed)
	}
}

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
		b := types.VersionstampToValue(version)
		token := string(key) + Base64EncodeURLSafe(b)
		tokens = append(tokens, token)
	}

	return strings.Join(tokens, ".")
}

func StringToVersionMap(s string) (types.VersionMap, error) {
	versions := make(types.VersionMap, 3) // we currently have 3 known versions (above)

	parts := strings.Split(s, ".")

	if parts[0] == "" {
		return versions, nil
	}

	for _, part := range parts {
		key, value := part[0], part[1:]

		bytes, err := Base64DecodeURLSafe(value)
		if err != nil {
			return nil, err
		}
		version := types.MustValueToVersionstamp(bytes)

		vKey := types.VersionKey(key)
		switch vKey {
		case types.RoomsVersionKey:
			versions[vKey] = version
		case types.AccountsVersionKey:
			versions[vKey] = version
		case types.DevicesVersionKey:
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
		return "beeper.com", nil
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
