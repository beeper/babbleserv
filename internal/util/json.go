package util

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/sjson"
)

func GetJSONSignature(b []byte, key ed25519.PrivateKey) (string, error) {
	var err error
	if b, err = sjson.DeleteBytes(b, "signatures"); err != nil {
		return "", err
	} else if b, err = sjson.DeleteBytes(b, "unsigned"); err != nil {
		return "", err
	}

	// Sign the canonical JSON
	canonicalB, err := gomatrixserverlib.CanonicalJSON(b)
	if err != nil {
		return "", err
	}

	signatureB := ed25519.Sign(key, canonicalB)
	return Base64Encode(signatureB), nil
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
	var err error

	// First, any existing unsigned, signature, and hashes members are removed
	if b, err = sjson.DeleteBytes(b, "signatures"); err != nil {
		return "", err
	} else if b, err = sjson.DeleteBytes(b, "unsigned"); err != nil {
		return "", err
	} else if b, err = sjson.DeleteBytes(b, "hashes"); err != nil {
		return "", err
	}

	// The resulting object is then encoded as Canonical JSON..
	if b, err = gomatrixserverlib.CanonicalJSON(b); err != nil {
		return "", err
	}

	// ..and the JSON is hashed using SHA-256
	sha256Hash := sha256.Sum256(b)

	// The hash is encoded using Unpadded Base64
	return base64.RawStdEncoding.WithPadding(base64.NoPadding).EncodeToString(sha256Hash[:]), nil
}
