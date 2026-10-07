package util

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	remoteKeysUser = id.UserID("@alice:remote.example")

	remotePhoneKeys = `{"user_id": "@alice:remote.example", "device_id": "PHONE", "keys": {"ed25519:PHONE": "key"},
		"signatures": {"@alice:remote.example": {"ed25519:PHONE": "sig"}}, "unsigned": {"device_display_name": "Phone"}}`
	canonicalRemotePhoneKeys = `{"device_id":"PHONE","keys":{"ed25519:PHONE":"key"},"signatures":{"@alice:remote.example":{"ed25519:PHONE":"sig"}},"user_id":"@alice:remote.example"}`
)

func TestParseDeviceListUpdateEDU(t *testing.T) {
	userID, update, err := ParseDeviceListUpdateEDU(json.RawMessage(
		`{"user_id":"@alice:remote.example","device_id":"PHONE","stream_id":5,"prev_id":[4],"device_display_name":"Phone","keys":` + remotePhoneKeys + `}`,
	))
	require.NoError(t, err)
	assert.Equal(t, remoteKeysUser, userID)
	assert.Equal(t, types.RemoteDeviceListUpdate{
		DeviceID: "PHONE",
		StreamID: 5,
		PrevIDs:  []int64{4},
		Device:   types.RemoteDevice{DisplayName: "Phone", Keys: json.RawMessage(canonicalRemotePhoneKeys)},
	}, update)

	bobKeys := strings.Replace(remotePhoneKeys, `"user_id": "@alice:remote.example"`, `"user_id": "@bob:remote.example"`, 1)
	for name, content := range map[string]string{
		"keys for another device": `{"user_id":"@alice:remote.example","device_id":"LAPTOP","stream_id":5,"prev_id":[4],"keys":` + remotePhoneKeys + `}`,
		"keys for another user":   `{"user_id":"@alice:remote.example","device_id":"PHONE","stream_id":5,"prev_id":[4],"keys":` + bobKeys + `}`,
		"malformed":               `{"user_id":"@alice:remote.example","device_id":"PHONE","stream_id":"5","prev_id":[4]}`,
	} {
		_, update, err := ParseDeviceListUpdateEDU(json.RawMessage(content))
		require.NoError(t, err, name)
		assert.True(t, update.Invalid, name)
	}

	_, _, err = ParseDeviceListUpdateEDU(json.RawMessage(`{"user_id":"alice","device_id":"PHONE","stream_id":5}`))
	assert.Error(t, err)
}

func TestParseSigningKeyUpdateEDU(t *testing.T) {
	publicKey := Base64Encode(make([]byte, 32))
	master := `{"keys":{"ed25519:` + publicKey + `":"` + publicKey + `"},"usage":["master"],"user_id":"@alice:remote.example"}`

	_, update, err := ParseSigningKeyUpdateEDU(json.RawMessage(`{"user_id":"@alice:remote.example","master_key":` + master + `,"self_signing_key":` + master + `}`))
	require.NoError(t, err)
	assert.Equal(t, types.RemoteSigningKeyUpdate{MasterKey: json.RawMessage(master), Invalid: true}, update)
}

func TestParseRemoteDeviceSnapshot(t *testing.T) {
	snapshot := func(devices ...string) json.RawMessage {
		return json.RawMessage(`{"user_id":"@alice:remote.example","stream_id":7,"devices":[` + strings.Join(devices, ",") + `]}`)
	}
	longName := strings.Repeat("a", types.MaxRemoteDeviceDisplayNameBytes+1)

	parsed, err := ParseRemoteDeviceSnapshot(remoteKeysUser, snapshot(
		`{"device_id":"PHONE","device_display_name":"Phone","keys":`+remotePhoneKeys+`}`,
		`{"device_id":"LAPTOP","device_display_name":"Laptop","keys":`+remotePhoneKeys+`}`,
		`{"device_id":"KEYLESS","device_display_name":"`+longName+`"}`,
	))
	require.NoError(t, err)
	assert.Equal(t, &types.RemoteDeviceSnapshot{
		StreamID: 7,
		Devices: map[id.DeviceID]types.RemoteDevice{
			"PHONE":   {DisplayName: "Phone", Keys: json.RawMessage(canonicalRemotePhoneKeys)},
			"KEYLESS": {},
		},
	}, parsed)

	tooMany := make([]string, types.MaxRemoteDevicesPerUser+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf(`{"device_id":"D%d"}`, i)
	}
	_, err = ParseRemoteDeviceSnapshot(remoteKeysUser, snapshot(tooMany...))
	assert.Error(t, err)
}
