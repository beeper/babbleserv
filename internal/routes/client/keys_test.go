package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
)

func TestRemoteKeyQueryTimeout(t *testing.T) {
	for _, tc := range []struct {
		timeoutMillis int64
		expected      time.Duration
	}{
		{0, 10 * time.Second},
		{-1, 10 * time.Second},
		{1, time.Second},
		{999, time.Second},
		{1000, time.Second},
		{2500, 2500 * time.Millisecond},
		{10_000, 10 * time.Second},
		{60_000, 10 * time.Second},
		{math.MaxInt64, 10 * time.Second},
	} {
		assert.Equal(t, tc.expected, remoteKeyQueryTimeout(tc.timeoutMillis), "timeout %d", tc.timeoutMillis)
	}
}

func TestRemoteKeyQueryFailure(t *testing.T) {
	notFound := mautrix.HTTPError{
		Response:  &http.Response{StatusCode: http.StatusNotFound},
		RespError: &mautrix.RespError{ErrCode: mautrix.MNotFound.ErrCode, Err: "no such user", StatusCode: http.StatusNotFound},
	}
	unreadable := mautrix.HTTPError{
		Response:     &http.Response{StatusCode: http.StatusOK},
		Message:      "failed to read response body",
		WrappedError: mautrix.ErrBodyReadReachedLimit,
	}
	unreachable := mautrix.HTTPError{Message: "request error", WrappedError: errors.New("connection refused")}
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"http error", notFound, http.StatusNotFound},
		{"wrapped http error", fmt.Errorf("query keys: %w", notFound), http.StatusNotFound},
		{"unreadable success", unreadable, http.StatusServiceUnavailable},
		{"request error", unreachable, http.StatusServiceUnavailable},
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"connection error", errors.New("connection refused"), http.StatusServiceUnavailable},
	} {
		assert.Equal(t, map[string]any{"status": tc.status, "message": tc.err.Error()}, remoteKeyQueryFailure(tc.err), tc.name)
	}
}

func TestRemoteKeyQueryBatches(t *testing.T) {
	missing := shared.MissingUserKeysByServer{
		"big.example":   {},
		"small.example": {"@alice:small.example": {"ALICEDEVICE"}},
	}
	for i := range 2*maxRemoteKeyQueryBatchUsers + 1 {
		missing["big.example"][id.UserID(fmt.Sprintf("@user%d:big.example", i))] = nil
	}

	batches := remoteKeyQueryBatches(missing)
	require.Len(t, batches, 4)

	assert.ElementsMatch(t, []string{"big.example", "small.example"}, []string{batches[0].server, batches[1].server})
	got := make(shared.MissingUserKeysByServer)
	for i, batch := range batches {
		assert.LessOrEqual(t, len(batch.users), maxRemoteKeyQueryBatchUsers)
		if i > 0 {
			assert.LessOrEqual(t, batches[i-1].round, batch.round)
		}
		if got[batch.server] == nil {
			got[batch.server] = make(map[id.UserID]mautrix.DeviceIDList)
		}
		for userID, deviceIDs := range batch.users {
			assert.NotContains(t, got[batch.server], userID)
			got[batch.server][userID] = deviceIDs
		}
	}
	assert.Equal(t, missing, got)
}

func parseTestRequest[T any](t *testing.T, body string) T {
	t.Helper()
	req, respErr := util.ParseRequestJSON[T](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	require.Nil(t, respErr)
	return req
}

func TestSignatureUploadTargetKeepsRawJSON(t *testing.T) {
	const target = `{"user_id":"@bob:remote.example","usage":["master"],"keys":{"ed25519:K":"K"},"org.example.extra":"signed","signatures":{"@alice:local.example":{"ed25519:USK":"sig"}}}`
	req := parseTestRequest[map[id.UserID]map[string]types.SignatureUploadTarget](t, `{"@bob:remote.example":{"K":`+target+`}}`)

	parsed := req["@bob:remote.example"]["K"]
	assert.Equal(t, target, string(parsed.Raw))
	assert.Equal(t, []id.CrossSigningUsage{id.XSUsageMaster}, parsed.View.Usage)
	assert.Equal(t, signatures.Signatures{"@alice:local.example": {"ed25519:USK": "sig"}}, parsed.View.Signatures)

	_, respErr := util.ParseRequestJSON[map[id.UserID]map[string]types.SignatureUploadTarget](httptest.NewRequest(
		http.MethodPost, "/", strings.NewReader(`{"@bob:remote.example":{"K":{"keys":"not an object"}}}`),
	))
	require.NotNil(t, respErr)
	assert.Equal(t, mautrix.MBadJSON.ErrCode, respErr.ErrCode)
}

const (
	testUploadUser   = id.UserID("@alice:local.example")
	testUploadDevice = id.DeviceID("DEVICE")
)

type testUploadKey struct {
	public  string
	private ed25519.PrivateKey
}

func newTestUploadKey(t *testing.T) testUploadKey {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return testUploadKey{public: util.Base64Encode(public), private: private}
}

func (k testUploadKey) keyID() string {
	return "ed25519:" + k.public
}

func (k testUploadKey) verifyKey() ed25519.PublicKey {
	return k.private.Public().(ed25519.PublicKey)
}

func (k testUploadKey) sign(t *testing.T, object any, keyID string) json.RawMessage {
	t.Helper()
	raw, ok := object.(json.RawMessage)
	if !ok {
		var err error
		raw, err = json.Marshal(object)
		require.NoError(t, err)
	}
	signed, err := util.SignJSON(raw, testUploadUser.String(), keyID, k.private)
	require.NoError(t, err)
	return signed
}

func testUploadDeviceKeys(t *testing.T, device testUploadKey, extra map[string]any) json.RawMessage {
	object := map[string]any{
		"user_id":    testUploadUser,
		"device_id":  testUploadDevice,
		"algorithms": []id.Algorithm{id.AlgorithmOlmV1, id.AlgorithmMegolmV1},
		"keys": map[string]string{
			"ed25519:" + testUploadDevice.String():    device.public,
			"curve25519:" + testUploadDevice.String(): util.Base64Encode([]byte("curve25519 public key 32 bytes!!")),
		},
	}
	maps.Copy(object, extra)
	return device.sign(t, object, "ed25519:"+testUploadDevice.String())
}

func setTestField(t *testing.T, object json.RawMessage, path string, value any) json.RawMessage {
	t.Helper()
	out, err := sjson.SetBytes(object, path, value)
	require.NoError(t, err)
	return out
}

// Stores the key as the database does, its signatures apart, and reads it back with the owner's
func storedAndRead[T any](t *testing.T, uploaded types.SignedJSON[T], owner map[id.KeyID]string) types.SignedJSON[T] {
	t.Helper()
	stored, err := uploaded.WithoutSignatures()
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(stored.Raw, "signatures").Exists())
	read, err := stored.WithOwnerSignatures(testUploadUser, owner)
	require.NoError(t, err)
	return read
}

func mustCanonical(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	canonical, err := signedjson.Canonical(raw)
	require.NoError(t, err)
	return canonical
}

func TestParseUploadedDeviceKeysKeepsUnknownSignedMembers(t *testing.T) {
	device := newTestUploadKey(t)
	uploaded := setTestField(t, testUploadDeviceKeys(t, device, map[string]any{"dehydrated": true}), "unsigned", map[string]any{"device_display_name": "Phone"})
	req := parseTestRequest[reqUploadKeys](t, `{"device_keys":`+string(uploaded)+`}`)

	keys, respErr := parseUploadedDeviceKeys(testUploadUser, testUploadDevice, req.DeviceKeys)
	require.Nil(t, respErr)
	assert.Equal(t, string(mustCanonical(t, uploaded)), string(keys.Raw))
	assert.True(t, gjson.GetBytes(keys.Raw, "dehydrated").Bool())
	assert.NoError(t, signedjson.Verify(keys.Raw, testUploadUser.String(), "ed25519:DEVICE", device.verifyKey()))

	read := storedAndRead(t, keys, keys.View.Signatures[testUploadUser])
	assert.Equal(t, string(keys.Raw), string(mustCanonical(t, read.Raw)))
	assert.NoError(t, signedjson.Verify(read.Raw, testUploadUser.String(), "ed25519:DEVICE", device.verifyKey()))
	assert.Equal(t, keys.View, read.View)
}

func TestParseUploadedDeviceKeysRejects(t *testing.T) {
	device := newTestUploadKey(t)
	valid := testUploadDeviceKeys(t, device, nil)
	other := newTestUploadKey(t)

	for name, tc := range map[string]struct {
		raw     json.RawMessage
		errCode string
	}{
		"other user":              {testUploadDeviceKeys(t, device, map[string]any{"user_id": "@bob:local.example"}), mautrix.MInvalidParam.ErrCode},
		"other device":            {testUploadDeviceKeys(t, device, map[string]any{"device_id": "OTHER"}), mautrix.MInvalidParam.ErrCode},
		"case-variant user ID":    {device.sign(t, setTestField(t, sjsonDelete(t, testUploadDeviceKeys(t, device, nil), "user_id"), "User_ID", testUploadUser), "ed25519:DEVICE"), mautrix.MInvalidParam.ErrCode},
		"no algorithms":           {testUploadDeviceKeys(t, device, map[string]any{"algorithms": []string{}}), mautrix.MBadJSON.ErrCode},
		"content changed":         {setTestField(t, valid, "dehydrated", true), mautrix.MInvalidParam.ErrCode},
		"signed by another key":   {other.sign(t, sjsonDelete(t, valid, "signatures"), "ed25519:DEVICE"), mautrix.MInvalidParam.ErrCode},
		"case-variant signatures": {device.sign(t, setTestField(t, sjsonDelete(t, valid, "signatures"), "Signatures", map[string]any{"@bob:local.example": map[string]string{"ed25519:X": "sig"}}), "ed25519:DEVICE"), mautrix.MBadJSON.ErrCode},
	} {
		keys, err := types.ParseSignedJSON[mautrix.DeviceKeys](tc.raw)
		require.NoError(t, err, name)
		_, respErr := parseUploadedDeviceKeys(testUploadUser, testUploadDevice, keys)
		require.NotNil(t, respErr, name)
		assert.Equal(t, tc.errCode, respErr.ErrCode, name)
	}
}

func sjsonDelete(t *testing.T, object json.RawMessage, path string) json.RawMessage {
	t.Helper()
	out, err := sjson.DeleteBytes(object, path)
	require.NoError(t, err)
	return out
}

func TestParseUploadedOneTimeKeys(t *testing.T) {
	device := newTestUploadKey(t)
	deviceKeys, err := types.ParseSignedJSON[mautrix.DeviceKeys](testUploadDeviceKeys(t, device, nil))
	require.NoError(t, err)
	curve := util.Base64Encode([]byte("one time curve25519 key 32 bytes"))
	oneTimeKey := device.sign(t, map[string]any{"key": curve, "fallback": false, "org.example.extra": "signed"}, "ed25519:DEVICE")
	fallbackKey := device.sign(t, map[string]any{"key": curve, "fallback": true, "org.example.extra": "signed"}, "ed25519:DEVICE")
	req := parseTestRequest[reqUploadKeys](t, `{
		"one_time_keys": {"signed_curve25519:A": `+string(oneTimeKey)+`},
		"fallback_keys": {"signed_curve25519:B": `+string(fallbackKey)+`}
	}`)

	for name, tc := range map[string]struct {
		uploaded types.SignedOneTimeKey
		fallback bool
		raw      json.RawMessage
	}{
		"one-time key with an explicit fallback false": {req.OneTimeKeys["signed_curve25519:A"], false, oneTimeKey},
		"fallback key": {req.FallbackKeys["signed_curve25519:B"], true, fallbackKey},
	} {
		key, respErr := parseUploadedOneTimeKey(testUploadUser, deviceKeys, tc.uploaded, tc.fallback)
		require.Nil(t, respErr, name)
		assert.Equal(t, string(mustCanonical(t, tc.raw)), string(key.Raw), name)
		assert.Equal(t, "signed", gjson.GetBytes(key.Raw, gjson.Escape("org.example.extra")).String(), name)
		assert.True(t, gjson.GetBytes(key.Raw, "fallback").Exists(), name)
		assert.NoError(t, signedjson.Verify(key.Raw, testUploadUser.String(), "ed25519:DEVICE", device.verifyKey()), name)

		_, respErr = parseUploadedOneTimeKey(testUploadUser, deviceKeys, tc.uploaded, !tc.fallback)
		require.NotNil(t, respErr, name)
		assert.Equal(t, mautrix.MInvalidParam.ErrCode, respErr.ErrCode, name)
	}

	tampered, err := types.ParseSignedJSON[types.OneTimeKey](setTestField(t, oneTimeKey, "key", util.Base64Encode([]byte("another curve25519 key, 32 bytes"))))
	require.NoError(t, err)
	_, respErr := parseUploadedOneTimeKey(testUploadUser, deviceKeys, tampered, false)
	require.NotNil(t, respErr)
	assert.Equal(t, mautrix.MInvalidParam.ErrCode, respErr.ErrCode)

	_, respErr = util.ParseRequestJSON[reqUploadKeys](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"one_time_keys": {"curve25519:A": "`+curve+`"}}`,
	)))
	require.NotNil(t, respErr, "an unsigned plain-string key")
	assert.Equal(t, mautrix.MBadJSON.ErrCode, respErr.ErrCode)

	reserved, err := types.ParseSignedJSON[types.OneTimeKey](device.sign(t, map[string]any{"key": curve, "Unsigned": map[string]any{}}, "ed25519:DEVICE"))
	require.NoError(t, err)
	_, respErr = parseUploadedOneTimeKey(testUploadUser, deviceKeys, reserved, false)
	require.NotNil(t, respErr, "a case-variant unsigned member")
	assert.Equal(t, mautrix.MBadJSON.ErrCode, respErr.ErrCode)
}

type testCrossSigningUpload struct {
	master, selfSigning, userSigning, device  testUploadKey
	masterKey, selfSigningKey, userSigningKey json.RawMessage
}

func testCrossSigningKey(usage id.CrossSigningUsage, public string, extra map[string]any) map[string]any {
	object := map[string]any{"user_id": testUploadUser, "usage": []id.CrossSigningUsage{usage}, "keys": map[string]string{"ed25519:" + public: public}}
	maps.Copy(object, extra)
	return object
}

func newTestCrossSigningUpload(t *testing.T) testCrossSigningUpload {
	u := testCrossSigningUpload{master: newTestUploadKey(t), selfSigning: newTestUploadKey(t), userSigning: newTestUploadKey(t), device: newTestUploadKey(t)}
	u.masterKey = u.master.sign(t, testCrossSigningKey(id.XSUsageMaster, u.master.public, map[string]any{"org.example.extra": "signed"}), u.master.keyID())
	u.selfSigningKey = u.master.sign(t, testCrossSigningKey(id.XSUsageSelfSigning, u.selfSigning.public, map[string]any{"org.example.extra": "signed"}), u.master.keyID())
	u.userSigningKey = u.master.sign(t, testCrossSigningKey(id.XSUsageUserSigning, u.userSigning.public, nil), u.master.keyID())
	return u
}

func uploadTestCrossSigningKeys(t *testing.T, keys map[string]json.RawMessage) reqUploadCrossSigningKeys {
	body, err := json.Marshal(keys)
	require.NoError(t, err)
	return parseTestRequest[reqUploadCrossSigningKeys](t, string(body))
}

func TestParseUploadedCrossSigningKeys(t *testing.T) {
	u := newTestCrossSigningUpload(t)
	req := uploadTestCrossSigningKeys(t, map[string]json.RawMessage{"master_key": u.masterKey, "self_signing_key": u.selfSigningKey, "user_signing_key": u.userSigningKey})

	upload, master, respErr := parseUploadedCrossSigningKeys(testUploadUser, req, types.SignedCrossSigningKey{})
	require.Nil(t, respErr)
	assert.Equal(t, id.Ed25519(u.master.public), master)
	for name, tc := range map[string]struct {
		key types.SignedCrossSigningKey
		raw json.RawMessage
	}{
		"master":       {upload.Master, u.masterKey},
		"self-signing": {upload.SelfSigning, u.selfSigningKey},
		"user-signing": {upload.UserSigning, u.userSigningKey},
	} {
		assert.Equal(t, string(mustCanonical(t, tc.raw)), string(tc.key.Raw), name)
		read := storedAndRead(t, tc.key, tc.key.View.Signatures[testUploadUser])
		assert.NoError(t, signedjson.Verify(read.Raw, testUploadUser.String(), u.master.keyID(), u.master.verifyKey()), name)
	}
	assert.Equal(t, "signed", gjson.GetBytes(upload.SelfSigning.Raw, gjson.Escape("org.example.extra")).String())
}

// mautrix-go signs a master key with only the device that publishes it
func TestParseUploadedCrossSigningKeysDeviceSignedMaster(t *testing.T) {
	u := newTestCrossSigningUpload(t)
	deviceSigned := u.device.sign(t, testCrossSigningKey(id.XSUsageMaster, u.master.public, nil), "ed25519:"+testUploadDevice.String())
	req := uploadTestCrossSigningKeys(t, map[string]json.RawMessage{"master_key": deviceSigned, "self_signing_key": u.selfSigningKey})

	upload, master, respErr := parseUploadedCrossSigningKeys(testUploadUser, req, types.SignedCrossSigningKey{})
	require.Nil(t, respErr)
	assert.Equal(t, id.Ed25519(u.master.public), master)
	assert.Equal(t, signatures.Signatures{testUploadUser: {"ed25519:" + id.KeyID(testUploadDevice): signaturesOf(t, deviceSigned)[testUploadUser]["ed25519:"+id.KeyID(testUploadDevice)]}}, upload.Master.View.Signatures)
	assert.NoError(t, signedjson.Verify(upload.Master.Raw, testUploadUser.String(), "ed25519:DEVICE", u.device.verifyKey()))
}

func TestParseUploadedCrossSigningKeysKeepsOnlyOwnerSignatures(t *testing.T) {
	u := newTestCrossSigningUpload(t)
	bob := newTestUploadKey(t)
	signedByBob := func(raw json.RawMessage) json.RawMessage {
		signed, err := util.SignJSON(raw, "@bob:local.example", bob.keyID(), bob.private)
		require.NoError(t, err)
		return signed
	}
	req := uploadTestCrossSigningKeys(t, map[string]json.RawMessage{"master_key": signedByBob(u.masterKey), "self_signing_key": signedByBob(u.selfSigningKey)})

	upload, _, respErr := parseUploadedCrossSigningKeys(testUploadUser, req, types.SignedCrossSigningKey{})
	require.Nil(t, respErr)
	for name, key := range map[string]types.SignedCrossSigningKey{"master": upload.Master, "self-signing": upload.SelfSigning} {
		assert.Equal(t, []id.UserID{testUploadUser}, slices.Collect(maps.Keys(key.View.Signatures)), name)
		assert.Equal(t, key.View.Signatures, signaturesOf(t, key.Raw), name)
	}

	device := newTestUploadKey(t)
	deviceReq := parseTestRequest[reqUploadKeys](t, `{"device_keys":`+string(signedByBob(testUploadDeviceKeys(t, device, nil)))+`}`)
	keys, respErr := parseUploadedDeviceKeys(testUploadUser, testUploadDevice, deviceReq.DeviceKeys)
	require.Nil(t, respErr)
	assert.Equal(t, []id.UserID{testUploadUser}, slices.Collect(maps.Keys(keys.View.Signatures)))
	assert.NoError(t, signedjson.Verify(keys.Raw, testUploadUser.String(), "ed25519:DEVICE", device.verifyKey()))
}

func TestParseUploadedCrossSigningKeysRejects(t *testing.T) {
	u := newTestCrossSigningUpload(t)
	storedMaster, err := types.ParseSignedJSON[mautrix.CrossSigningKeys](u.masterKey)
	require.NoError(t, err)
	selfSigned := func(object map[string]any) json.RawMessage {
		return u.master.sign(t, object, u.master.keyID())
	}

	for name, tc := range map[string]struct {
		keys    map[string]json.RawMessage
		message string
	}{
		"master of another user": {map[string]json.RawMessage{"master_key": selfSigned(testCrossSigningKey(id.XSUsageMaster, u.master.public, map[string]any{"user_id": "@bob:local.example"}))}, "user ID"},
		"master usage":           {map[string]json.RawMessage{"master_key": selfSigned(testCrossSigningKey(id.XSUsageSelfSigning, u.master.public, nil))}, "usage"},
		"self-signing usage":     {map[string]json.RawMessage{"self_signing_key": selfSigned(testCrossSigningKey(id.XSUsageMaster, u.selfSigning.public, nil))}, "usage"},
		"user-signing usage":     {map[string]json.RawMessage{"user_signing_key": selfSigned(testCrossSigningKey(id.XSUsageSelfSigning, u.userSigning.public, nil))}, "usage"},
		"two keys": {map[string]json.RawMessage{"self_signing_key": selfSigned(testCrossSigningKey(id.XSUsageSelfSigning, u.selfSigning.public, map[string]any{
			"keys": map[string]string{u.selfSigning.keyID(): u.selfSigning.public, u.userSigning.keyID(): u.userSigning.public},
		}))}, "keys"},
		"key ID not its key": {map[string]json.RawMessage{"self_signing_key": selfSigned(testCrossSigningKey(id.XSUsageSelfSigning, u.selfSigning.public, map[string]any{
			"keys": map[string]string{u.userSigning.keyID(): u.selfSigning.public},
		}))}, "does not match its key"},
		"not signed by the master": {map[string]json.RawMessage{"self_signing_key": u.selfSigning.sign(t, testCrossSigningKey(id.XSUsageSelfSigning, u.selfSigning.public, nil), u.selfSigning.keyID())}, "no signature"},
		"master self-signature":    {map[string]json.RawMessage{"master_key": setTestField(t, u.masterKey, "org\\.example\\.extra", "tampered")}, "unable to verify master key"},
		"case-variant signatures": {map[string]json.RawMessage{"self_signing_key": selfSigned(testCrossSigningKey(id.XSUsageSelfSigning, u.selfSigning.public, map[string]any{
			"Signatures": map[string]any{"@bob:local.example": map[string]string{"ed25519:X": "sig"}},
		}))}, "case variant"},
	} {
		_, _, respErr := parseUploadedCrossSigningKeys(testUploadUser, uploadTestCrossSigningKeys(t, tc.keys), storedMaster)
		require.NotNil(t, respErr, name)
		assert.Contains(t, respErr.Err, tc.message, name)
	}
}

func TestParseUploadedCrossSigningKeysPartial(t *testing.T) {
	u := newTestCrossSigningUpload(t)
	storedMaster, err := types.ParseSignedJSON[mautrix.CrossSigningKeys](u.masterKey)
	require.NoError(t, err)
	req := uploadTestCrossSigningKeys(t, map[string]json.RawMessage{"self_signing_key": u.selfSigningKey})

	upload, master, respErr := parseUploadedCrossSigningKeys(testUploadUser, req, storedMaster)
	require.Nil(t, respErr)
	assert.Equal(t, id.Ed25519(u.master.public), master)
	assert.True(t, upload.Master.IsZero())
	assert.True(t, upload.UserSigning.IsZero())
	assert.Equal(t, string(mustCanonical(t, u.selfSigningKey)), string(upload.SelfSigning.Raw))

	_, _, respErr = parseUploadedCrossSigningKeys(testUploadUser, req, types.SignedCrossSigningKey{})
	require.NotNil(t, respErr)
	assert.Equal(t, "No master key available", respErr.Err)

	other := newTestCrossSigningUpload(t)
	otherMaster, err := types.ParseSignedJSON[mautrix.CrossSigningKeys](other.masterKey)
	require.NoError(t, err)
	_, _, respErr = parseUploadedCrossSigningKeys(testUploadUser, req, otherMaster)
	require.NotNil(t, respErr, "a self-signing key not signed by the stored master key")
	assert.Equal(t, mautrix.MInvalidParam.ErrCode, respErr.ErrCode)
}

func signaturesOf(t *testing.T, object json.RawMessage) signatures.Signatures {
	t.Helper()
	signed, err := types.ParseSignedJSON[mautrix.DeviceKeys](object)
	require.NoError(t, err)
	return signed.View.Signatures
}

// Alice signs her own dehydrated device with her self-signing key, over the keys as served
func TestLocalSignatureTargetKeepsUnknownMembers(t *testing.T) {
	device := newTestUploadKey(t)
	selfSigning := newTestUploadKey(t)
	uploaded, err := types.ParseSignedJSON[mautrix.DeviceKeys](testUploadDeviceKeys(t, device, map[string]any{"dehydrated": true}))
	require.NoError(t, err)
	stored := storedAndRead(t, uploaded, uploaded.View.Signatures[testUploadUser])
	signerKeys := map[id.KeyID]string{id.KeyID(selfSigning.keyID()): selfSigning.public, "ed25519:DEVICE": device.public}
	target := func(raw json.RawMessage) types.SignatureUploadTarget {
		parsed, err := types.ParseSignedJSON[mautrix.ReqKeysSignatures](raw)
		require.NoError(t, err)
		return parsed
	}

	signed := target(selfSigning.sign(t, stored.Raw, selfSigning.keyID()))
	assert.True(t, signed.Matches(stored.Raw))
	assert.NoError(t, verifyLocalTargetSignatures(testUploadUser, signed, signerKeys))
	assert.Len(t, signed.View.Signatures[testUploadUser], 2)

	tampered := target(selfSigning.sign(t, setTestField(t, stored.Raw, "dehydrated", false), selfSigning.keyID()))
	assert.False(t, tampered.Matches(stored.Raw))

	unknown := newTestUploadKey(t)
	assert.ErrorContains(t, verifyLocalTargetSignatures(testUploadUser, target(unknown.sign(t, stored.Raw, unknown.keyID())), signerKeys), "unknown signing key")
	relabelled := target(setTestField(t, stored.Raw, "signatures", map[string]any{
		testUploadUser.String(): map[string]string{selfSigning.keyID(): stored.View.Signatures[testUploadUser]["ed25519:DEVICE"]},
	}))
	assert.ErrorContains(t, verifyLocalTargetSignatures(testUploadUser, relabelled, signerKeys), "unable to verify object")
	bob, err := util.SignJSON(stored.Raw, "@bob:local.example", selfSigning.keyID(), selfSigning.private)
	require.NoError(t, err)
	assert.ErrorContains(t, verifyLocalTargetSignatures(testUploadUser, target(bob), signerKeys), "invalid signing user ID")
}

func TestRemoteClaimedKeysPassThrough(t *testing.T) {
	signedKey := json.RawMessage(`{"key":"abc","org.example.extra":true,"signatures":{"@bob:remote.example":{"ed25519:DEVICE":"sig"}}}`)
	plainKey := json.RawMessage(`"plain"`)
	claimed := remoteClaimedKeys(fclient.RespClaimKeys{OneTimeKeys: map[string]map[string]map[string]json.RawMessage{
		"@bob:remote.example": {"DEVICE": {"signed_curve25519:A": signedKey, "curve25519:B": plainKey}},
	}})
	assert.Equal(t, map[id.UserID]map[id.DeviceID]map[id.KeyID]json.RawMessage{
		"@bob:remote.example": {"DEVICE": {"signed_curve25519:A": signedKey, "curve25519:B": plainKey}},
	}, claimed)
}
