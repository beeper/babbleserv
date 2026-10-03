package util_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// This test uses the test vectors defined in the Matrix spec:
// https://spec.matrix.org/v1.10/appendices/#cryptographic-test-vectors
func TestEventHashAndSign(t *testing.T) {
	ev := &types.Event{
		PartialEvent: types.PartialEvent{
			RoomID: id.RoomID("!x:domain"),
			Sender: id.UserID("@a:domain"),

			Type:    event.NewEventType("X"),
			Content: []byte(`{}`),
			Unsigned: map[string]any{
				"age_ts": 1000000,
			},
			Timestamp: 1000000,
		},
		ID:           "abc",
		Depth:        3,
		PrevEventIDs: []id.EventID{},
		AuthEventIDs: []id.EventID{},

		RoomVersion: "5",
		Origin:      "domain",
	}

	t.Run("test event content hashing", func(t *testing.T) {
		hash, err := util.GetEventContentHash(ev)
		require.NoError(t, err)
		assert.Equal(t, "5jM4wQpv6lnBo7CLIghJuHdW+s2CMBJPUOGOC89ncos", hash)

		ev.Hashes = map[string]string{
			"sha256": hash,
		}
	})

	keySeed := "YJDBA9Xnr2sVqXD9Vj7XVUnmFZcZrlw8Md7kMW+3XA1"
	keySeedBytes, err := base64.RawStdEncoding.DecodeString(keySeed)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(keySeedBytes)

	expectedSignature := "KxwGjPSDEtvnFgU00fwFz+l6d2pJM6XBIaMEn81SXPTRl16AqLAYqfIReFGZlHi5KLjAWbOoMszkwsQma+lYAg"

	t.Run("test event sign and verify", func(t *testing.T) {
		// Test GetEventSignature is correct
		evSignature, err := util.GetEventSignature(ev, key)
		require.NoError(t, err)
		assert.Equal(t, expectedSignature, evSignature)

		// Now test event -> JSON -> sign gives the same signature
		eventJSON, err := json.Marshal(ev)
		require.NoError(t, err)
		signedEvent, err := util.SignJSON(eventJSON, "domain", "ed25519:1", key)
		require.NoError(t, err)
		signature := gjson.GetBytes(signedEvent, "signatures.domain.ed25519:1").String()
		assert.Equal(t, expectedSignature, signature)

		// And verifying works
		err = util.VerifyJSON(signedEvent, "domain", "ed25519:1", key.Public().(ed25519.PublicKey))
		require.NoError(t, err)
	})

	t.Run("test event multiple sign and verify", func(t *testing.T) {
		eventJSON, err := json.Marshal(ev)
		require.NoError(t, err)

		signedEvent, err := util.SignJSON(eventJSON, "domain.com", "ed25519:1", key)
		require.NoError(t, err)

		signedEvent, err = util.SignJSON(signedEvent, "domain.com", "ed25519:2", key)
		require.NoError(t, err)

		signedEvent, err = util.SignJSON(signedEvent, "anotherdomain", "ed25519:1", key)
		require.NoError(t, err)

		assert.Equal(t, expectedSignature, gjson.GetBytes(signedEvent, "signatures.domain\\.com.ed25519:1").String())
		assert.Equal(t, expectedSignature, gjson.GetBytes(signedEvent, "signatures.domain\\.com.ed25519:2").String())
		assert.Equal(t, expectedSignature, gjson.GetBytes(signedEvent, "signatures.anotherdomain.ed25519:1").String())

		err = util.VerifyJSON(signedEvent, "domain.com", "ed25519:1", key.Public().(ed25519.PublicKey))
		require.NoError(t, err)
		err = util.VerifyJSON(signedEvent, "domain.com", "ed25519:2", key.Public().(ed25519.PublicKey))
		require.NoError(t, err)
		err = util.VerifyJSON(signedEvent, "anotherdomain", "ed25519:1", key.Public().(ed25519.PublicKey))
		require.NoError(t, err)
	})
}

func TestEventReferenceHash(t *testing.T) {
	// Grabbed a few recent events from #MatrixHQ
	eventIDToJSON := map[id.EventID][]byte{
		"$KYnkyaM7qO1Po8cFpZ6I_mKEAKZHYa1JIfMwkSvdkGo": []byte(`{"auth_events":["$nZKmKBHk7MvKbeF1MFU1ynXVrw1UlfciDsCwbMlmBKc","$jKWngIwBoV3n1rP-Ywpv2epgd0GCbGMJUyXXaUOhDM8","$MaPUZG2fNleIE_gUYDmOy-nj0Zom8mQ5FNM7gEAVI6I"],"content":{"body":"theyve been going around matrix for a long time lol","m.mentions":{},"msgtype":"m.text"},"depth":660767,"hashes":{"sha256":"VnIqS+DaDPlCfYpN4srFqsiz9zPmsLYNA3FrDMFDdeA"},"origin":"rory.gay","origin_server_ts":1719024388003,"prev_events":["$OyhdvjNGS9EOyaXgCGtlsOAwpC1iiz-JHPCITqxzSp0"],"room_id":"!OGEhHVWSdvArJzumhm:matrix.org","sender":"@emma:rory.gay","type":"m.room.message","signatures":{"rory.gay":{"ed25519:a_VUxK":"4XNUZJiKnDs5Afcs5tnnk97tn1cqIUCEQ23zNYxQpqs4YMhdNUaiKKuWRgXWml63CI2ppcfMaoOHHvD8x3D3Ag"}},"unsigned":{}}`),

		"$0cCwNmVsVD__8HORXMkPc_M5xeS8-o57ERO0QMxPHQo": []byte(`{"auth_events":["$MaPUZG2fNleIE_gUYDmOy-nj0Zom8mQ5FNM7gEAVI6I","$UYWIGiJzKJEDoGas39f_ArFfdU06Ygzs0dXzJrVQ3TY","$jKWngIwBoV3n1rP-Ywpv2epgd0GCbGMJUyXXaUOhDM8","$SqIDVqBmM7wAqYNxbTW-jU2Y-da1ftQmwdwfWlTDlCU"],"content":{"avatar_url":"mxc://matrix.org/vlmaCKYyQjeqJdeqpocwwoUj","displayname":"Jacob Hall (Snake)","membership":"join"},"depth":660786,"hashes":{"sha256":"dRUBpz+o0eC2z6rNiL25bzjjNbF2XurMNJCRHp8UOLE"},"origin":"matrix.org","origin_server_ts":1719048063670,"prev_events":["$rx6nIDVAideiwnK7YV06gsecEAv-p1qc6kMl3nHi0lM"],"room_id":"!OGEhHVWSdvArJzumhm:matrix.org","sender":"@bloodymew:matrix.org","state_key":"@bloodymew:matrix.org","type":"m.room.member","signatures":{"matrix.org":{"ed25519:a_RXGa":"s0RBE5MsUO7QSm8/jQKsfxvwvs7jbdRU6zOpcJfVVRQutmsEtLBFfztBHUbFI4Q5hn08FcuYmljLYlaQjT2bCQ"}},"unsigned":{"replaces_state":"$SqIDVqBmM7wAqYNxbTW-jU2Y-da1ftQmwdwfWlTDlCU"}}`),

		"$1rOEdWKN1mYUhrhc1kSlejIPP44PGWqlzInVc3DlLcw": []byte(`{"auth_events":["$MaPUZG2fNleIE_gUYDmOy-nj0Zom8mQ5FNM7gEAVI6I","$jKWngIwBoV3n1rP-Ywpv2epgd0GCbGMJUyXXaUOhDM8","$UYWIGiJzKJEDoGas39f_ArFfdU06Ygzs0dXzJrVQ3TY"],"content":{"avatar_url":"mxc://mozilla.org/cd0b5cbb2270d23b70207a42ce599d638d62171b1804179563454922752","displayname":"qui","membership":"join"},"depth":660773,"hashes":{"sha256":"NQ2+ocK+y6zL6WeiVWB1qFdihTB+DToWJWXRMppRypk"},"origin":"mozilla.org","origin_server_ts":1719035764005,"prev_events":["$1czuYSO5-mAE7HtcofMFalw83jCxocxJcpatRFXZr9A"],"room_id":"!OGEhHVWSdvArJzumhm:matrix.org","sender":"@qui:mozilla.org","state_key":"@qui:mozilla.org","type":"m.room.member","signatures":{"mozilla.org":{"ed25519:0":"N6+WdFB2RjWOanlWqRIYOQEW2UzZidwyPkme+zB/RnuXmc8q8rA/6pPRjm5Nl0m9yVfMMnBnULsnCm4Sl+ygBw"}},"unsigned":{}}`),
	}

	roomSpec, err := gomatrixserverlib.GetRoomVersion("5")
	require.NoError(t, err)

	for eventID, b := range eventIDToJSON {
		b, err = roomSpec.RedactEventJSON(b)
		require.NoError(t, err)
		refHash, err := util.GetRefHashForRedactedBytes(b, "5")
		require.NoError(t, err)
		assert.Equal(t, eventID, refHash)
	}
}

func TestVerifyEventJSON(t *testing.T) {
	eventJSON := []byte(`{"type":"m.room.create","room_id":"!zultniSWROlGPODHZM:matrix.org","sender":"@fizzadar:matrix.org","state_key":"","content":{"creator":"@fizzadar:matrix.org"},"hashes":{"sha256":"7bfddEoElN2fD8e4jGuITmGWzAvpyp87ON+Y2B81xew"},"signatures":{"matrix.org":{"ed25519:a_RXGa":"vqHo3anxGIYEyNuN8HVICgsH9QjiDcVdY2n1NHBEo0XvlZmrMj4bqi1uaFcCIgKw2tkS/p0ZZPPuB0ampXoECQ"}},"depth":1,"prev_events":[],"prev_state":[],"auth_events":[],"origin":"matrix.org","origin_server_ts":1661245020332}`)

	// JSON unmarshal/marshal the event to verify that our event type has all fields
	var ev types.Event
	err := json.Unmarshal(eventJSON, &ev)
	require.NoError(t, err)
	assert.Equal(t, []id.EventID{}, ev.PrevState)

	b, err := json.Marshal(&ev)
	require.NoError(t, err)

	pubKey, err := util.Base64Decode("l8Hft5qXKn1vfHrg3p4+W8gELQVo8N13JkluMfmn2sQ")
	require.NoError(t, err)

	err = util.VerifyJSON(b, "matrix.org", "ed25519:a_RXGa", pubKey)
	require.NoError(t, err)
}

func TestV12CreateHashAndRoomID(t *testing.T) {
	empty := ""
	create := &types.Event{RoomVersion: "12", PartialEvent: types.PartialEvent{Type: event.StateCreate, StateKey: &empty, Sender: "@alice:example.com", Timestamp: 1234, Content: json.RawMessage(`{"room_version":"12"}`)}}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	require.NoError(t, util.HashAndSignEvent(create, "example.com", "ed25519:test", key))
	create.RoomID = id.RoomID("!" + create.ID[1:])
	raw, err := json.Marshal(create)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(raw, "room_id").Exists())
	native, err := create.MustGetRoomSpec().NewEventFromTrustedJSON(raw, false)
	require.NoError(t, err)
	require.Equal(t, create.ID.String(), native.EventID())
	require.Equal(t, create.RoomID.String(), native.RoomID().String())
	decoded := &types.Event{RoomVersion: "12"}
	require.NoError(t, json.Unmarshal(raw, decoded))
	require.Equal(t, create.RoomID, decoded.RoomID)
	// Re-signing in the room transaction does not change the precomputed ID.
	before := create.ID
	require.NoError(t, util.HashAndSignEvent(create, "example.com", "ed25519:test", key))
	require.Equal(t, before, create.ID)
	malformed, err := sjson.SetBytes(raw, "room_id", create.RoomID)
	require.NoError(t, err)
	require.Error(t, json.Unmarshal(malformed, &types.Event{RoomVersion: "12"}))
	malformed, err = sjson.DeleteBytes(raw, "state_key")
	require.NoError(t, err)
	require.Error(t, json.Unmarshal(malformed, &types.Event{RoomVersion: "12"}))
	redacted, err := create.GetRedactedEvent()
	require.NoError(t, err)
	require.Equal(t, create.RoomID, redacted.RoomID)
	require.Equal(t, create.RoomVersion, redacted.RoomVersion)
}

type keysClient struct {
	fclient.FederationClient
	key ed25519.PublicKey
}

func (c keysClient) GetServerKeys(context.Context, spec.ServerName) (gomatrixserverlib.ServerKeys, error) {
	var keys gomatrixserverlib.ServerKeys
	keys.VerifyKeys = map[gomatrixserverlib.KeyID]gomatrixserverlib.VerifyKey{"ed25519:test": {Key: spec.Base64Bytes(c.key)}}
	keys.ValidUntilTS = spec.AsTimestamp(time.Now().Add(time.Hour))
	return keys, nil
}

func TestVerifyRemoteEvents(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signed := func(body string) *types.Event {
		ev := &types.Event{
			PartialEvent: types.PartialEvent{
				RoomID:    "!room:example.com",
				Sender:    "@alice:example.com",
				Type:      event.EventMessage,
				Timestamp: time.Now().UnixMilli(),
				Content:   json.RawMessage(`{"msgtype":"m.text","body":"` + body + `"}`),
			},
			RoomVersion:  "11",
			PrevEventIDs: []id.EventID{},
			AuthEventIDs: []id.EventID{},
		}
		require.NoError(t, util.HashAndSignEvent(ev, "example.com", "ed25519:test", key))
		return ev
	}
	pdu := func(ev *types.Event) json.RawMessage {
		b, err := json.Marshal(ev)
		require.NoError(t, err)
		return b
	}

	valid := signed("valid")
	redacted := signed("redacted")
	redactedPDU, err := sjson.SetBytes(pdu(redacted), "content.body", "altered")
	require.NoError(t, err)
	forged, err := sjson.SetBytes(pdu(signed("forged")), "sender", "@mallory:example.com")
	require.NoError(t, err)

	seenIDs := map[id.EventID]struct{}{}
	evs, err := util.VerifyRemoteEvents(
		context.Background(),
		[]json.RawMessage{json.RawMessage(`{"content":[`), forged, redactedPDU, pdu(valid), pdu(valid)},
		"11", util.NewKeyStore(keysClient{key: key.Public().(ed25519.PublicKey)}), seenIDs,
	)
	require.NoError(t, err)

	// An event that cannot be parsed or fails verification is skipped, and a redacted one is kept
	// redacted
	require.Len(t, evs, 2)
	assert.Equal(t, redacted.ID, evs[0].ID)
	assert.True(t, evs[0].Redacted)
	assert.JSONEq(t, `{}`, string(evs[0].Content))
	assert.Equal(t, valid.ID, evs[1].ID)
	assert.False(t, evs[1].Redacted)
	assert.Equal(t, map[id.EventID]struct{}{redacted.ID: {}, valid.ID: {}}, seenIDs)
}

// Signs a member event as its sender would, over its content exactly as given and without the
// checks this server makes of the events it signs
func signedRemoteMember(t *testing.T, key ed25519.PrivateKey, stateKey, content string) (id.EventID, json.RawMessage) {
	ev := &types.Event{
		PartialEvent: types.PartialEvent{
			RoomID: "!room:example.com", Sender: "@alice:example.com", Type: event.StateMember, StateKey: &stateKey,
			Timestamp: time.Now().UnixMilli(), Content: json.RawMessage(content),
		},
		RoomVersion:  "11",
		PrevEventIDs: []id.EventID{},
		AuthEventIDs: []id.EventID{},
	}
	hash, err := util.GetEventContentHash(ev)
	require.NoError(t, err)
	ev.Hashes = map[string]string{"sha256": hash}
	signature, eventID, err := util.GetEventSignatureAndRefrerenceHash(ev, key)
	require.NoError(t, err)
	ev.Signatures, ev.ID = map[string]map[string]string{"example.com": {"ed25519:test": signature}}, eventID
	b, err := json.Marshal(ev)
	require.NoError(t, err)
	return eventID, b
}

func TestVerifyRequestEventWithDuplicateKeys(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	// The sender hashed the content with both members, redaction keeps the last membership
	eventID, pdu := signedRemoteMember(t, key, "@bob:example.com", `{"membership":"join","displayname":"Bob","membership":"ban"}`)
	ev, respErr := util.ParseRequestJSON[types.Event](httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(pdu)))
	require.Nil(t, respErr)
	ev.RoomVersion = "11"
	verified, verifyErr, err := util.VerifyRemoteEvent(
		context.Background(), &ev,
		util.NewKeyStore(keysClient{key: key.Public().(ed25519.PublicKey)}),
	)
	require.NoError(t, err)
	require.NoError(t, verifyErr)
	require.NotNil(t, verified)
	assert.Equal(t, eventID, verified.ID)
	assert.True(t, verified.Redacted)
	assert.JSONEq(t, `{"membership":"ban"}`, string(verified.Content))
	assert.Equal(t, event.MembershipBan, verified.Membership())
}
