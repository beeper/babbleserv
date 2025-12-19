package util

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
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

// Verify signatures generated as above
func VerifyJSON(b []byte, signingName, keyID string, pubKey ed25519.PublicKey) error {
	extract := struct {
		Signatures map[string]map[string]string `json:"signatures"`
	}{}

	err := json.Unmarshal(b, &extract)
	if err != nil {
		return err
	}

	signature, found := extract.Signatures[signingName][keyID]
	if !found {
		return fmt.Errorf("no signature from %q with ID %q", signingName, keyID)
	}

	// Now strip signatures/unsigned and canonicalize the JSON
	// var err error
	if b, err = sjson.DeleteBytes(b, "signatures"); err != nil {
		return err
	} else if b, err = sjson.DeleteBytes(b, "unsigned"); err != nil {
		return err
	} else if b, err = gomatrixserverlib.CanonicalJSON(b); err != nil {
		return err
	}

	signatureBytes, err := Base64Decode(signature)
	if err != nil {
		return err
	}

	if len(pubKey) != ed25519.PublicKeySize {
		// Guard against ed25519.Panic: "It will panic if len(publicKey) is not PublicKeySize."
		return errors.New("invalid public key size")
	} else if !ed25519.Verify(pubKey, b, signatureBytes) {
		return fmt.Errorf("bad signature from %q with ID %q", signingName, keyID)
	}

	return nil
}

func VerifyObjectWithKeyMap(userID id.UserID, keys mautrix.KeyMap, object any) error {
	zerolog.Ctx(context.TODO()).Trace().
		Str("user_id", userID.String()).
		Any("keys", keys).
		Any("object", object).
		Msg("VerifyObjectWithKeyMap")

	b, _ := json.Marshal(object)

	var err error
	for keyID, keyStr := range keys {
		if !strings.HasPrefix(keyID.String(), "ed25519:") {
			// TODO: support curve25519 signatures - unnecessary?
			continue
		}
		key, _ := Base64Decode(keyStr)
		err = VerifyJSON(b, userID.String(), keyID.String(), ed25519.PublicKey(key))
		if err != nil {
			return err
		}
	}

	return err
}

func VerifyObjectWithEd25519Keys(userID id.UserID, keys map[id.KeyID]id.Ed25519, object any) error {
	keyMap := make(mautrix.KeyMap, len(keys))
	for keyID, keyStr := range keys {
		keyMap[id.DeviceKeyID(keyID)] = keyStr.String()
	}
	return VerifyObjectWithKeyMap(userID, keyMap, object)
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

func CompareSignedObjectsJSON(a, b any) bool {
	aBytes, _ := json.Marshal(a)
	bBytes, _ := json.Marshal(b)
	return CompareSignedJSON(aBytes, bBytes)
}

func CompareSignedJSON(a, b json.RawMessage) bool {
	a, _ = sjson.DeleteBytes(a, "signatures")
	a, _ = sjson.DeleteBytes(a, "unsigned")
	a, _ = gomatrixserverlib.CanonicalJSON(a)

	b, _ = sjson.DeleteBytes(b, "signatures")
	b, _ = sjson.DeleteBytes(b, "unsigned")
	b, _ = gomatrixserverlib.CanonicalJSON(b)

	equal := bytes.Equal(a, b)

	zerolog.Ctx(context.TODO()).Trace().
		Any("A", a).
		Any("B", b).
		Bool("equal", equal).
		Msg("CompareSignedJSON")

	return equal
}
