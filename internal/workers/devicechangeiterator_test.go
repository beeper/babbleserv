package workers

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
)

const testEDUUser = id.UserID("@alice:local.example")

func newTestEDUKey(t *testing.T) (string, ed25519.PrivateKey) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return util.Base64Encode(public), private
}

func mustSigned[T any](t *testing.T, raw json.RawMessage) types.SignedJSON[T] {
	t.Helper()
	signed, err := types.ParseSignedJSON[T](raw)
	require.NoError(t, err)
	return signed
}

// Our own ingest accepts what we send, so a signed member the structs don't know still verifies
func TestDeviceListUpdateContentKeepsUnknownSignedMembers(t *testing.T) {
	public, private := newTestEDUKey(t)
	unsigned, err := json.Marshal(map[string]any{
		"user_id":    testEDUUser,
		"device_id":  "LAPTOP",
		"algorithms": []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		"keys":       map[string]string{"ed25519:LAPTOP": public},
		"dehydrated": true,
	})
	require.NoError(t, err)
	signed, err := util.SignJSON(unsigned, testEDUUser.String(), "ed25519:LAPTOP", private)
	require.NoError(t, err)
	keys := mustSigned[mautrix.DeviceKeys](t, signed)

	update := types.LocalDeviceListUpdate{Version: 3, Device: &types.Device{ID: "LAPTOP"}, Keys: keys}
	content, err := json.Marshal(deviceListUpdateContent(testEDUUser, "LAPTOP", update, &types.DeviceListStream{StreamID: 3, PrevID: 2}))
	require.NoError(t, err)

	userID, parsed, err := util.ParseDeviceListUpdateEDU(content)
	require.NoError(t, err)
	assert.Equal(t, testEDUUser, userID)
	assert.False(t, parsed.Invalid)
	assert.True(t, parsed.Device.Keys.Matches(keys.Raw))
	assert.NoError(t, signedjson.Verify(parsed.Device.Keys.Raw, testEDUUser.String(), "ed25519:LAPTOP", private.Public().(ed25519.PublicKey)))
}

// A user without a self-signing key sends only the master key, which our own ingest accepts
func TestSigningKeyUpdateContentOmitsAbsentKey(t *testing.T) {
	public, _ := newTestEDUKey(t)
	master, err := json.Marshal(map[string]any{
		"user_id":           testEDUUser,
		"usage":             []id.CrossSigningUsage{id.XSUsageMaster},
		"keys":              map[string]string{"ed25519:" + public: public},
		"org.example.extra": "signed",
	})
	require.NoError(t, err)
	keys := &types.UserCrossSigningKeys{Master: mustSigned[mautrix.CrossSigningKeys](t, master)}

	content, err := json.Marshal(signingKeyUpdateContent(testEDUUser, keys))
	require.NoError(t, err)
	assert.JSONEq(t, `{"user_id":"`+testEDUUser.String()+`","master_key":`+string(master)+`}`, string(content))

	userID, parsed, err := util.ParseSigningKeyUpdateEDU(content)
	require.NoError(t, err)
	assert.Equal(t, testEDUUser, userID)
	assert.False(t, parsed.Invalid)
	assert.True(t, parsed.MasterKey.Matches(master))
	assert.True(t, parsed.SelfSigningKey.IsZero())
}
