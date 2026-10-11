package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"
)

func mustParseSigned[T any](t *testing.T, raw string) SignedJSON[T] {
	t.Helper()
	signed, err := ParseSignedJSON[T](json.RawMessage(raw))
	require.NoError(t, err)
	return signed
}

func TestSignedJSONViewIsCaseSensitive(t *testing.T) {
	keys := mustParseSigned[mautrix.DeviceKeys](t, `{"User_ID":"@alice:example.com","device_id":"DEVICE","Signatures":{"@bob:example.com":{"ed25519:X":"sig"}}}`)
	assert.Empty(t, keys.View.UserID)
	assert.Equal(t, id.DeviceID("DEVICE"), keys.View.DeviceID)
	assert.Empty(t, keys.View.Signatures)
}

func TestSignedJSONMarshalsRaw(t *testing.T) {
	raw := `{ "device_id" : "DEVICE", "dehydrated": true, "org.example.extra": [1, 2] }`
	content := DeviceListUpdateEDUContent{DeviceID: "DEVICE", DeviceKeys: mustParseSigned[mautrix.DeviceKeys](t, raw)}
	b, err := json.Marshal(content)
	require.NoError(t, err)
	assert.JSONEq(t, `{"device_id":"DEVICE","user_id":"","stream_id":0,"keys":`+raw+`}`, string(b))
	assert.Contains(t, string(b), `"keys":{"device_id":"DEVICE","dehydrated":true,"org.example.extra":[1,2]}`)

	var decoded DeviceListUpdateEDUContent
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, content.DeviceKeys.View, decoded.DeviceKeys.View)
}

func TestSignedJSONAbsent(t *testing.T) {
	for _, raw := range []string{``, `null`, ` null `} {
		assert.True(t, mustParseSigned[mautrix.CrossSigningKeys](t, raw).IsZero(), raw)
	}

	b, err := json.Marshal(SigningKeyUpdateEDUContent{UserID: "@alice:example.com"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"user_id":"@alice:example.com"}`, string(b))

	var decoded SigningKeyUpdateEDUContent
	require.NoError(t, json.Unmarshal([]byte(`{"user_id":"@alice:example.com","master_key":null}`), &decoded))
	assert.True(t, decoded.MasterKey.IsZero())
}

func TestUserCrossSigningKeysOldRow(t *testing.T) {
	const master = `{"user_id":"@alice:example.com","usage":["master"],"keys":{"ed25519:pub":"pub"}}`
	var keys UserCrossSigningKeys
	require.NoError(t, json.Unmarshal([]byte(`{
		"master_key": `+master+`,
		"self_signing_key": {"user_id":"","usage":null,"keys":null},
		"user_signing_key": {"user_id":"","usage":null,"keys":null}
	}`), &keys))

	assert.Equal(t, master, string(keys.Master.Raw))
	assert.Equal(t, id.Ed25519("pub"), CrossSigningPublicKey(keys.Master))
	assert.True(t, keys.SelfSigning.IsZero())
	assert.True(t, keys.UserSigning.IsZero())

	b, err := json.Marshal(keys)
	require.NoError(t, err)
	assert.JSONEq(t, `{"master_key":`+master+`}`, string(b))
}

func TestUserCrossSigningKeysNewRow(t *testing.T) {
	keys := UserCrossSigningKeys{
		Master:      mustParseSigned[mautrix.CrossSigningKeys](t, `{"user_id":"@alice:example.com","usage":["master"],"keys":{"ed25519:m":"m"},"org.example.extra":"signed","signatures":{"@alice:example.com":{"ed25519:DEVICE":"sig"}}}`),
		UserSigning: mustParseSigned[mautrix.CrossSigningKeys](t, `{"user_id":"@alice:example.com","usage":["user_signing"],"keys":{"ed25519:u":"u"}}`),
	}
	for _, key := range []*SignedCrossSigningKey{&keys.Master, &keys.SelfSigning, &keys.UserSigning} {
		var err error
		*key, err = key.WithoutSignatures()
		require.NoError(t, err)
	}
	b, err := json.Marshal(keys)
	require.NoError(t, err)

	var decoded UserCrossSigningKeys
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, keys, decoded)
	assert.Equal(t, `{"user_id":"@alice:example.com","usage":["master"],"keys":{"ed25519:m":"m"},"org.example.extra":"signed"}`, string(decoded.Master.Raw))
	assert.True(t, decoded.SelfSigning.IsZero())
}

func TestFallbackKeyOldRow(t *testing.T) {
	const key = `{"key":"curve","fallback":true,"signatures":{"@alice:example.com":{"ed25519:DEVICE":"sig"}}}`
	var fallback FallbackKey
	require.NoError(t, json.Unmarshal([]byte(`{"Key":`+key+`,"KeyID":"signed_curve25519:A","Used":true}`), &fallback))
	assert.Equal(t, key, string(fallback.Key.Raw))
	assert.True(t, fallback.Key.View.Fallback)
	assert.Equal(t, id.KeyID("signed_curve25519:A"), fallback.KeyID)
	assert.True(t, fallback.Used)
}

func TestUserCrossSigningKeysMerge(t *testing.T) {
	stored := UserCrossSigningKeys{
		Master:      mustParseSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:m1":"m1"}}`),
		SelfSigning: mustParseSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:s1":"s1"}}`),
		UserSigning: mustParseSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:u1":"u1"}}`),
	}
	selfSigning := mustParseSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:s2":"s2"}}`)

	merged := stored.Merge(UserCrossSigningKeys{SelfSigning: selfSigning})
	assert.Equal(t, UserCrossSigningKeys{Master: stored.Master, SelfSigning: selfSigning, UserSigning: stored.UserSigning}, merged)
	assert.Equal(t, UserCrossSigningKeys{SelfSigning: selfSigning}, UserCrossSigningKeys{}.Merge(UserCrossSigningKeys{SelfSigning: selfSigning}))
}

func TestSignedJSONStoredAndRead(t *testing.T) {
	keys := mustParseSigned[mautrix.DeviceKeys](t, `{"device_id":"DEVICE","dehydrated":true,"signatures":{"@alice:example.com":{"ed25519:DEVICE":"sig"}},"unsigned":{"x":1}}`)

	stored, err := keys.WithoutSignatures()
	require.NoError(t, err)
	assert.Equal(t, `{"device_id":"DEVICE","dehydrated":true,"unsigned":{"x":1}}`, string(stored.Raw))
	assert.Empty(t, stored.View.Signatures)

	read, err := stored.WithOwnerSignatures("@alice:example.com", keys.View.Signatures["@alice:example.com"])
	require.NoError(t, err)
	assert.Equal(t, `{"device_id":"DEVICE","dehydrated":true,"signatures":{"@alice:example.com":{"ed25519:DEVICE":"sig"}}}`, string(read.Raw))
	assert.Equal(t, keys.View.Signatures, read.View.Signatures)
	assert.True(t, read.Matches(keys.Raw))

	unsigned, err := stored.WithOwnerSignatures("@alice:example.com", nil)
	require.NoError(t, err)
	assert.Equal(t, signatures.Signatures{}, unsigned.View.Signatures)
}
