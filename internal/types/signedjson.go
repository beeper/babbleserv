package types

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util/signedjson"
)

// SignedJSON is a signed key object kept as the raw JSON it was received or stored as, with a typed
// view decoded from those same bytes. It always marshals as the raw bytes, never through the view,
// so signed members the view doesn't know survive. The zero value is an absent object.
type SignedJSON[T any] struct {
	Raw  json.RawMessage
	View T
}

type (
	SignedDeviceKeys      = SignedJSON[mautrix.DeviceKeys]
	SignedCrossSigningKey = SignedJSON[mautrix.CrossSigningKeys]
	SignedOneTimeKey      = SignedJSON[OneTimeKey]
	SignatureUploadTarget = SignedJSON[mautrix.ReqKeysSignatures]
)

// OneTimeKey is the view of a signed one-time or fallback key
type OneTimeKey struct {
	Fallback bool `json:"fallback"`
}

// ParseSignedJSON keeps raw as is and decodes the view from it, matching names case-sensitively so a
// case-variant member never fills a field the object doesn't have. Empty or null is absent.
func ParseSignedJSON[T any](raw json.RawMessage) (SignedJSON[T], error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return SignedJSON[T]{}, nil
	}
	var view T
	if err := jsonv2.Unmarshal(raw, &view); err != nil {
		return SignedJSON[T]{}, err
	}
	return SignedJSON[T]{Raw: raw, View: view}, nil
}

func (s *SignedJSON[T]) UnmarshalJSON(data []byte) error {
	parsed, err := ParseSignedJSON[T](slices.Clone(data))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

func (s SignedJSON[T]) MarshalJSON() ([]byte, error) {
	if s.IsZero() {
		return []byte("null"), nil
	}
	return s.Raw, nil
}

func (s SignedJSON[T]) IsZero() bool {
	return len(s.Raw) == 0
}

func (s SignedJSON[T]) Canonical() (SignedJSON[T], error) {
	raw, err := signedjson.Canonical(s.Raw)
	if err != nil {
		return SignedJSON[T]{}, err
	}
	return ParseSignedJSON[T](raw)
}

func (s SignedJSON[T]) Overlay(sigs signatures.Signatures, unsigned map[string]any) (json.RawMessage, error) {
	return signedjson.Overlay(s.Raw, sigs, unsigned)
}

// WithoutSignatures returns the object as stored, its signatures being stored apart from it
func (s SignedJSON[T]) WithoutSignatures() (SignedJSON[T], error) {
	if s.IsZero() {
		return s, nil
	}
	raw, err := sjson.DeleteBytes(s.Raw, "signatures")
	if err != nil {
		return SignedJSON[T]{}, err
	}
	return ParseSignedJSON[T](raw)
}

// WithOwnerSignatures returns a stored object with its owner's signatures, read apart from it, and
// without unsigned
func (s SignedJSON[T]) WithOwnerSignatures(userID id.UserID, owner map[id.KeyID]string) (SignedJSON[T], error) {
	if s.IsZero() {
		return s, nil
	}
	sigs := make(signatures.Signatures, 1)
	if len(owner) > 0 {
		sigs[userID] = owner
	}
	raw, err := s.Overlay(sigs, nil)
	if err != nil {
		return SignedJSON[T]{}, err
	}
	return ParseSignedJSON[T](raw)
}

// Verify checks userID's signature by the key, an unpadded base64 public key, over the raw bytes
func (s SignedJSON[T]) Verify(userID id.UserID, keyID, publicKey string) error {
	key, err := base64.RawStdEncoding.DecodeString(publicKey)
	if err != nil {
		return fmt.Errorf("invalid public key %q: %w", keyID, err)
	}
	return signedjson.Verify(s.Raw, userID.String(), keyID, key)
}

func (s SignedJSON[T]) Matches(other json.RawMessage) bool {
	return signedjson.Matches(s.Raw, other)
}

// CrossSigningPublicKey is the public key a cross-signing key's signatures are indexed by, empty
// when absent
func CrossSigningPublicKey(key SignedCrossSigningKey) id.Ed25519 {
	return key.View.FirstKey()
}

// VerifyKeys checks the object carries userID's signature by each ed25519 key, by key ID to unpadded
// base64 public key. Other algorithms are skipped, there being nothing to verify them with.
func VerifyKeys[T any, K, V ~string](s SignedJSON[T], userID id.UserID, keys map[K]V) error {
	for keyID, publicKey := range keys {
		if !strings.HasPrefix(string(keyID), "ed25519:") {
			continue
		}
		if err := s.Verify(userID, string(keyID), string(publicKey)); err != nil {
			return err
		}
	}
	return nil
}
