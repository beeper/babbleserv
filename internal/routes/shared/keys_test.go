package shared

import (
	"crypto/ed25519"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
)

const (
	testRequester  = id.UserID("@alice:local.example")
	testRemoteUser = id.UserID("@bob:remote.example")
	testThirdUser  = id.UserID("@carol:remote.example")
	// Path syntax to gjson and sjson
	testPathSyntaxUser = id.UserID("@b.o*b?:remote.example")
)

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func setTestField(t *testing.T, object json.RawMessage, path string, value any) json.RawMessage {
	t.Helper()
	out, err := sjson.SetBytes(object, path, value)
	require.NoError(t, err)
	return out
}

func mustSigned[T any](t *testing.T, raw json.RawMessage) types.SignedJSON[T] {
	t.Helper()
	signed, err := types.ParseSignedJSON[T](raw)
	require.NoError(t, err)
	return signed
}

func signaturesOf(t *testing.T, object json.RawMessage) signatures.Signatures {
	t.Helper()
	var decoded struct {
		Signatures signatures.Signatures `json:"signatures"`
	}
	require.NoError(t, json.Unmarshal(object, &decoded))
	return decoded.Signatures
}

type testSigningKey struct {
	public  id.Ed25519
	private ed25519.PrivateKey
}

func newTestSigningKey(t *testing.T) testSigningKey {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return testSigningKey{public: id.Ed25519(util.Base64Encode(public)), private: private}
}

func (k testSigningKey) keyID() id.KeyID {
	return id.NewKeyID(id.KeyAlgorithmEd25519, k.public.String())
}

func (k testSigningKey) verifyKey() ed25519.PublicKey {
	return k.private.Public().(ed25519.PublicKey)
}

func (k testSigningKey) crossSigningKey(userID id.UserID, usage id.CrossSigningUsage) mautrix.CrossSigningKeys {
	return mautrix.CrossSigningKeys{
		UserID: userID,
		Usage:  []id.CrossSigningUsage{usage},
		Keys:   map[id.KeyID]id.Ed25519{k.keyID(): k.public},
	}
}

func (k testSigningKey) signJSON(t *testing.T, object json.RawMessage, signer id.UserID, keyID id.KeyID) json.RawMessage {
	t.Helper()
	signed, err := util.SignJSON(object, signer.String(), keyID.String(), k.private)
	require.NoError(t, err)
	return signed
}

func (k testSigningKey) sign(t *testing.T, object json.RawMessage, signer id.UserID) json.RawMessage {
	return k.signJSON(t, object, signer, k.keyID())
}

func testDeviceKeys(t *testing.T, userID id.UserID, deviceID id.DeviceID, key testSigningKey, extra map[string]any) json.RawMessage {
	object := map[string]any{
		"user_id":    userID,
		"device_id":  deviceID,
		"algorithms": []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		"keys": map[id.DeviceKeyID]string{
			id.NewDeviceKeyID(id.KeyAlgorithmEd25519, deviceID):    key.public.String(),
			id.NewDeviceKeyID(id.KeyAlgorithmCurve25519, deviceID): util.Base64Encode([]byte("curve25519 public key 32 bytes!!")),
		},
	}
	maps.Copy(object, extra)
	return key.signJSON(t, mustMarshal(t, object), userID, id.NewKeyID(id.KeyAlgorithmEd25519, deviceID.String()))
}

func testCrossSigningKey(t *testing.T, userID id.UserID, usage id.CrossSigningUsage, key testSigningKey, extra map[string]any) json.RawMessage {
	object := map[string]any{
		"user_id": userID,
		"usage":   []id.CrossSigningUsage{usage},
		"keys":    map[id.KeyID]id.Ed25519{key.keyID(): key.public},
	}
	maps.Copy(object, extra)
	return mustMarshal(t, object)
}

const testExtraMasterMember = "org.example.extra"

// A remote user's keys as their server signs them. The laptop is a dehydrated device, signing a
// member the mautrix structs drop, and the master key carries an unknown signed member too.
type testRemoteKeys struct {
	userID id.UserID

	phoneKey, laptopKey, masterKey, selfSigningKey, userSigningKey testSigningKey

	phone, laptop, master, selfSigning, userSigning json.RawMessage
}

func newTestRemoteKeys(t *testing.T, userID id.UserID) testRemoteKeys {
	k := testRemoteKeys{
		userID:         userID,
		phoneKey:       newTestSigningKey(t),
		laptopKey:      newTestSigningKey(t),
		masterKey:      newTestSigningKey(t),
		selfSigningKey: newTestSigningKey(t),
		userSigningKey: newTestSigningKey(t),
	}
	k.phone = testDeviceKeys(t, userID, "PHONE", k.phoneKey, nil)
	k.laptop = testDeviceKeys(t, userID, "LAPTOP", k.laptopKey, map[string]any{"dehydrated": true})
	k.master = k.phoneKey.signJSON(t,
		testCrossSigningKey(t, userID, id.XSUsageMaster, k.masterKey, map[string]any{testExtraMasterMember: "signed"}),
		userID, "ed25519:PHONE")
	k.selfSigning = k.masterKey.sign(t, testCrossSigningKey(t, userID, id.XSUsageSelfSigning, k.selfSigningKey, nil), userID)
	k.userSigning = k.masterKey.sign(t, testCrossSigningKey(t, userID, id.XSUsageUserSigning, k.userSigningKey, nil), userID)
	return k
}

// Filled through the snapshot parser, as the worker fills it
func testRemoteCache(t *testing.T, keys testRemoteKeys) *types.RemoteDeviceCache {
	snapshot, err := util.ParseRemoteDeviceSnapshot(keys.userID, mustMarshal(t, map[string]any{
		"user_id":   keys.userID,
		"stream_id": 7,
		"devices": []any{
			map[string]any{"device_id": "PHONE", "device_display_name": "Phone", "keys": keys.phone},
			map[string]any{"device_id": "LAPTOP", "keys": keys.laptop},
			map[string]any{"device_id": "KEYLESS", "device_display_name": "No keys"},
		},
		"master_key":       keys.master,
		"self_signing_key": keys.selfSigning,
	}))
	require.NoError(t, err)
	return &types.RemoteDeviceCache{UserID: keys.userID, RemoteDeviceSnapshot: *snapshot}
}

// The same keys as testRemoteCache as a remote /user/keys/query answers them, plus content a client
// must never see: other users' signatures, a remote user-signing key and extra unsigned data
func testLiveResponse(t *testing.T, keys testRemoteKeys) types.RespQueryKeys {
	carol := newTestSigningKey(t)
	phone := setTestField(t, keys.phone, "unsigned", map[string]any{"device_display_name": "Phone", "other": "metadata"})
	return types.RespQueryKeys{
		DeviceKeys: map[id.UserID]map[id.DeviceID]json.RawMessage{
			keys.userID: {
				"PHONE":  carol.sign(t, phone, testThirdUser),
				"LAPTOP": setTestField(t, keys.laptop, "unsigned", map[string]any{"device_display_name": ""}),
			},
		},
		MasterKeys:      map[id.UserID]json.RawMessage{keys.userID: carol.sign(t, keys.master, testThirdUser)},
		SelfSigningKeys: map[id.UserID]json.RawMessage{keys.userID: keys.selfSigning},
		UserSigningKeys: map[id.UserID]json.RawMessage{keys.userID: keys.userSigning},
	}
}

func liveTestUserKeys(t *testing.T, body json.RawMessage, req map[id.UserID]mautrix.DeviceIDList) map[id.UserID]UserKeys {
	t.Helper()
	keys, err := UserKeysFromQueryResponse(body, req)
	require.NoError(t, err)
	return keys
}

func liveTestRemoteUserKeys(t *testing.T, keys testRemoteKeys) UserKeys {
	return liveTestUserKeys(t, mustMarshal(t, testLiveResponse(t, keys)), map[id.UserID]mautrix.DeviceIDList{keys.userID: nil})[keys.userID]
}

// Unvalidated, as local keys reach AddUserKeys, so other users' signatures and unsigned data are
// still there to filter
func testUnvalidatedKeys(t *testing.T, keys testRemoteKeys, res types.RespQueryKeys) UserKeys {
	return UserKeys{
		Devices: map[id.DeviceID]UserDeviceKeys{
			"PHONE":  {Keys: mustSigned[mautrix.DeviceKeys](t, res.DeviceKeys[keys.userID]["PHONE"])},
			"LAPTOP": {Keys: mustSigned[mautrix.DeviceKeys](t, res.DeviceKeys[keys.userID]["LAPTOP"])},
		},
		Master:      mustSigned[mautrix.CrossSigningKeys](t, res.MasterKeys[keys.userID]),
		SelfSigning: mustSigned[mautrix.CrossSigningKeys](t, res.SelfSigningKeys[keys.userID]),
	}
}

func testOverlay(keys testRemoteKeys) map[id.KeyID]map[id.KeyID]string {
	return map[id.KeyID]map[id.KeyID]string{
		id.KeyID(keys.masterKey.public): {"ed25519:aliceUSK": "alice-master-signature"},
		"PHONE":                         {"ed25519:aliceUSK": "alice-device-signature"},
	}
}

func addTestUserKeys(t *testing.T, keys UserKeys, deviceIDs mautrix.DeviceIDList, requestUserID id.UserID, overlay map[id.KeyID]map[id.KeyID]string) types.RespQueryKeys {
	t.Helper()
	resp := newQueryKeysResponse(1)
	require.NoError(t, AddUserKeys(&resp, testRemoteUser, deviceIDs, keys, requestUserID, overlay))
	return resp
}

func TestCachedAndLiveKeysFilterIdentically(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	cached := UserKeysFromRemoteCache(testRemoteCache(t, keys))
	live := liveTestRemoteUserKeys(t, keys)
	assert.ElementsMatch(t, []id.DeviceID{"PHONE", "LAPTOP"}, slices.Collect(maps.Keys(live.Devices)))
	assert.True(t, live.UserSigning.IsZero())

	for _, deviceIDs := range []mautrix.DeviceIDList{nil, {"PHONE"}, {"LAPTOP", "KEYLESS", "UNKNOWN"}} {
		cachedResp := addTestUserKeys(t, cached, deviceIDs, testRequester, testOverlay(keys))
		liveResp := addTestUserKeys(t, live, deviceIDs, testRequester, testOverlay(keys))
		assert.Equal(t, string(mustMarshal(t, cachedResp)), string(mustMarshal(t, liveResp)), "device IDs %v", deviceIDs)
	}
}

func TestKeysQueryKeepsUnknownSignedMembers(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	for source, userKeys := range map[string]UserKeys{
		"cache": UserKeysFromRemoteCache(testRemoteCache(t, keys)),
		"live":  liveTestRemoteUserKeys(t, keys),
	} {
		resp := addTestUserKeys(t, userKeys, nil, testRequester, testOverlay(keys))

		laptop := resp.DeviceKeys[testRemoteUser]["LAPTOP"]
		assert.True(t, gjson.GetBytes(laptop, "dehydrated").Bool(), source)
		assert.NoError(t, signedjson.Verify(laptop, testRemoteUser.String(), "ed25519:LAPTOP", keys.laptopKey.verifyKey()), source)
		assert.False(t, gjson.GetBytes(laptop, "unsigned").Exists(), source)

		phone := resp.DeviceKeys[testRemoteUser]["PHONE"]
		assert.NoError(t, signedjson.Verify(phone, testRemoteUser.String(), "ed25519:PHONE", keys.phoneKey.verifyKey()), source)
		assert.JSONEq(t, `{"device_display_name":"Phone"}`, gjson.GetBytes(phone, "unsigned").Raw, source)
		assert.Equal(t, signatures.Signatures{
			testRemoteUser: signaturesOf(t, keys.phone)[testRemoteUser],
			testRequester:  {"ed25519:aliceUSK": "alice-device-signature"},
		}, signaturesOf(t, phone), source)

		master := resp.MasterKeys[testRemoteUser]
		assert.Equal(t, "signed", gjson.GetBytes(master, gjson.Escape(testExtraMasterMember)).String(), source)
		assert.NoError(t, signedjson.Verify(master, testRemoteUser.String(), "ed25519:PHONE", keys.phoneKey.verifyKey()), source)
		assert.Equal(t, signatures.Signatures{
			testRemoteUser: signaturesOf(t, keys.master)[testRemoteUser],
			testRequester:  {"ed25519:aliceUSK": "alice-master-signature"},
		}, signaturesOf(t, master), source)

		selfSigning := resp.SelfSigningKeys[testRemoteUser]
		assert.NoError(t, signedjson.Verify(selfSigning, testRemoteUser.String(), keys.masterKey.keyID().String(), keys.masterKey.verifyKey()), source)

		// What decoding into the mautrix structs served instead
		var decoded mautrix.DeviceKeys
		require.NoError(t, json.Unmarshal(laptop, &decoded), source)
		assert.Error(t, signedjson.Verify(mustMarshal(t, decoded), testRemoteUser.String(), "ed25519:LAPTOP", keys.laptopKey.verifyKey()), source)
	}
}

// Local keys come from storage with the owner's signatures read back apart from them
func TestLocalKeysKeepUnknownSignedMembers(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	stored := func(raw json.RawMessage) types.SignedJSON[mautrix.DeviceKeys] {
		uploaded := mustSigned[mautrix.DeviceKeys](t, raw)
		withoutSignatures, err := uploaded.WithoutSignatures()
		require.NoError(t, err)
		read, err := withoutSignatures.WithOwnerSignatures(testRemoteUser, uploaded.View.Signatures[testRemoteUser])
		require.NoError(t, err)
		return read
	}
	userKeys := userKeysFromLocalSnapshot(&types.LocalDeviceSnapshot{
		Devices: []types.LocalSnapshotDevice{
			{Device: types.Device{ID: "LAPTOP", DisplayName: "Laptop"}, Keys: stored(keys.laptop)},
			{Device: types.Device{ID: "KEYLESS"}},
		},
		MasterKey: mustSigned[mautrix.CrossSigningKeys](t, keys.master),
	})
	assert.Equal(t, []id.DeviceID{"LAPTOP"}, slices.Collect(maps.Keys(userKeys.Devices)))

	resp := addTestUserKeys(t, userKeys, nil, testRequester, testOverlay(keys))
	laptop := resp.DeviceKeys[testRemoteUser]["LAPTOP"]
	assert.True(t, gjson.GetBytes(laptop, "dehydrated").Bool())
	assert.JSONEq(t, `{"device_display_name":"Laptop"}`, gjson.GetBytes(laptop, "unsigned").Raw)
	assert.NoError(t, signedjson.Verify(laptop, testRemoteUser.String(), "ed25519:LAPTOP", keys.laptopKey.verifyKey()))

	master := resp.MasterKeys[testRemoteUser]
	assert.Equal(t, "signed", gjson.GetBytes(master, gjson.Escape(testExtraMasterMember)).String())
	assert.NoError(t, signedjson.Verify(master, testRemoteUser.String(), "ed25519:PHONE", keys.phoneKey.verifyKey()))
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.master)[testRemoteUser],
		testRequester:  {"ed25519:aliceUSK": "alice-master-signature"},
	}, signaturesOf(t, master))
}

func TestKeysQueryUserIDWithPathSyntax(t *testing.T) {
	keys := newTestRemoteKeys(t, testPathSyntaxUser)
	for source, userKeys := range map[string]UserKeys{
		"cache": UserKeysFromRemoteCache(testRemoteCache(t, keys)),
		"live":  liveTestRemoteUserKeys(t, keys),
	} {
		require.ElementsMatch(t, []id.DeviceID{"PHONE", "LAPTOP"}, slices.Collect(maps.Keys(userKeys.Devices)), source)
		resp := newQueryKeysResponse(1)
		require.NoError(t, AddUserKeys(&resp, testPathSyntaxUser, nil, userKeys, testRequester, testOverlay(keys)), source)

		assert.Equal(t, signatures.Signatures{
			testPathSyntaxUser: signaturesOf(t, keys.phone)[testPathSyntaxUser],
			testRequester:      {"ed25519:aliceUSK": "alice-device-signature"},
		}, signaturesOf(t, resp.DeviceKeys[testPathSyntaxUser]["PHONE"]), source)
		assert.Equal(t, signatures.Signatures{
			testPathSyntaxUser: signaturesOf(t, keys.master)[testPathSyntaxUser],
			testRequester:      {"ed25519:aliceUSK": "alice-master-signature"},
		}, signaturesOf(t, resp.MasterKeys[testPathSyntaxUser]), source)
	}
}

func TestAddUserKeysUserSigningOnlyForRequester(t *testing.T) {
	remote := newTestRemoteKeys(t, testRemoteUser)
	keys := UserKeys{UserSigning: mustSigned[mautrix.CrossSigningKeys](t, remote.userSigning)}

	assert.Empty(t, addTestUserKeys(t, keys, nil, testRequester, nil).UserSigningKeys)
	assert.Empty(t, addTestUserKeys(t, keys, nil, "", nil).UserSigningKeys)

	own := addTestUserKeys(t, keys, nil, testRemoteUser, nil).UserSigningKeys[testRemoteUser]
	assert.Equal(t, remote.userSigningKey.public, types.CrossSigningPublicKey(mustSigned[mautrix.CrossSigningKeys](t, own)))
}

func TestAddUserKeysKeepsOwnerSignaturesAndOverlay(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	resp := addTestUserKeys(t, testUnvalidatedKeys(t, keys, testLiveResponse(t, keys)), nil, testRequester, testOverlay(keys))

	phone := resp.DeviceKeys[testRemoteUser]["PHONE"]
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.phone)[testRemoteUser],
		testRequester:  {"ed25519:aliceUSK": "alice-device-signature"},
	}, signaturesOf(t, phone))
	assert.False(t, gjson.GetBytes(phone, "unsigned").Exists())
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.laptop)[testRemoteUser],
	}, signaturesOf(t, resp.DeviceKeys[testRemoteUser]["LAPTOP"]))
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.master)[testRemoteUser],
		testRequester:  {"ed25519:aliceUSK": "alice-master-signature"},
	}, signaturesOf(t, resp.MasterKeys[testRemoteUser]))
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.selfSigning)[testRemoteUser],
	}, signaturesOf(t, resp.SelfSigningKeys[testRemoteUser]))
}

func TestAddUserKeysOverlayNeverReplacesOwnerSignatures(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	resp := addTestUserKeys(t, UserKeysFromRemoteCache(testRemoteCache(t, keys)), nil, testRemoteUser, testOverlay(keys))

	assert.Equal(t, signatures.Signatures{
		testRemoteUser: signaturesOf(t, keys.master)[testRemoteUser],
	}, signaturesOf(t, resp.MasterKeys[testRemoteUser]))
}

func TestAddUserKeysDoesNotMutateKeys(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	userKeys := testUnvalidatedKeys(t, keys, testLiveResponse(t, keys))
	phone := slices.Clone(userKeys.Devices["PHONE"].Keys.Raw)
	master := slices.Clone(userKeys.Master.Raw)
	addTestUserKeys(t, userKeys, nil, testRequester, testOverlay(keys))

	assert.Equal(t, phone, userKeys.Devices["PHONE"].Keys.Raw)
	assert.Equal(t, master, userKeys.Master.Raw)
}

func TestAddUserKeysSkipsUnservableObjects(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	userKeys := UserKeysFromRemoteCache(testRemoteCache(t, keys))
	userKeys.Devices["BROKEN"] = UserDeviceKeys{Keys: types.SignedDeviceKeys{Raw: json.RawMessage(`{"device_id":"BROKEN","device_id":"OTHER"}`)}}
	userKeys.SelfSigning = types.SignedCrossSigningKey{Raw: json.RawMessage(`[]`)}

	resp := newQueryKeysResponse(1)
	err := AddUserKeys(&resp, testRemoteUser, nil, userKeys, testRequester, nil)
	assert.ErrorContains(t, err, "BROKEN")
	assert.ErrorContains(t, err, "self-signing")
	assert.ElementsMatch(t, []id.DeviceID{"PHONE", "LAPTOP"}, slices.Collect(maps.Keys(resp.DeviceKeys[testRemoteUser])))
	assert.Contains(t, resp.MasterKeys, testRemoteUser)
	assert.Empty(t, resp.SelfSigningKeys)
}

func TestUserKeysFromRemoteCacheWithoutDevicesOrSigningKeys(t *testing.T) {
	keys := UserKeysFromRemoteCache(&types.RemoteDeviceCache{
		UserID:               testRemoteUser,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{Devices: map[id.DeviceID]types.RemoteDevice{}},
	})

	resp := addTestUserKeys(t, keys, nil, testRequester, nil)
	assert.NotNil(t, resp.DeviceKeys[testRemoteUser])
	assert.Empty(t, resp.DeviceKeys[testRemoteUser])
	assert.Empty(t, resp.MasterKeys)
	assert.Empty(t, resp.SelfSigningKeys)
}

func TestUserKeysFromQueryResponseDropsInvalidObjects(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	stranger := newTestSigningKey(t)
	res := testLiveResponse(t, keys)
	res.DeviceKeys[testRemoteUser]["ALIAS"] = keys.phone
	res.DeviceKeys[testRemoteUser]["FORGED"] = setTestField(t, testDeviceKeys(t, testRemoteUser, "FORGED", stranger, nil), "algorithms", []string{"m.other"})
	res.DeviceKeys[testRemoteUser]["STRANGER"] = testDeviceKeys(t, testThirdUser, "STRANGER", stranger, nil)
	res.MasterKeys[testRemoteUser] = testCrossSigningKey(t, testThirdUser, id.XSUsageMaster, keys.masterKey, nil)
	res.DeviceKeys[testThirdUser] = map[id.DeviceID]json.RawMessage{"CAROL": testDeviceKeys(t, testThirdUser, "CAROL", stranger, nil)}

	all := liveTestUserKeys(t, mustMarshal(t, res), map[id.UserID]mautrix.DeviceIDList{testRemoteUser: nil})
	assert.Len(t, all, 1)
	userKeys := all[testRemoteUser]
	assert.ElementsMatch(t, []id.DeviceID{"PHONE", "LAPTOP"}, slices.Collect(maps.Keys(userKeys.Devices)))
	assert.True(t, userKeys.Master.IsZero())
	assert.False(t, userKeys.SelfSigning.IsZero())
	assert.True(t, userKeys.UserSigning.IsZero())
}

func TestUserKeysFromQueryResponseOnlyValidatesRequestedDevices(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	body := mustMarshal(t, testLiveResponse(t, keys))

	userKeys := liveTestUserKeys(t, body, map[id.UserID]mautrix.DeviceIDList{testRemoteUser: {"PHONE", "UNKNOWN", "PHONE"}})[testRemoteUser]
	assert.Equal(t, []id.DeviceID{"PHONE"}, slices.Collect(maps.Keys(userKeys.Devices)))
	assert.False(t, userKeys.Master.IsZero())
}

func TestUserKeysFromQueryResponseDropsOnlyMalformedUsers(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	live := testLiveResponse(t, keys)
	const absentUser = id.UserID("@dave:remote.example")
	body := mustMarshal(t, map[string]any{
		"device_keys": map[id.UserID]any{
			testRemoteUser: live.DeviceKeys[testRemoteUser],
			testThirdUser:  "not an object",
		},
		"master_keys":       live.MasterKeys,
		"self_signing_keys": live.SelfSigningKeys,
		"user_signing_keys": "not an object",
		"failures":          "not an object",
	})

	all := liveTestUserKeys(t, body, map[id.UserID]mautrix.DeviceIDList{testRemoteUser: nil, testThirdUser: nil, absentUser: nil})
	assert.ElementsMatch(t, []id.UserID{testRemoteUser, absentUser}, slices.Collect(maps.Keys(all)))
	assert.Len(t, all[testRemoteUser].Devices, 2)
	assert.Empty(t, all[absentUser].Devices)

	_, err := UserKeysFromQueryResponse(json.RawMessage(`{"device_keys": []}`), map[id.UserID]mautrix.DeviceIDList{testRemoteUser: nil})
	assert.Error(t, err)
}

func TestAddUserKeysDeviceFilter(t *testing.T) {
	keys := UserKeysFromRemoteCache(testRemoteCache(t, newTestRemoteKeys(t, testRemoteUser)))

	for _, tc := range []struct {
		deviceIDs mautrix.DeviceIDList
		expected  []id.DeviceID
	}{
		{nil, []id.DeviceID{"PHONE", "LAPTOP"}},
		{mautrix.DeviceIDList{}, []id.DeviceID{"PHONE", "LAPTOP"}},
		{mautrix.DeviceIDList{"LAPTOP"}, []id.DeviceID{"LAPTOP"}},
		{mautrix.DeviceIDList{"LAPTOP", "UNKNOWN", "LAPTOP"}, []id.DeviceID{"LAPTOP"}},
		{mautrix.DeviceIDList{"UNKNOWN"}, []id.DeviceID{}},
	} {
		resp := addTestUserKeys(t, keys, tc.deviceIDs, testRequester, nil)
		require.Contains(t, resp.DeviceKeys, testRemoteUser, "device IDs %v", tc.deviceIDs)
		assert.ElementsMatch(t, tc.expected, slices.Collect(maps.Keys(resp.DeviceKeys[testRemoteUser])), "device IDs %v", tc.deviceIDs)
		assert.Contains(t, resp.MasterKeys, testRemoteUser, "device IDs %v", tc.deviceIDs)
	}
}

func TestSignatureOverlayTargets(t *testing.T) {
	remote := newTestRemoteKeys(t, testRemoteUser)
	keys := map[id.UserID]UserKeys{
		testRemoteUser: UserKeysFromRemoteCache(testRemoteCache(t, remote)),
		testRequester:  {Devices: map[id.DeviceID]UserDeviceKeys{"OWN": {}}},
		testThirdUser:  {Devices: map[id.DeviceID]UserDeviceKeys{}},
	}
	req := mautrix.DeviceKeysRequest{testRemoteUser: {"PHONE", "KEYLESS"}}

	assert.Equal(t, map[id.UserID]map[id.KeyID]struct{}{
		testRemoteUser: {
			"PHONE":                                {},
			id.KeyID(remote.masterKey.public):      {},
			id.KeyID(remote.selfSigningKey.public): {},
		},
	}, signatureOverlayTargets(req, keys, testRequester))
	assert.Nil(t, signatureOverlayTargets(req, keys, ""))
}

func TestRemoteSignatureTargetFailure(t *testing.T) {
	remoteMaster := newTestSigningKey(t)
	remoteSelfSigning := newTestSigningKey(t)
	requesterUserSigning := newTestSigningKey(t)
	requesterMaster := newTestSigningKey(t)

	cachedMaster := remoteMaster.crossSigningKey(testRemoteUser, id.XSUsageMaster)
	cachedMaster.Signatures = signatures.Signatures{testRemoteUser: {"ed25519:PHONE": "owner-signature"}}
	cache := &types.RemoteDeviceCache{
		UserID: testRemoteUser,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{
			Devices:        map[id.DeviceID]types.RemoteDevice{},
			MasterKey:      mustSigned[mautrix.CrossSigningKeys](t, mustMarshal(t, cachedMaster)),
			SelfSigningKey: mustSigned[mautrix.CrossSigningKeys](t, mustMarshal(t, remoteSelfSigning.crossSigningKey(testRemoteUser, id.XSUsageSelfSigning))),
		},
	}
	userSigningKey := mustSigned[mautrix.CrossSigningKeys](t, mustMarshal(t, requesterUserSigning.crossSigningKey(testRequester, id.XSUsageUserSigning)))
	target := func(raw json.RawMessage) types.SignatureUploadTarget {
		return mustSigned[mautrix.ReqKeysSignatures](t, raw)
	}

	unsignedTarget := mustMarshal(t, mautrix.ReqKeysSignatures{
		UserID: testRemoteUser,
		Usage:  []id.CrossSigningUsage{id.XSUsageMaster},
		Keys:   map[id.KeyID]string{remoteMaster.keyID(): remoteMaster.public.String()},
	})
	signedTarget := requesterUserSigning.sign(t, unsignedTarget, testRequester)
	withOwnerSignature := requesterUserSigning.sign(t, setTestField(t, unsignedTarget, "signatures", cachedMaster.Signatures), testRequester)
	mismatched := requesterUserSigning.sign(t, setTestField(t, unsignedTarget, "usage", []id.CrossSigningUsage{id.XSUsageSelfSigning}), testRequester)
	tampered := setTestField(t, unsignedTarget, "signatures", signaturesOf(t, mismatched))
	deviceTarget := setTestField(t, signedTarget, "device_id", "PHONE")

	noMaster := *cache
	noMaster.MasterKey = types.SignedCrossSigningKey{}

	masterKey := remoteMaster.public.String()

	for _, tc := range []struct {
		name           string
		userSigningKey types.SignedCrossSigningKey
		cache          *types.RemoteDeviceCache
		targetKey      string
		target         json.RawMessage
		errCode        string
	}{
		{"accepted", userSigningKey, cache, masterKey, signedTarget, ""},
		{"owner signatures ignored", userSigningKey, cache, masterKey, withOwnerSignature, ""},
		{"device target", userSigningKey, cache, "PHONE", deviceTarget, mautrix.MInvalidParam.ErrCode},
		{"uncached or no valid snapshot", userSigningKey, nil, masterKey, signedTarget, mautrix.MNotFound.ErrCode},
		{"no cached master key", userSigningKey, &noMaster, masterKey, signedTarget, mautrix.MNotFound.ErrCode},
		{"not the master key", userSigningKey, cache, remoteSelfSigning.public.String(), signedTarget, mautrix.MNotFound.ErrCode},
		{"content mismatch", userSigningKey, cache, masterKey, mismatched, mautrix.MInvalidParam.ErrCode},
		{"requester has no user-signing key", types.SignedCrossSigningKey{}, cache, masterKey, signedTarget, errCodeInvalidSignature},
		{"no requester signature", userSigningKey, cache, masterKey, unsignedTarget, errCodeInvalidSignature},
		{"signed by another requester key", userSigningKey, cache, masterKey, requesterMaster.sign(t, unsignedTarget, testRequester), errCodeInvalidSignature},
		{"signature does not verify", userSigningKey, cache, masterKey, tampered, errCodeInvalidSignature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := RemoteSignatureTargetFailure(testRequester, tc.userSigningKey, tc.cache, tc.targetKey, target(tc.target))
			if tc.errCode == "" {
				assert.Nil(t, failure)
				return
			}
			require.NotNil(t, failure)
			assert.Equal(t, tc.errCode, failure.ErrCode)
			expected := map[string]string{"errcode": tc.errCode, "error": failure.Err}
			assert.JSONEq(t, string(mustMarshal(t, expected)), string(mustMarshal(t, failure)))
		})
	}
}

// The client signs the master key as served, unknown members included
func TestRemoteSignatureTargetOverServedMasterKey(t *testing.T) {
	keys := newTestRemoteKeys(t, testRemoteUser)
	cache := testRemoteCache(t, keys)
	requesterUserSigning := newTestSigningKey(t)
	userSigningKey := mustSigned[mautrix.CrossSigningKeys](t, mustMarshal(t, requesterUserSigning.crossSigningKey(testRequester, id.XSUsageUserSigning)))
	masterKey := keys.masterKey.public.String()
	signedTarget := func(raw json.RawMessage) types.SignatureUploadTarget {
		return mustSigned[mautrix.ReqKeysSignatures](t, requesterUserSigning.sign(t, raw, testRequester))
	}

	served := addTestUserKeys(t, UserKeysFromRemoteCache(cache), nil, testRequester, nil).MasterKeys[testRemoteUser]
	assert.Nil(t, RemoteSignatureTargetFailure(testRequester, userSigningKey, cache, masterKey, signedTarget(served)))

	tampered := setTestField(t, served, gjson.Escape(testExtraMasterMember), "tampered")
	require.Equal(t, "tampered", gjson.GetBytes(tampered, gjson.Escape(testExtraMasterMember)).String())
	failure := RemoteSignatureTargetFailure(testRequester, userSigningKey, cache, masterKey, signedTarget(tampered))
	require.NotNil(t, failure)
	assert.Equal(t, mautrix.MInvalidParam.ErrCode, failure.ErrCode)
}
