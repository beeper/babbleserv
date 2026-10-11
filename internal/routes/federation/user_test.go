package federation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/types"
)

func TestUserDevicesResponseServesKeysAsStored(t *testing.T) {
	const (
		laptop = `{"user_id":"@alice:local.example","device_id":"LAPTOP","dehydrated":true,"signatures":{"@alice:local.example":{"ed25519:LAPTOP":"sig"}}}`
		master = `{"user_id":"@alice:local.example","usage":["master"],"keys":{"ed25519:pub":"pub"},"org.example.extra":"signed"}`
	)
	parse := func(raw string) types.SignedJSON[mautrix.DeviceKeys] {
		keys, err := types.ParseSignedJSON[mautrix.DeviceKeys](json.RawMessage(raw))
		require.NoError(t, err)
		return keys
	}
	masterKey, err := types.ParseSignedJSON[mautrix.CrossSigningKeys](json.RawMessage(master))
	require.NoError(t, err)

	resp, err := json.Marshal(userDevicesResponse("@alice:local.example", &types.LocalDeviceSnapshot{
		Version: 4,
		Devices: []types.LocalSnapshotDevice{
			{Device: types.Device{ID: "LAPTOP", DisplayName: "Laptop"}, Keys: parse(laptop)},
			{Device: types.Device{ID: "KEYLESS"}},
		},
		MasterKey: masterKey,
	}))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"devices": [{"keys": `+laptop+`, "device_id": "LAPTOP", "device_display_name": "Laptop"}],
		"master_key": `+master+`,
		"stream_id": 4,
		"user_id": "@alice:local.example"
	}`, string(resp))
	assert.Contains(t, string(resp), `"keys":`+laptop)
	assert.Contains(t, string(resp), `"master_key":`+master)
	assert.NotContains(t, string(resp), "self_signing_key")
}
