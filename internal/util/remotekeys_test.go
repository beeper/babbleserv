package util

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	remoteKeysUser  = id.UserID("@alice:remote.example")
	remoteKeysOther = id.UserID("@bob:other.example")
)

type remoteTestKey struct {
	public  string
	private ed25519.PrivateKey
}

func newRemoteTestKey(t *testing.T) remoteTestKey {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return remoteTestKey{public: Base64Encode(public), private: private}
}

func (k remoteTestKey) keyID() string {
	return "ed25519:" + k.public
}

func (k remoteTestKey) sign(t *testing.T, object json.RawMessage, signer id.UserID, keyID string) json.RawMessage {
	signed, err := SignJSON(object, signer.String(), keyID, k.private)
	require.NoError(t, err)
	return signed
}

func testJSON(t *testing.T, object map[string]any, overrides map[string]any, omit ...string) json.RawMessage {
	maps.Copy(object, overrides)
	for _, field := range omit {
		delete(object, field)
	}
	raw, err := json.Marshal(object)
	require.NoError(t, err)
	return raw
}

func setTestField(t *testing.T, raw json.RawMessage, path string, value any) json.RawMessage {
	out, err := sjson.SetBytes(raw, path, value)
	require.NoError(t, err)
	return out
}

func replaceTestJSON(t *testing.T, raw json.RawMessage, old, new string) json.RawMessage {
	require.Contains(t, string(raw), old)
	return json.RawMessage(strings.Replace(string(raw), old, new, 1))
}

func deviceKeysJSON(t *testing.T, deviceID id.DeviceID, key remoteTestKey, overrides map[string]any) json.RawMessage {
	return testJSON(t, map[string]any{
		"user_id":    remoteKeysUser,
		"device_id":  deviceID,
		"algorithms": []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		"keys": map[string]string{
			"ed25519:" + string(deviceID):    key.public,
			"curve25519:" + string(deviceID): Base64Encode([]byte("curve25519 public key 32 bytes!!")),
		},
	}, overrides)
}

func signedDeviceKeys(t *testing.T, deviceID id.DeviceID, key remoteTestKey, overrides map[string]any) json.RawMessage {
	return key.sign(t, deviceKeysJSON(t, deviceID, key, overrides), remoteKeysUser, "ed25519:"+string(deviceID))
}

// Signs as the owner without SignJSON, which would merge a case-variant Signatures member into the
// signatures it writes
func ownerSigned(t *testing.T, unsigned json.RawMessage, key remoteTestKey, keyID string, others map[id.UserID]any) json.RawMessage {
	signature, err := GetJSONSignature(unsigned, key.private)
	require.NoError(t, err)
	signatures := map[id.UserID]any{remoteKeysUser: map[string]string{keyID: signature}}
	maps.Copy(signatures, others)
	return setTestField(t, unsigned, "signatures", signatures)
}

func crossSigningKeyJSON(t *testing.T, key remoteTestKey, usage id.CrossSigningUsage, overrides map[string]any) json.RawMessage {
	return testJSON(t, map[string]any{
		"user_id": remoteKeysUser,
		"usage":   []id.CrossSigningUsage{usage},
		"keys":    map[string]string{key.keyID(): key.public},
	}, overrides)
}

func mustCanonical(t *testing.T, raw json.RawMessage) json.RawMessage {
	canonical, err := canonicalSignedJSON(raw)
	require.NoError(t, err)
	return canonical
}

func signers(t *testing.T, raw json.RawMessage) map[id.UserID]map[id.KeyID]string {
	var object struct {
		Signatures map[id.UserID]map[id.KeyID]string `json:"signatures"`
	}
	require.NoError(t, json.Unmarshal(raw, &object))
	return object.Signatures
}

func mustDecodeKey(t *testing.T, encoded string) ed25519.PublicKey {
	key, err := Base64Decode(encoded)
	require.NoError(t, err)
	return key
}

func TestCanonicalSignedJSON(t *testing.T) {
	canonical, err := canonicalSignedJSON(json.RawMessage(`{
		"b": 1,
		"unsigned": {"device_display_name": "Phone"},
		"signatures": {"@a:b": {"ed25519:K": "sig"}},
		"a": "A"
	}`))
	require.NoError(t, err)
	assert.Equal(t, `{"a":"A","b":1,"signatures":{"@a:b":{"ed25519:K":"sig"}}}`, string(canonical))

	for _, raw := range []string{``, `null`, `[]`, `{"a":`, `{"a":1,"a":2}`, "{\"a\":\"\xff\"}"} {
		_, err := canonicalSignedJSON(json.RawMessage(raw))
		assert.Error(t, err, raw)
	}
}

func TestValidateRemoteDeviceKeys(t *testing.T) {
	device := newRemoteTestKey(t)
	selfSigning := newRemoteTestKey(t)
	foreign := newRemoteTestKey(t)
	unsigned := deviceKeysJSON(t, "DEVICE", device, nil)
	valid := signedDeviceKeys(t, "DEVICE", device, nil)
	crossSigned := selfSigning.sign(t, valid, remoteKeysUser, selfSigning.keyID())

	for name, tc := range map[string]struct {
		raw      json.RawMessage
		deviceID id.DeviceID
		valid    bool
		unsigned bool
	}{
		"valid":                                   {valid, "DEVICE", true, false},
		"owner cross-signature kept":              {crossSigned, "DEVICE", true, false},
		"foreign signature stripped":              {foreign.sign(t, valid, remoteKeysOther, foreign.keyID()), "DEVICE", true, false},
		"malformed foreign signature":             {ownerSigned(t, unsigned, device, "ed25519:DEVICE", map[id.UserID]any{remoteKeysOther: "garbage"}), "DEVICE", true, false},
		"extra signed fields":                     {signedDeviceKeys(t, "DEVICE", device, map[string]any{"dehydrated": true}), "DEVICE", true, false},
		"unsigned metadata":                       {setTestField(t, valid, "unsigned", map[string]any{"device_display_name": "Phone"}), "DEVICE", true, false},
		"mismatched user ID":                      {signedDeviceKeys(t, "DEVICE", device, map[string]any{"user_id": remoteKeysOther}), "DEVICE", false, false},
		"mismatched device":                       {valid, "OTHER", false, false},
		"missing ed25519 key":                     {signedDeviceKeys(t, "DEVICE", device, map[string]any{"keys": map[string]string{"curve25519:DEVICE": device.public}}), "DEVICE", false, false},
		"short ed25519 key":                       {signedDeviceKeys(t, "DEVICE", device, map[string]any{"keys": map[string]string{"ed25519:DEVICE": Base64Encode(make([]byte, 16))}}), "DEVICE", false, false},
		"content changed":                         {setTestField(t, valid, "algorithms", []string{"m.other"}), "DEVICE", false, false},
		"signed by another key":                   {foreign.sign(t, unsigned, remoteKeysUser, "ed25519:DEVICE"), "DEVICE", false, false},
		"no signatures":                           {unsigned, "DEVICE", true, true},
		"empty signatures":                        {setTestField(t, valid, "signatures", map[string]any{}), "DEVICE", true, true},
		"owner signatures without self-signature": {setTestField(t, valid, "signatures", map[id.UserID]any{remoteKeysUser: map[string]string{}}), "DEVICE", false, false},
		"signatures not an object":                {setTestField(t, valid, "signatures", "signed"), "DEVICE", false, false},
		"oversize":                                {signedDeviceKeys(t, "DEVICE", device, map[string]any{"padding": strings.Repeat("x", types.MaxRemoteKeyObjectBytes)}), "DEVICE", false, false},
		"duplicate names":                         {replaceTestJSON(t, valid, "{", `{"device_id":"OTHER",`), "DEVICE", false, false},
		"case-variant foreign signatures": {ownerSigned(t, deviceKeysJSON(t, "DEVICE", device, map[string]any{
			"Signatures": map[id.UserID]map[string]string{remoteKeysOther: {"ed25519:X": "c2ln"}},
		}), device, "ed25519:DEVICE", nil), "DEVICE", false, false},
	} {
		keys, err := validateRemoteDeviceKeys(remoteKeysUser, tc.deviceID, tc.raw)
		if !tc.valid {
			assert.Error(t, err, name)
			assert.Nil(t, keys, name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, keys, mustCanonical(t, keys), name)
		assert.False(t, gjson.GetBytes(keys, "unsigned").Exists(), name)
		if tc.unsigned {
			assert.False(t, gjson.GetBytes(keys, "signatures").Exists(), name)
			continue
		}
		assert.Equal(t, []id.UserID{remoteKeysUser}, slices.Collect(maps.Keys(signers(t, keys))), name)
		assert.NoError(t, VerifyJSON(keys, remoteKeysUser.String(), "ed25519:DEVICE", mustDecodeKey(t, device.public)), name)
	}

	keys, err := validateRemoteDeviceKeys(remoteKeysUser, "DEVICE", crossSigned)
	require.NoError(t, err)
	assert.Len(t, signers(t, keys)[remoteKeysUser], 2)
}

func TestValidateRemoteCrossSigningKey(t *testing.T) {
	master := newRemoteTestKey(t)
	selfSigning := newRemoteTestKey(t)
	foreign := newRemoteTestKey(t)
	masterKey := crossSigningKeyJSON(t, master, id.XSUsageMaster, nil)
	selfSigningKey := master.sign(t, crossSigningKeyJSON(t, selfSigning, id.XSUsageSelfSigning, nil), remoteKeysUser, master.keyID())

	for name, tc := range map[string]struct {
		raw   json.RawMessage
		usage id.CrossSigningUsage
		valid bool
	}{
		"unsigned master":                 {masterKey, id.XSUsageMaster, true},
		"self-signing":                    {selfSigningKey, id.XSUsageSelfSigning, true},
		"foreign signature stripped":      {foreign.sign(t, masterKey, remoteKeysOther, foreign.keyID()), id.XSUsageMaster, true},
		"usage list containing the usage": {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{"usage": []id.CrossSigningUsage{id.XSUsageSelfSigning, id.XSUsageMaster}}), id.XSUsageMaster, true},
		"wrong usage":                     {masterKey, id.XSUsageSelfSigning, false},
		"wrong user":                      {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{"user_id": remoteKeysOther}), id.XSUsageMaster, false},
		"multiple keys":                   {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{"keys": map[string]string{master.keyID(): master.public, foreign.keyID(): foreign.public}}), id.XSUsageMaster, false},
		"key ID mismatch":                 {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{"keys": map[string]string{foreign.keyID(): master.public}}), id.XSUsageMaster, false},
		"short key":                       {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{"keys": map[string]string{"ed25519:AAAA": "AAAA"}}), id.XSUsageMaster, false},
		"case-variant foreign signatures": {crossSigningKeyJSON(t, master, id.XSUsageMaster, map[string]any{
			"Signatures": map[id.UserID]map[string]string{remoteKeysOther: {"ed25519:X": "c2ln"}},
		}), id.XSUsageMaster, false},
	} {
		key, err := validateRemoteCrossSigningKey(remoteKeysUser, tc.usage, tc.raw)
		if !tc.valid {
			assert.Error(t, err, name)
			assert.Nil(t, key, name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, key, mustCanonical(t, key), name)
		for signer := range signers(t, key) {
			assert.Equal(t, remoteKeysUser, signer, name)
		}
	}
}

func TestBoundRemoteDeviceDisplayName(t *testing.T) {
	for name, expected := range map[string]string{
		"Alice's phone":          "Alice's phone",
		strings.Repeat("a", 256): strings.Repeat("a", 256),
		strings.Repeat("a", 257): "",
		"invalid \xff utf-8":     "",
	} {
		assert.Equal(t, expected, BoundRemoteDeviceDisplayName(name), name)
	}
}

func deviceListEDU(t *testing.T, overrides map[string]any, omit ...string) json.RawMessage {
	return testJSON(t, map[string]any{
		"user_id":             remoteKeysUser,
		"device_id":           "DEVICE",
		"stream_id":           5,
		"prev_id":             []int64{4},
		"device_display_name": "Phone",
	}, overrides, omit...)
}

func TestParseDeviceListUpdateEDU(t *testing.T) {
	device := newRemoteTestKey(t)
	keys := signedDeviceKeys(t, "DEVICE", device, nil)
	phone := types.RemoteDevice{DisplayName: "Phone"}
	manyPrevIDs := make([]int64, types.MaxRemoteDevicePrevIDs+1)
	longDeviceID := id.DeviceID(strings.Repeat("D", types.MaxRemoteDeviceIDBytes+1))

	for name, tc := range map[string]struct {
		content  json.RawMessage
		expected types.RemoteDeviceListUpdate
	}{
		"with keys": {deviceListEDU(t, map[string]any{"keys": keys}), types.RemoteDeviceListUpdate{
			DeviceID: "DEVICE", StreamID: 5, PrevIDs: []int64{4}, Device: types.RemoteDevice{DisplayName: "Phone", Keys: mustCanonical(t, keys)},
		}},
		"without keys": {deviceListEDU(t, nil), types.RemoteDeviceListUpdate{DeviceID: "DEVICE", StreamID: 5, PrevIDs: []int64{4}, Device: phone}},
		"invalid keys keep the stream ID": {deviceListEDU(t, map[string]any{"device_id": "OTHER", "keys": keys}), types.RemoteDeviceListUpdate{
			DeviceID: "OTHER", StreamID: 5, PrevIDs: []int64{4}, Device: phone, Invalid: true,
		}},
		"deleted ignores keys": {deviceListEDU(t, map[string]any{"deleted": true, "keys": "garbage"}), types.RemoteDeviceListUpdate{
			DeviceID: "DEVICE", StreamID: 5, PrevIDs: []int64{4}, Deleted: true,
		}},
		"missing stream ID": {deviceListEDU(t, nil, "stream_id"), types.RemoteDeviceListUpdate{DeviceID: "DEVICE", Invalid: true}},
		"string stream ID":  {deviceListEDU(t, map[string]any{"stream_id": "5"}), types.RemoteDeviceListUpdate{Invalid: true}},
		"zero stream ID":    {deviceListEDU(t, map[string]any{"stream_id": 0}), types.RemoteDeviceListUpdate{DeviceID: "DEVICE", PrevIDs: []int64{4}, Device: phone}},
		"prev IDs over the limit are left to the caller": {deviceListEDU(t, map[string]any{"prev_id": manyPrevIDs}), types.RemoteDeviceListUpdate{
			DeviceID: "DEVICE", StreamID: 5, PrevIDs: manyPrevIDs, Device: phone,
		}},
		"over-long device ID": {deviceListEDU(t, map[string]any{"device_id": longDeviceID}), types.RemoteDeviceListUpdate{StreamID: 5, PrevIDs: []int64{4}, Invalid: true}},
		"display name bounded": {deviceListEDU(t, map[string]any{"device_display_name": strings.Repeat("a", types.MaxRemoteDeviceDisplayNameBytes+1)}), types.RemoteDeviceListUpdate{
			DeviceID: "DEVICE", StreamID: 5, PrevIDs: []int64{4},
		}},
		"display name with invalid UTF-8": {replaceTestJSON(t, deviceListEDU(t, nil), `"Phone"`, "\"Ph\xffone\""), types.RemoteDeviceListUpdate{
			DeviceID: "DEVICE", StreamID: 5, PrevIDs: []int64{4},
		}},
	} {
		userID, update, err := ParseDeviceListUpdateEDU(tc.content)
		require.NoError(t, err, name)
		assert.Equal(t, remoteKeysUser, userID, name)
		assert.Equal(t, tc.expected, update, name)
	}

	for name, content := range map[string]json.RawMessage{
		"missing user ID": deviceListEDU(t, nil, "user_id"),
		"invalid user ID": deviceListEDU(t, map[string]any{"user_id": "alice"}),
		"not an object":   json.RawMessage(`[]`),
	} {
		_, _, err := ParseDeviceListUpdateEDU(content)
		assert.Error(t, err, name)
	}
}

func TestParseSigningKeyUpdateEDU(t *testing.T) {
	master := newRemoteTestKey(t)
	selfSigning := newRemoteTestKey(t)
	masterKey := crossSigningKeyJSON(t, master, id.XSUsageMaster, nil)
	selfSigningKey := master.sign(t, crossSigningKeyJSON(t, selfSigning, id.XSUsageSelfSigning, nil), remoteKeysUser, master.keyID())
	edu := func(fields map[string]any) json.RawMessage {
		return testJSON(t, map[string]any{"user_id": remoteKeysUser}, fields)
	}

	for name, tc := range map[string]struct {
		content  json.RawMessage
		expected types.RemoteSigningKeyUpdate
	}{
		"both keys": {edu(map[string]any{"master_key": masterKey, "self_signing_key": selfSigningKey}), types.RemoteSigningKeyUpdate{
			MasterKey: mustCanonical(t, masterKey), SelfSigningKey: mustCanonical(t, selfSigningKey),
		}},
		"master only": {edu(map[string]any{"master_key": masterKey}), types.RemoteSigningKeyUpdate{MasterKey: mustCanonical(t, masterKey)}},
		"neither":     {edu(nil), types.RemoteSigningKeyUpdate{}},
		"invalid master dropped": {edu(map[string]any{"master_key": selfSigningKey, "self_signing_key": selfSigningKey}), types.RemoteSigningKeyUpdate{
			SelfSigningKey: mustCanonical(t, selfSigningKey), Invalid: true,
		}},
	} {
		userID, update, err := ParseSigningKeyUpdateEDU(tc.content)
		require.NoError(t, err, name)
		assert.Equal(t, remoteKeysUser, userID, name)
		assert.Equal(t, tc.expected, update, name)
	}

	_, _, err := ParseSigningKeyUpdateEDU(edu(map[string]any{"user_id": "@alice", "master_key": masterKey}))
	assert.Error(t, err)
}

func TestParseRemoteDeviceSnapshot(t *testing.T) {
	phone := newRemoteTestKey(t)
	master := newRemoteTestKey(t)
	phoneKeys := signedDeviceKeys(t, "PHONE", phone, nil)
	masterKey := crossSigningKeyJSON(t, master, id.XSUsageMaster, nil)
	phoneDevice := map[string]any{"device_id": "PHONE", "device_display_name": "Phone", "keys": phoneKeys}
	keylessDevice := map[string]any{"device_id": "KEYLESS"}
	snapshot := func(overrides map[string]any, omit ...string) json.RawMessage {
		return testJSON(t, map[string]any{
			"user_id":    remoteKeysUser,
			"stream_id":  7,
			"devices":    []any{phoneDevice, keylessDevice},
			"master_key": masterKey,
		}, overrides, omit...)
	}
	keylessDevices := func(count int) []any {
		devices := make([]any, count)
		for i := range devices {
			devices[i] = map[string]any{"device_id": fmt.Sprintf("D%04d", i)}
		}
		return devices
	}

	parsed, err := ParseRemoteDeviceSnapshot(remoteKeysUser, snapshot(nil))
	require.NoError(t, err)
	assert.Equal(t, &types.RemoteDeviceSnapshot{
		StreamID: 7,
		Devices: map[id.DeviceID]types.RemoteDevice{
			"PHONE":   {DisplayName: "Phone", Keys: mustCanonical(t, phoneKeys)},
			"KEYLESS": {},
		},
		MasterKey: mustCanonical(t, masterKey),
	}, parsed)

	for name, tc := range map[string]struct {
		body     json.RawMessage
		expected map[id.DeviceID]types.RemoteDevice
	}{
		"zero devices": {snapshot(map[string]any{"devices": []any{}}), map[id.DeviceID]types.RemoteDevice{}},
		"invalid device dropped": {snapshot(map[string]any{"devices": []any{
			keylessDevice, map[string]any{"device_id": "LAPTOP", "keys": phoneKeys},
		}}), map[id.DeviceID]types.RemoteDevice{"KEYLESS": {}}},
		"malformed entries dropped": {snapshot(map[string]any{"devices": []any{
			"device", map[string]any{"device_id": 5}, map[string]any{"device_id": ""}, keylessDevice,
		}}), map[id.DeviceID]types.RemoteDevice{"KEYLESS": {}}},
		"duplicate keeps first": {snapshot(map[string]any{"devices": []any{
			map[string]any{"device_id": "PHONE", "device_display_name": "First"}, phoneDevice,
		}}), map[id.DeviceID]types.RemoteDevice{"PHONE": {DisplayName: "First"}}},
	} {
		parsed, err := ParseRemoteDeviceSnapshot(remoteKeysUser, tc.body)
		require.NoError(t, err, name)
		assert.Equal(t, tc.expected, parsed.Devices, name)
	}

	parsed, err = ParseRemoteDeviceSnapshot(remoteKeysUser, snapshot(map[string]any{"master_key": "key"}))
	require.NoError(t, err)
	assert.Nil(t, parsed.MasterKey)

	for name, body := range map[string]json.RawMessage{
		"missing stream ID":    snapshot(nil, "stream_id"),
		"wrong user":           snapshot(map[string]any{"user_id": remoteKeysOther}),
		"devices not an array": snapshot(map[string]any{"devices": map[string]any{"PHONE": phoneDevice}}),
		"too many devices":     snapshot(map[string]any{"devices": keylessDevices(types.MaxRemoteDevicesPerUser + 1)}),
		"oversize":             snapshot(map[string]any{"padding": strings.Repeat("x", types.MaxRemoteDeviceSnapshotBytes)}),
		"invalid JSON":         json.RawMessage(`{"user_id":`),
	} {
		parsed, err := ParseRemoteDeviceSnapshot(remoteKeysUser, body)
		assert.Error(t, err, name)
		assert.Nil(t, parsed, name)
	}
}
