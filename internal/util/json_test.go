package util

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/crypto/signatures"
)

func TestOverlaySignedJSON(t *testing.T) {
	object := json.RawMessage(`{"b": 1, "dehydrated":true, "signatures":{"@a:b":{"ed25519:K":"sig"}}, "unsigned":{"x":1}, "a":"A"}`)
	original := slices.Clone(object)
	sigs := signatures.Signatures{"@c:d": {"ed25519:L": "other"}}

	withUnsigned, err := OverlaySignedJSON(object, sigs, map[string]any{"device_display_name": "Phone"})
	require.NoError(t, err)
	assert.Equal(t, `{"b": 1, "dehydrated":true, "signatures":{"@c:d":{"ed25519:L":"other"}}, "unsigned":{"device_display_name":"Phone"}, "a":"A"}`, string(withUnsigned))

	withoutUnsigned, err := OverlaySignedJSON(object, sigs, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"b": 1, "dehydrated":true, "signatures":{"@c:d":{"ed25519:L":"other"}}, "a":"A"}`, string(withoutUnsigned))
	assert.Equal(t, original, object)

	appended, err := OverlaySignedJSON(json.RawMessage(`{"a": [1, 2]}`), signatures.Signatures{}, nil)
	require.NoError(t, err)
	assert.Equal(t, `{"a": [1, 2],"signatures":{}}`, string(appended))

	for _, raw := range []string{``, `null`, `[]`, `"object"`, `{"a":1,"a":2}`, "{\"a\":\"\xff\"}", `{"a":`} {
		_, err := OverlaySignedJSON(json.RawMessage(raw), sigs, nil)
		assert.Error(t, err, raw)
	}
}
