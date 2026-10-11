package accounts

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
)

func mustSigned[T any](t *testing.T, raw string) types.SignedJSON[T] {
	t.Helper()
	signed, err := types.ParseSignedJSON[T](json.RawMessage(raw))
	require.NoError(t, err)
	return signed
}

func TestSameSignedJSON(t *testing.T) {
	master := mustSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:m":"m"},"signatures":{"@a:b":{"ed25519:X":"one"}}}`)
	resigned := mustSigned[mautrix.CrossSigningKeys](t, `{"signatures":{"@a:b":{"ed25519:Y":"two"}},"keys":{"ed25519:m":"m"}}`)
	other := mustSigned[mautrix.CrossSigningKeys](t, `{"keys":{"ed25519:n":"n"}}`)

	assert.True(t, sameSignedJSON(types.SignedCrossSigningKey{}, types.SignedCrossSigningKey{}))
	assert.True(t, sameSignedJSON(master, resigned))
	assert.False(t, sameSignedJSON(master, other))
	assert.False(t, sameSignedJSON(master, types.SignedCrossSigningKey{}))
	assert.False(t, sameSignedJSON(types.SignedCrossSigningKey{}, master))
}

// Rows written before keys were kept raw are the mautrix struct with signatures removed, which still
// verifies for a client that signed only the struct's members
func TestOldDeviceKeysRowReadsWithOwnerSignatures(t *testing.T) {
	const userID = id.UserID("@alice:local.example")
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	unsigned, err := json.Marshal(mautrix.DeviceKeys{
		UserID:     userID,
		DeviceID:   "DEVICE",
		Algorithms: []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		Keys:       mautrix.KeyMap{"ed25519:DEVICE": util.Base64Encode(public)},
	})
	require.NoError(t, err)
	signed, err := util.SignJSON(unsigned, userID.String(), "ed25519:DEVICE", private)
	require.NoError(t, err)
	uploaded := mustSigned[mautrix.DeviceKeys](t, string(signed))

	oldRow, err := json.Marshal(uploaded.View)
	require.NoError(t, err)
	oldRow, err = sjson.DeleteBytes(oldRow, "signatures")
	require.NoError(t, err)

	read, err := mustSigned[mautrix.DeviceKeys](t, string(oldRow)).WithOwnerSignatures(userID, uploaded.View.Signatures[userID])
	require.NoError(t, err)
	assert.NoError(t, signedjson.Verify(read.Raw, userID.String(), "ed25519:DEVICE", public))
	assert.Equal(t, uploaded.View, read.View)
}
