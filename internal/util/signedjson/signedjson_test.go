package signedjson

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/crypto/signatures"
)

func TestCanonical(t *testing.T) {
	canonical, err := Canonical(json.RawMessage(`{
		"b": 1,
		"unsigned": {"device_display_name": "Phone"},
		"signatures": {"@a:b": {"ed25519:K": "sig"}},
		"a": "A"
	}`))
	require.NoError(t, err)
	assert.Equal(t, `{"a":"A","b":1,"signatures":{"@a:b":{"ed25519:K":"sig"}}}`, string(canonical))

	for _, raw := range []string{``, `null`, `[]`, `{"a":`, `{"a":1,"a":2}`, "{\"a\":\"\xff\"}"} {
		_, err := Canonical(json.RawMessage(raw))
		assert.Error(t, err, raw)
	}
}

func TestMatches(t *testing.T) {
	assert.True(t, Matches(json.RawMessage(`{"b":2,"a":1,"signatures":{"@a:b":{}}}`), json.RawMessage(`{"a":1, "b":2, "unsigned":{}}`)))
	assert.False(t, Matches(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)))
	assert.False(t, Matches(nil, nil))
	assert.False(t, Matches(json.RawMessage(`{"a":`), json.RawMessage(`{"a":`)))
}

func TestCheckReservedNames(t *testing.T) {
	assert.NoError(t, CheckReservedNames(json.RawMessage(`{"signatures":{},"unsigned":{},"signatures_extra":1}`)))
	for _, raw := range []string{`{"Signatures":{}}`, `{"UNSIGNED":{}}`, `{"ſignatures":{}}`} {
		assert.Error(t, CheckReservedNames(json.RawMessage(raw)), raw)
	}
}

func TestVerifyUsesExactNames(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	object := json.RawMessage(`{"a":1}`)
	canonical, err := CanonicalWithout(object, "signatures", "unsigned")
	require.NoError(t, err)
	signature := base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, canonical))

	signed, err := Overlay(object, signatures.Signatures{"@u:example.com": {"ed25519:K": signature}}, nil)
	require.NoError(t, err)
	assert.NoError(t, Verify(signed, "@u:example.com", "ed25519:K", public))

	for name, raw := range map[string]string{
		"case-variant signatures member": `{"a":1,"Signatures":{"@u:example.com":{"ed25519:K":"` + signature + `"}}}`,
		"case-variant signer":            `{"a":1,"signatures":{"@U:example.com":{"ed25519:K":"` + signature + `"}}}`,
		"signature not a string":         `{"a":1,"signatures":{"@u:example.com":{"ed25519:K":5}}}`,
	} {
		assert.Error(t, Verify(json.RawMessage(raw), "@u:example.com", "ed25519:K", public), name)
	}
}

func TestOverlay(t *testing.T) {
	object := json.RawMessage(`{"b": 1, "dehydrated":true, "signatures":{"@a:b":{"ed25519:K":"sig"}}, "unsigned":{"x":1}, "a":"A"}`)
	original := slices.Clone(object)
	sigs := signatures.Signatures{"@c:d": {"ed25519:L": "other"}}

	withUnsigned, err := Overlay(object, sigs, map[string]any{"device_display_name": "Phone"})
	require.NoError(t, err)
	assert.Equal(t, `{"b": 1, "dehydrated":true, "signatures":{"@c:d":{"ed25519:L":"other"}}, "unsigned":{"device_display_name":"Phone"}, "a":"A"}`, string(withUnsigned))

	withoutUnsigned, err := Overlay(object, sigs, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"b": 1, "dehydrated":true, "signatures":{"@c:d":{"ed25519:L":"other"}}, "a":"A"}`, string(withoutUnsigned))
	assert.Equal(t, original, object)

	appended, err := Overlay(json.RawMessage(`{"a": [1, 2]}`), signatures.Signatures{}, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"a": [1, 2],"signatures":{}}`, string(appended))

	for _, raw := range []string{``, `null`, `[]`, `"object"`, `{"a":1,"a":2}`, "{\"a\":\"\xff\"}", `{"a":`} {
		_, err := Overlay(json.RawMessage(raw), sigs, nil)
		assert.Error(t, err, raw)
	}
}
