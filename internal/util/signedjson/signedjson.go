// Package signedjson works on Matrix signed JSON objects as bytes, so members a struct doesn't know
// are never lost. It sits below types, whose SignedJSON wraps it, and util.
package signedjson

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/crypto/signatures"
)

// Signing, hashing and comparison all work on canonical JSON with some members removed
func CanonicalWithout(b []byte, members ...string) ([]byte, error) {
	var err error
	for _, member := range members {
		if b, err = sjson.DeleteBytes(b, member); err != nil {
			return nil, err
		}
	}
	return gomatrixserverlib.CanonicalJSON(b)
}

// Canonical returns a signed object as canonical JSON without unsigned, the form keys are stored and
// compared in, with any signatures kept. Duplicate names are rejected because gjson and
// encoding/json would resolve them differently.
func Canonical(raw json.RawMessage) (json.RawMessage, error) {
	if !jsontext.Value(raw).IsValid() {
		return nil, errors.New("signed object is not valid JSON or has duplicate names")
	} else if !gjson.ParseBytes(raw).IsObject() {
		return nil, errors.New("signed object is not a JSON object")
	}
	return CanonicalWithout(raw, "unsigned")
}

// CheckReservedNames rejects a member whose name only case-folds to signatures or unsigned:
// canonicalisation and comparison keep it as content, while encoding/json reads it as the real one
func CheckReservedNames(object json.RawMessage) error {
	var err error
	gjson.ParseBytes(object).ForEach(func(name, _ gjson.Result) bool {
		for _, reserved := range []string{"signatures", "unsigned"} {
			if name.Str != reserved && strings.EqualFold(name.Str, reserved) {
				err = fmt.Errorf("member %q is a case variant of %q", name.Str, reserved)
				return false
			}
		}
		return true
	})
	return err
}

// https://spec.matrix.org/v1.10/appendices/#checking-for-a-signature. The signature is looked up by
// exact names, so it is the one canonicalisation removes and the object carries.
func Verify(b []byte, signingName, keyID string, pubKey ed25519.PublicKey) error {
	found := gjson.GetBytes(b, "signatures."+gjson.Escape(signingName)+"."+gjson.Escape(keyID))
	if found.Type != gjson.String {
		return fmt.Errorf("no signature from %q with ID %q", signingName, keyID)
	}
	signature := found.Str

	canonicalB, err := CanonicalWithout(b, "signatures", "unsigned")
	if err != nil {
		return err
	}

	signatureBytes, err := base64.RawStdEncoding.DecodeString(signature)
	if err != nil {
		return err
	}

	if len(pubKey) != ed25519.PublicKeySize {
		// Guard against ed25519.Panic: "It will panic if len(publicKey) is not PublicKeySize."
		return errors.New("invalid public key size")
	} else if !ed25519.Verify(pubKey, canonicalB, signatureBytes) {
		return fmt.Errorf("bad signature from %q with ID %q", signingName, keyID)
	}

	return nil
}

// Matches reports whether both objects have the same signed content. Inputs that are not JSON never
// match.
func Matches(a, b json.RawMessage) bool {
	a, errA := CanonicalWithout(a, "signatures", "unsigned")
	b, errB := CanonicalWithout(b, "signatures", "unsigned")
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// Overlay returns a copy of a signed object with its signatures replaced and its unsigned set, or
// removed when nil. Every other member is kept byte for byte so signatures over members we don't
// know still verify.
func Overlay(object json.RawMessage, sigs signatures.Signatures, unsigned map[string]any) (json.RawMessage, error) {
	if !jsontext.Value(object).IsValid() || !gjson.ParseBytes(object).IsObject() {
		return nil, errors.New("signed object is not a valid JSON object")
	}
	out, err := sjson.SetBytes(object, "signatures", sigs)
	if err != nil {
		return nil, err
	} else if unsigned == nil {
		return sjson.DeleteBytes(out, "unsigned")
	}
	return sjson.SetBytes(out, "unsigned", unsigned)
}
