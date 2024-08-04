package util_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

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
		},
		ID:           "abc",
		Depth:        3,
		PrevEventIDs: []id.EventID{},
		AuthEventIDs: []id.EventID{},
		Unsigned: map[string]any{
			"age_ts": 1000000,
		},
		RoomVersion: "5",
		Origin:      "domain",
		Timestamp:   1000000,
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
