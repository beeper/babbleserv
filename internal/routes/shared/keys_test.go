package shared

import (
	"crypto/ed25519"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	testRequester  = id.UserID("@alice:local.example")
	testRemoteUser = id.UserID("@bob:remote.example")
	testThirdUser  = id.UserID("@carol:remote.example")

	testMasterPublic      = id.Ed25519("bobMasterPublicKey")
	testSelfSigningPublic = id.Ed25519("bobSelfSigningPublicKey")
	testUserSigningPublic = id.Ed25519("bobUserSigningPublicKey")
)

func testDeviceKeys(deviceID id.DeviceID) mautrix.DeviceKeys {
	return mautrix.DeviceKeys{
		UserID:     testRemoteUser,
		DeviceID:   deviceID,
		Algorithms: []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		Keys: mautrix.KeyMap{
			id.NewDeviceKeyID(id.KeyAlgorithmEd25519, deviceID):    "ed25519-" + string(deviceID),
			id.NewDeviceKeyID(id.KeyAlgorithmCurve25519, deviceID): "curve25519-" + string(deviceID),
		},
		Signatures: signatures.Signatures{
			testRemoteUser: {id.NewKeyID(id.KeyAlgorithmEd25519, string(deviceID)): "self-" + string(deviceID)},
		},
	}
}

func testCrossSigningKey(usage id.CrossSigningUsage, public id.Ed25519) mautrix.CrossSigningKeys {
	return mautrix.CrossSigningKeys{
		UserID: testRemoteUser,
		Usage:  []id.CrossSigningUsage{usage},
		Keys:   map[id.KeyID]id.Ed25519{id.NewKeyID(id.KeyAlgorithmEd25519, public.String()): public},
		Signatures: signatures.Signatures{
			testRemoteUser: {"ed25519:PHONE": "owner-" + string(usage)},
		},
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func testRemoteCache(t *testing.T) *types.RemoteDeviceCache {
	return &types.RemoteDeviceCache{
		UserID: testRemoteUser,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{
			StreamID: 7,
			Devices: map[id.DeviceID]types.RemoteDevice{
				"PHONE":   {DisplayName: "Phone", Keys: mustMarshal(t, testDeviceKeys("PHONE"))},
				"LAPTOP":  {Keys: mustMarshal(t, testDeviceKeys("LAPTOP"))},
				"KEYLESS": {DisplayName: "No keys"},
			},
			MasterKey:      mustMarshal(t, testCrossSigningKey(id.XSUsageMaster, testMasterPublic)),
			SelfSigningKey: mustMarshal(t, testCrossSigningKey(id.XSUsageSelfSigning, testSelfSigningPublic)),
		},
	}
}

// The same keys as testRemoteCache, served the way a remote /user/keys/query answers, plus content
// a client must never see: other users' signatures, a remote user-signing key and extra unsigned data
func testLiveResponse() mautrix.RespQueryKeys {
	phone := testDeviceKeys("PHONE")
	phone.Signatures[testThirdUser] = map[id.KeyID]string{"ed25519:CAROL": "carol-device-signature"}
	phone.Unsigned = map[string]any{"device_display_name": "Phone", "other": "metadata"}
	laptop := testDeviceKeys("LAPTOP")
	laptop.Unsigned = map[string]any{"device_display_name": ""}

	master := testCrossSigningKey(id.XSUsageMaster, testMasterPublic)
	master.Signatures[testThirdUser] = map[id.KeyID]string{"ed25519:carolUSK": "carol-master-signature"}

	return mautrix.RespQueryKeys{
		DeviceKeys: map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys{
			testRemoteUser: {"PHONE": phone, "LAPTOP": laptop},
		},
		MasterKeys:      map[id.UserID]mautrix.CrossSigningKeys{testRemoteUser: master},
		SelfSigningKeys: map[id.UserID]mautrix.CrossSigningKeys{testRemoteUser: testCrossSigningKey(id.XSUsageSelfSigning, testSelfSigningPublic)},
		UserSigningKeys: map[id.UserID]mautrix.CrossSigningKeys{testRemoteUser: testCrossSigningKey(id.XSUsageUserSigning, testUserSigningPublic)},
	}
}

func testOverlay() map[id.KeyID]map[id.KeyID]string {
	return map[id.KeyID]map[id.KeyID]string{
		id.KeyID(testMasterPublic): {"ed25519:aliceUSK": "alice-master-signature"},
		"PHONE":                    {"ed25519:aliceUSK": "alice-device-signature"},
	}
}

func addTestUserKeys(keys UserKeys, deviceIDs mautrix.DeviceIDList, requestUserID id.UserID, overlay map[id.KeyID]map[id.KeyID]string) mautrix.RespQueryKeys {
	resp := newQueryKeysResponse(1)
	AddUserKeys(&resp, testRemoteUser, deviceIDs, keys, requestUserID, overlay)
	return resp
}

func TestCachedAndLiveKeysFilterIdentically(t *testing.T) {
	cached, err := UserKeysFromRemoteCache(testRemoteCache(t))
	require.NoError(t, err)
	live := UserKeysFromQueryResponse(testRemoteUser, testLiveResponse())
	assert.Nil(t, live.UserSigning)

	for _, deviceIDs := range []mautrix.DeviceIDList{nil, {"PHONE"}, {"LAPTOP", "KEYLESS", "UNKNOWN"}} {
		cachedResp := addTestUserKeys(cached, deviceIDs, testRequester, testOverlay())
		liveResp := addTestUserKeys(live, deviceIDs, testRequester, testOverlay())
		assert.JSONEq(t, string(mustMarshal(t, cachedResp)), string(mustMarshal(t, liveResp)), "device IDs %v", deviceIDs)
	}

	phone := addTestUserKeys(cached, nil, testRequester, nil).DeviceKeys[testRemoteUser]["PHONE"]
	assert.Equal(t, map[string]any{"device_display_name": "Phone"}, phone.Unsigned)
}

func TestAddUserKeysUserSigningOnlyForRequester(t *testing.T) {
	userSigning := testCrossSigningKey(id.XSUsageUserSigning, testUserSigningPublic)
	keys := UserKeys{UserSigning: &userSigning}

	assert.Empty(t, addTestUserKeys(keys, nil, testRequester, nil).UserSigningKeys)
	assert.Empty(t, addTestUserKeys(keys, nil, "", nil).UserSigningKeys)

	own := addTestUserKeys(keys, nil, testRemoteUser, nil).UserSigningKeys[testRemoteUser]
	assert.Equal(t, testUserSigningPublic, own.FirstKey())
}

func TestAddUserKeysKeepsOwnerSignaturesAndOverlay(t *testing.T) {
	resp := addTestUserKeys(UserKeysFromQueryResponse(testRemoteUser, testLiveResponse()), nil, testRequester, testOverlay())

	assert.Equal(t, signatures.Signatures{
		testRemoteUser: {"ed25519:PHONE": "self-PHONE"},
		testRequester:  {"ed25519:aliceUSK": "alice-device-signature"},
	}, resp.DeviceKeys[testRemoteUser]["PHONE"].Signatures)
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: {"ed25519:LAPTOP": "self-LAPTOP"},
	}, resp.DeviceKeys[testRemoteUser]["LAPTOP"].Signatures)
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: {"ed25519:PHONE": "owner-master"},
		testRequester:  {"ed25519:aliceUSK": "alice-master-signature"},
	}, resp.MasterKeys[testRemoteUser].Signatures)
	assert.Equal(t, signatures.Signatures{
		testRemoteUser: {"ed25519:PHONE": "owner-self_signing"},
	}, resp.SelfSigningKeys[testRemoteUser].Signatures)
}

func TestAddUserKeysOverlayNeverReplacesOwnerSignatures(t *testing.T) {
	keys := UserKeysFromQueryResponse(testRemoteUser, testLiveResponse())
	resp := addTestUserKeys(keys, nil, testRemoteUser, testOverlay())

	assert.Equal(t, signatures.Signatures{
		testRemoteUser: {"ed25519:PHONE": "owner-master"},
	}, resp.MasterKeys[testRemoteUser].Signatures)
}

func TestAddUserKeysDoesNotMutateKeys(t *testing.T) {
	keys := UserKeysFromQueryResponse(testRemoteUser, testLiveResponse())
	addTestUserKeys(keys, nil, testRequester, testOverlay())

	assert.Contains(t, keys.Devices["PHONE"].Keys.Signatures, testThirdUser)
	assert.NotContains(t, keys.Devices["PHONE"].Keys.Signatures, testRequester)
	assert.NotContains(t, keys.Master.Signatures, testRequester)
}

func TestUserKeysFromRemoteCacheWithoutDevicesOrSigningKeys(t *testing.T) {
	keys, err := UserKeysFromRemoteCache(&types.RemoteDeviceCache{
		UserID:               testRemoteUser,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{Devices: map[id.DeviceID]types.RemoteDevice{}},
	})
	require.NoError(t, err)

	resp := addTestUserKeys(keys, nil, testRequester, nil)
	assert.NotNil(t, resp.DeviceKeys[testRemoteUser])
	assert.Empty(t, resp.DeviceKeys[testRemoteUser])
	assert.Empty(t, resp.MasterKeys)
	assert.Empty(t, resp.SelfSigningKeys)
}

func TestUserKeysFromQueryResponseDropsMismatchedObjects(t *testing.T) {
	res := testLiveResponse()
	res.DeviceKeys[testRemoteUser]["ALIAS"] = testDeviceKeys("PHONE")
	stranger := testDeviceKeys("STRANGER")
	stranger.UserID = testThirdUser
	res.DeviceKeys[testRemoteUser]["STRANGER"] = stranger
	master := res.MasterKeys[testRemoteUser]
	master.UserID = testThirdUser
	res.MasterKeys[testRemoteUser] = master
	res.DeviceKeys[testThirdUser] = map[id.DeviceID]mautrix.DeviceKeys{"CAROL": testDeviceKeys("CAROL")}

	keys := UserKeysFromQueryResponse(testRemoteUser, res)
	assert.ElementsMatch(t, []id.DeviceID{"PHONE", "LAPTOP"}, slices.Collect(maps.Keys(keys.Devices)))
	assert.Nil(t, keys.Master)
	assert.NotNil(t, keys.SelfSigning)
}

func TestAddUserKeysDeviceFilter(t *testing.T) {
	keys, err := UserKeysFromRemoteCache(testRemoteCache(t))
	require.NoError(t, err)

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
		resp := addTestUserKeys(keys, tc.deviceIDs, testRequester, nil)
		require.Contains(t, resp.DeviceKeys, testRemoteUser, "device IDs %v", tc.deviceIDs)
		assert.ElementsMatch(t, tc.expected, slices.Collect(maps.Keys(resp.DeviceKeys[testRemoteUser])), "device IDs %v", tc.deviceIDs)
		assert.Contains(t, resp.MasterKeys, testRemoteUser, "device IDs %v", tc.deviceIDs)
	}
}

func TestSignatureOverlayTargets(t *testing.T) {
	cached, err := UserKeysFromRemoteCache(testRemoteCache(t))
	require.NoError(t, err)
	keys := map[id.UserID]UserKeys{
		testRemoteUser: cached,
		testRequester:  {Devices: map[id.DeviceID]UserDeviceKeys{"OWN": {}}},
		testThirdUser:  {Devices: map[id.DeviceID]UserDeviceKeys{}},
	}
	req := mautrix.DeviceKeysRequest{testRemoteUser: {"PHONE", "KEYLESS"}}

	assert.Equal(t, map[id.UserID]map[id.KeyID]struct{}{
		testRemoteUser: {
			"PHONE":                         {},
			id.KeyID(testMasterPublic):      {},
			id.KeyID(testSelfSigningPublic): {},
		},
	}, signatureOverlayTargets(req, keys, testRequester))
	assert.Nil(t, signatureOverlayTargets(req, keys, ""))
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

func (k testSigningKey) crossSigningKey(userID id.UserID, usage id.CrossSigningUsage) mautrix.CrossSigningKeys {
	return mautrix.CrossSigningKeys{
		UserID: userID,
		Usage:  []id.CrossSigningUsage{usage},
		Keys:   map[id.KeyID]id.Ed25519{k.keyID(): k.public},
	}
}

func (k testSigningKey) sign(t *testing.T, object mautrix.ReqKeysSignatures, signer id.UserID) mautrix.ReqKeysSignatures {
	t.Helper()
	signed, err := util.SignJSON(mustMarshal(t, object), signer.String(), k.keyID().String(), k.private)
	require.NoError(t, err)
	var out mautrix.ReqKeysSignatures
	require.NoError(t, json.Unmarshal(signed, &out))
	return out
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
			MasterKey:      mustMarshal(t, cachedMaster),
			SelfSigningKey: mustMarshal(t, remoteSelfSigning.crossSigningKey(testRemoteUser, id.XSUsageSelfSigning)),
		},
	}
	userSigningKey := requesterUserSigning.crossSigningKey(testRequester, id.XSUsageUserSigning)

	unsignedTarget := mautrix.ReqKeysSignatures{
		UserID: testRemoteUser,
		Usage:  []id.CrossSigningUsage{id.XSUsageMaster},
		Keys:   map[id.KeyID]string{remoteMaster.keyID(): remoteMaster.public.String()},
	}
	signedTarget := requesterUserSigning.sign(t, unsignedTarget, testRequester)

	withOwnerSignature := unsignedTarget
	withOwnerSignature.Signatures = cachedMaster.Signatures
	withOwnerSignature = requesterUserSigning.sign(t, withOwnerSignature, testRequester)

	mismatched := unsignedTarget
	mismatched.Usage = []id.CrossSigningUsage{id.XSUsageSelfSigning}
	mismatched = requesterUserSigning.sign(t, mismatched, testRequester)

	tampered := unsignedTarget
	tampered.Signatures = mismatched.Signatures

	deviceTarget := signedTarget
	deviceTarget.DeviceID = "PHONE"

	noMaster := *cache
	noMaster.MasterKey = nil

	masterKey := remoteMaster.public.String()

	for _, tc := range []struct {
		name           string
		userSigningKey *mautrix.CrossSigningKeys
		cache          *types.RemoteDeviceCache
		targetKey      string
		target         mautrix.ReqKeysSignatures
		errCode        string
	}{
		{"accepted", &userSigningKey, cache, masterKey, signedTarget, ""},
		{"owner signatures ignored", &userSigningKey, cache, masterKey, withOwnerSignature, ""},
		{"device target", &userSigningKey, cache, "PHONE", deviceTarget, mautrix.MInvalidParam.ErrCode},
		{"uncached or no valid snapshot", &userSigningKey, nil, masterKey, signedTarget, mautrix.MNotFound.ErrCode},
		{"no cached master key", &userSigningKey, &noMaster, masterKey, signedTarget, mautrix.MNotFound.ErrCode},
		{"not the master key", &userSigningKey, cache, remoteSelfSigning.public.String(), signedTarget, mautrix.MNotFound.ErrCode},
		{"content mismatch", &userSigningKey, cache, masterKey, mismatched, mautrix.MInvalidParam.ErrCode},
		{"requester has no user-signing key", nil, cache, masterKey, signedTarget, errCodeInvalidSignature},
		{"no requester signature", &userSigningKey, cache, masterKey, unsignedTarget, errCodeInvalidSignature},
		{"signed by another requester key", &userSigningKey, cache, masterKey, requesterMaster.sign(t, unsignedTarget, testRequester), errCodeInvalidSignature},
		{"signature does not verify", &userSigningKey, cache, masterKey, tampered, errCodeInvalidSignature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := RemoteSignatureTargetFailure(testRequester, tc.userSigningKey, tc.cache, tc.targetKey, tc.target)
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
