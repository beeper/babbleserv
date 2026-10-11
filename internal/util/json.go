package util

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"github.com/tidwall/sjson"

	"github.com/beeper/babbleserv/internal/util/signedjson"
)

func GetJSONSignature(b []byte, key ed25519.PrivateKey) (string, error) {
	canonicalB, err := signedjson.CanonicalWithout(b, "signatures", "unsigned")
	if err != nil {
		return "", err
	}
	return Base64Encode(ed25519.Sign(key, canonicalB)), nil
}

// https://spec.matrix.org/v1.10/appendices/#signing-json
func SignJSON(b []byte, signingName, keyID string, key ed25519.PrivateKey) ([]byte, error) {
	preserve := struct {
		Signatures map[string]map[string]string `json:"signatures"`
		Unsigned   json.RawMessage              `json:"unsigned"`
	}{}

	err := json.Unmarshal(b, &preserve)
	if err != nil {
		return nil, err
	}

	signature, err := GetJSONSignature(b, key)
	if err != nil {
		return nil, err
	}

	// Add to our external preserve object
	if preserve.Signatures == nil {
		preserve.Signatures = make(map[string]map[string]string)
	}

	if _, found := preserve.Signatures[signingName]; found {
		preserve.Signatures[signingName][keyID] = signature
	} else {
		preserve.Signatures[signingName] = map[string]string{
			keyID: signature,
		}
	}

	// Now inject signatures/unsigned back into the original message
	if b, err = sjson.SetBytes(b, "signatures", preserve.Signatures); err != nil {
		return nil, err
	}
	if preserve.Unsigned != nil {
		if b, err = sjson.SetRawBytes(b, "unsigned", preserve.Unsigned); err != nil {
			return nil, err
		}
	}

	return b, nil
}

// https://spec.matrix.org/v1.10/server-server-api/#calculating-the-content-hash-for-an-event
func GetJSONContentHash(b []byte) (string, error) {
	canonicalB, err := signedjson.CanonicalWithout(b, "signatures", "unsigned", "hashes")
	if err != nil {
		return "", err
	}
	sha256Hash := sha256.Sum256(canonicalB)

	// The hash is encoded using Unpadded Base64
	return base64.RawStdEncoding.WithPadding(base64.NoPadding).EncodeToString(sha256Hash[:]), nil
}
