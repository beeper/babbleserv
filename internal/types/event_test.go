package types_test

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/json/jsontext"
	"maps"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func TestEventSerializationNoPrevState(t *testing.T) {
	// Check empty event has nil PrevState
	ev := &types.Event{}
	assert.Nil(t, ev.PrevState)

	// And this is maintained when doing msgpack marshal/unmarshal
	b, err := msgpack.Marshal(ev)
	require.NoError(t, err)
	ev = &types.Event{}
	err = msgpack.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.Nil(t, ev.PrevState)

	// And also when doing JSON
	b, err = json.Marshal(ev)
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(b, "prev_state").Exists())
	ev = &types.Event{}
	err = json.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.Nil(t, ev.PrevState)
}

func TestEventSerializationWithPrevState(t *testing.T) {
	// Check empty event has nil PrevState
	ev := &types.Event{
		PrevState: []id.EventID{},
	}
	assert.NotNil(t, ev.PrevState)

	// And this is maintained when doing msgpack marshal/unmarshal
	b, err := msgpack.Marshal(ev)
	require.NoError(t, err)
	ev = &types.Event{}
	err = msgpack.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.NotNil(t, ev.PrevState)

	// And also when doing JSON
	b, err = json.Marshal(ev)
	require.NoError(t, err)
	assert.True(t, gjson.GetBytes(b, "prev_state").Exists())
	ev = &types.Event{}
	err = json.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.NotNil(t, ev.PrevState)
}

func TestDomainlessRoomIDInvertsImplicitCreateEventID(t *testing.T) {
	create := &types.Event{ID: "$abc", PartialEvent: types.PartialEvent{Type: event.StateCreate}, RoomVersion: "12"}
	create.RoomID = create.DomainlessRoomID()
	assert.Equal(t, id.RoomID("!abc"), create.RoomID)
	assert.Equal(t, create.ID, create.ImplicitCreateEventID())

	create.RoomVersion = "11"
	assert.Empty(t, create.DomainlessRoomID())
}

func TestStateTupCompareOrdersTypeFirst(t *testing.T) {
	topic := types.StateTup{Type: event.StateTopic}
	alice := types.MemberStateTup("@alice:example.com")
	bob := types.MemberStateTup("@bob:example.com")
	assert.Negative(t, alice.Compare(bob))
	assert.Negative(t, bob.Compare(topic))
	assert.Positive(t, topic.Compare(alice))
	assert.Zero(t, alice.Compare(alice))
}

func TestAuthDependencyIDsFollowRoomVersionCapabilities(t *testing.T) {
	for _, test := range []struct {
		roomVersion string
		evType      event.Type
		expected    []id.EventID
	}{
		{roomVersion: "11", evType: event.EventMessage, expected: []id.EventID{"$power"}},
		{roomVersion: "12", evType: event.EventMessage, expected: []id.EventID{"$abc", "$power"}},
		{roomVersion: "org.matrix.hydra.11", evType: event.EventMessage, expected: []id.EventID{"$abc", "$power"}},
		{roomVersion: "12", evType: event.StateCreate, expected: []id.EventID{"$power"}},
		{roomVersion: "unknown", evType: event.EventMessage, expected: []id.EventID{"$power"}},
	} {
		ev := &types.Event{
			PartialEvent: types.PartialEvent{RoomID: "!abc", Type: test.evType},
			RoomVersion:  test.roomVersion,
			AuthEventIDs: []id.EventID{"$power"},
		}
		assert.Equal(t, test.expected, ev.AuthDependencyIDs(), test.roomVersion)
	}
	assert.True(t, util.RoomVersionHas("org.matrix.hydra.11", func(impl gomatrixserverlib.IRoomVersion) bool {
		return impl.StateResAlgorithm() == gomatrixserverlib.StateResV2_1
	}))
	assert.False(t, util.RoomVersionHas("11", func(impl gomatrixserverlib.IRoomVersion) bool {
		return impl.StateResAlgorithm() == gomatrixserverlib.StateResV2_1
	}))
}

func TestStoredEventContentIsCompacted(t *testing.T) {
	received := `{ "body" : "hi",  "list": [1, 2] }`
	ev := &types.Event{PartialEvent: types.PartialEvent{Type: event.EventMessage, Content: json.RawMessage(received)}}
	stored := types.MustNewEventFromBytes(ev.ToMsgpack(), ev.ID)
	assert.Equal(t, `{"body":"hi","list":[1,2]}`, string(stored.Content))
	assert.Equal(t, received, string(ev.Content))
}

func TestMembershipRowRoundTrips(t *testing.T) {
	for _, row := range []types.MembershipRow{
		{MembershipTup: types.MembershipTup{EventID: "$join", RoomID: "!room", Membership: event.MembershipJoin}},
		{MembershipTup: types.MembershipTup{EventID: "$invite", RoomID: "!room", Membership: event.MembershipInvite}, Outlier: true},
	} {
		assert.Equal(t, row, types.BytesToMembershipRow(types.MembershipRowToBytes(row)))
		assert.Equal(t, row.MembershipTup, types.BytesToMembershipTup(types.MembershipRowToBytes(row)))
	}
}

const extraMembersPDU = `{
	"type": "m.room.message", "room_id": "!room:example.com", "sender": "@alice:example.com",
	"content": {"body": "hi"}, "origin": "example.com", "origin_server_ts": 1700000000000, "depth": 3,
	"prev_events": ["$prev"], "auth_events": ["$create"], "prev_state": [],
	"hashes": {"sha256": "hash"}, "signatures": {"example.com": {"ed25519:test": "signature"}},
	"unsigned": {"age": 1},
	"msc4354_sticky": { "duration_ms" : 3600000 },
	"org.example.string": "a string \u00e9",
	"org.example.nested": {"list": [1, {"deep": null}], "object": {"a.b": "c"}},
	"org.example.dotted.name": true,
	"org.example.null": null,
	":content": "not the content",
	"": "empty name"
}`

var extraMembers = types.EventExtra{
	"msc4354_sticky":          jsontext.Value(`{ "duration_ms" : 3600000 }`),
	"org.example.string":      jsontext.Value(`"a string \u00e9"`),
	"org.example.nested":      jsontext.Value(`{"list": [1, {"deep": null}], "object": {"a.b": "c"}}`),
	"org.example.dotted.name": jsontext.Value(`true`),
	"org.example.null":        jsontext.Value(`null`),
	":content":                jsontext.Value(`"not the content"`),
	"":                        jsontext.Value(`"empty name"`),
}

// Canonical JSON keeps duplicate members, which comparing decoded JSON would hide
func canonicalJSON(t *testing.T, b []byte) string {
	b, err := gomatrixserverlib.CanonicalJSON(b)
	require.NoError(t, err)
	return string(b)
}

func TestEventExtraMembersRoundTrip(t *testing.T) {
	ev := &types.Event{RoomVersion: "11"}
	require.NoError(t, json.Unmarshal([]byte(extraMembersPDU), ev))
	assert.Equal(t, extraMembers, ev.Extra)
	assert.JSONEq(t, `{"body": "hi"}`, string(ev.Content))

	expected, err := sjson.Delete(extraMembersPDU, "unsigned")
	require.NoError(t, err)
	b, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.Equal(t, canonicalJSON(t, []byte(expected)), canonicalJSON(t, b))

	stored := types.MustNewEventFromBytes(ev.ToMsgpack(), ev.ID)
	compacted := maps.Clone(extraMembers)
	compacted["msc4354_sticky"] = jsontext.Value(`{"duration_ms":3600000}`)
	compacted["org.example.nested"] = jsontext.Value(`{"list":[1,{"deep":null}],"object":{"a.b":"c"}}`)
	assert.Equal(t, compacted, stored.Extra)
	assert.Equal(t, extraMembers, ev.Extra, "compacting on save leaves the event as received")
	b, err = json.Marshal(stored)
	require.NoError(t, err)
	assert.Equal(t, canonicalJSON(t, []byte(expected)), canonicalJSON(t, b))
}

// Hashes a remote event as its sender would, over its JSON exactly as given
func hashedRemoteEvent(t *testing.T, roomVersion, raw string) (id.EventID, []byte) {
	hash, err := util.GetJSONContentHash([]byte(raw))
	require.NoError(t, err)
	pdu, err := sjson.SetBytes([]byte(raw), "hashes.sha256", hash)
	require.NoError(t, err)
	redacted, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion)).RedactEventJSON(pdu)
	require.NoError(t, err)
	eventID, err := util.GetRefHashForRedactedBytes(redacted, gomatrixserverlib.RoomVersion(roomVersion))
	require.NoError(t, err)
	return eventID, pdu
}

func TestEventExtraMembersHashAsSent(t *testing.T) {
	raw := `{
		"type": "m.room.member", "room_id": "!room:example.com", "sender": "@alice:example.com",
		"state_key": "@alice:example.com", "content": {"membership": "join"},
		"origin_server_ts": 1700000000000, "depth": 3, "prev_events": ["$prev"], "auth_events": ["$create"],
		"membership": "join", "msc4354_sticky": {"duration_ms": 3600000}
	}`
	for roomVersion, redactedExtra := range map[string]types.EventExtra{
		"10": {"membership": jsontext.Value(`"join"`)},
		"11": nil,
	} {
		eventID, pdu := hashedRemoteEvent(t, roomVersion, raw)
		ev := &types.Event{RoomVersion: roomVersion}
		require.NoError(t, json.Unmarshal(pdu, ev), roomVersion)
		hash, err := util.GetEventContentHash(ev)
		require.NoError(t, err, roomVersion)
		assert.Equal(t, gjson.GetBytes(pdu, "hashes.sha256").String(), hash, roomVersion)
		refHash, err := util.GetEventReferenceHash(ev)
		require.NoError(t, err, roomVersion)
		assert.Equal(t, eventID, refHash, roomVersion)

		redacted, err := ev.GetRedactedEvent()
		require.NoError(t, err, roomVersion)
		assert.Equal(t, redactedExtra, redacted.Extra, roomVersion)
		refHash, err = util.GetEventReferenceHash(redacted)
		require.NoError(t, err, roomVersion)
		assert.Equal(t, eventID, refHash, roomVersion)

		altered, err := sjson.SetBytes(pdu, "msc4354_sticky.duration_ms", 1)
		require.NoError(t, err, roomVersion)
		alteredEv := &types.Event{RoomVersion: roomVersion}
		require.NoError(t, json.Unmarshal(altered, alteredEv), roomVersion)
		alteredHash, err := util.GetEventContentHash(alteredEv)
		require.NoError(t, err, roomVersion)
		assert.NotEqual(t, hash, alteredHash, roomVersion)
	}
}

func TestDomainlessCreateExtraMembersHashAsSent(t *testing.T) {
	eventID, pdu := hashedRemoteEvent(t, "12", `{
		"type": "m.room.create", "state_key": "", "sender": "@alice:example.com",
		"content": {"room_version": "12"}, "origin_server_ts": 1700000000000, "depth": 1,
		"prev_events": [], "auth_events": [], "msc4354_sticky": {"duration_ms": 3600000}
	}`)
	ev := &types.Event{RoomVersion: "12"}
	require.NoError(t, json.Unmarshal(pdu, ev))
	assert.Equal(t, id.RoomID("!"+eventID[1:]), ev.RoomID)
	hash, err := util.GetEventContentHash(ev)
	require.NoError(t, err)
	assert.Equal(t, gjson.GetBytes(pdu, "hashes.sha256").String(), hash)
	refHash, err := util.GetEventReferenceHash(ev)
	require.NoError(t, err)
	assert.Equal(t, eventID, refHash)
}

func TestEventExtraMembersForClients(t *testing.T) {
	ev := &types.Event{RoomVersion: "11", ID: "$id"}
	require.NoError(t, json.Unmarshal([]byte(extraMembersPDU), ev))
	ev.IsForClientAPI = true
	b, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.Equal(t, `{"sender":"@alice:example.com","content":{"body":"hi"},"origin_server_ts":1700000000000,`+
		`"type":"m.room.message","unsigned":{"age":1},"":"empty name",":content":"not the content",`+
		`"msc4354_sticky":{"duration_ms":3600000},"org.example.dotted.name":true,`+
		`"org.example.nested":{"list":[1,{"deep":null}],"object":{"a.b":"c"}},"org.example.null":null,`+
		`"org.example.string":"a string é","event_id":"$id"}`, string(b))
}

func TestEventExtraMembersLeaveOutModelledAndUnhashedMembers(t *testing.T) {
	ev := &types.Event{}
	require.NoError(t, json.Unmarshal([]byte(`{
		"type": "m.room.member", "cont\u0065nt": {"membership": "join"},
		"membership": "join", "age_ts": 1, "outlier": true, "destinations": {"example.com": 1}
	}`), ev))
	assert.Equal(t, types.EventExtra{"membership": jsontext.Value(`"join"`)}, ev.Extra)
	assert.Equal(t, event.MembershipJoin, ev.Membership())
}

func TestEventRefusedMembers(t *testing.T) {
	for name, members := range map[string]string{
		"case variant of content":        `"Content": {"body": "forged"}`,
		"case variant of hashes":         `"HASHES": {}`,
		"case variant of membership":     `"Membership": "join"`,
		"case variant of redacts":        `"Redacts": "$event"`,
		"case variant of unsigned":       `"UNSIGNED": {}`,
		"unicode case variant of sender": `"ſender": "@mallory:example.com"`,
		"event_id":                       `"event_id": "$claimed"`,
		"case variant of event_id":       `"Event_ID": "$claimed"`,
		"escaped event_id":               `"event\u005fid": "$claimed"`,
		"quote in name":                  `"a\"b": 1`,
		"backslash in name":              `"a\\b": 1`,
		"control character in name":      `"a\u0001b": 1`,
		"lone surrogate in name":         `"\ud800": 1`,
		"duplicate name":                 `"org.example": 1, "org.example": 2`,
		"duplicate name in value":        `"org.example": {"a": 1, "a": 2}`,
		"invalid UTF-8 in name":          "\"org.example\xff\": 1",
		"invalid UTF-8 in value":         "\"org.example\": \"\xff\"",
	} {
		pdu := `{"type": "m.room.message", "sender": "@alice:example.com", "content": {}, ` + members + `}`
		assert.Error(t, json.Unmarshal([]byte(pdu), &types.Event{}), name)
	}
}

func TestDomainlessCreateEventIDRefused(t *testing.T) {
	for _, eventID := range []string{"$claimed", "$" + strings.Repeat("A", 43)} {
		create := `{"type": "m.room.create", "state_key": "", "sender": "@alice:example.com",` +
			`"content": {"room_version": "12"}, "origin_server_ts": 1234, "depth": 1, "prev_events": [],` +
			`"auth_events": [], "hashes": {"sha256": "hash"}, "signatures": {}, "event_id": "` + eventID + `"}`
		assert.NotPanics(t, func() {
			assert.Error(t, json.Unmarshal([]byte(create), &types.Event{RoomVersion: "12"}), eventID)
		}, eventID)
	}
}

func TestLocalEventJSONUnchanged(t *testing.T) {
	stateKey := "@alice:example.com"
	ev := &types.Event{
		PartialEvent: types.PartialEvent{
			RoomID: "!room:example.com", Sender: "@alice:example.com", Type: event.StateMember, StateKey: &stateKey,
			Timestamp: 1700000000000, Content: json.RawMessage(`{"membership":"join","displayname":"<Alice>"}`),
			Unsigned: map[string]any{"age": 5},
		},
		RoomVersion:  "10",
		Depth:        4,
		PrevEventIDs: []id.EventID{"$prev"},
		AuthEventIDs: []id.EventID{"$create", "$power"},
	}
	require.NoError(t, util.HashAndSignEvent(ev, "example.com", "ed25519:test", ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))))
	assert.Equal(t, id.EventID("$046Hn3SC7u516oyGua3qMmTghJX3lDVZoHDFcnNN-js"), ev.ID)

	b, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.Equal(t, canonicalJSON(t, []byte(`{"room_id":"!room:example.com","sender":"@alice:example.com","state_key":"@alice:example.com","content":{"membership":"join","displayname":"\u003cAlice\u003e"},"origin_server_ts":1700000000000,"type":"m.room.member","depth":4,"prev_events":["$prev"],"auth_events":["$create","$power"],"hashes":{"sha256":"vkmaVQwNHGjKBnkfbQTPtcb9WifQvXeDWBLaIroNeW4"},"signatures":{"example.com":{"ed25519:test":"O+scEJQzjuhOR2edR8Lw1vFH9P1TAcjxTIaxagvop82iKklOSj5ARS4J96aXvQGaKJC2nADwvWAsk2iOYCiJBQ"}}}`)), canonicalJSON(t, b))

	var stored map[string]any
	require.NoError(t, msgpack.Unmarshal(ev.ToMsgpack(), &stored))
	assert.NotContains(t, stored, "ext")

	ev.IsForClientAPI = true
	ev.ClientTransactionID = "txn1"
	b, err = json.Marshal(ev)
	require.NoError(t, err)
	assert.Equal(t, canonicalJSON(t, []byte(`{"sender":"@alice:example.com","state_key":"@alice:example.com","content":{"membership":"join","displayname":"\u003cAlice\u003e"},"origin_server_ts":1700000000000,"type":"m.room.member","unsigned":{"age":5,"transaction_id":"txn1"},"event_id":"$046Hn3SC7u516oyGua3qMmTghJX3lDVZoHDFcnNN-js"}`)), canonicalJSON(t, b))
}
