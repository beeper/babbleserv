package types_test

import (
	"encoding/json"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Synapse's test_redact_m_room_member (rust/src/events/utils.rs), with unsigned and non-object cases added
func TestRedactionKeepsTheSignedThirdPartyInviteFromVersion11(t *testing.T) {
	member := func(thirdPartyInvite string) string {
		return `{"type": "m.room.member", "content": {"unknown_key": "unknown_value", "membership": "join",
			"join_authorised_via_users_server": "server", "third_party_invite": ` + thirdPartyInvite + `}}`
	}
	for name, test := range map[string]struct {
		versions         []gomatrixserverlib.RoomVersion
		thirdPartyInvite string
		content          string
	}{
		"dropped before version 11": {
			[]gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV9, gomatrixserverlib.RoomVersionV10},
			`{"signed": {}}`,
			`{"membership": "join", "join_authorised_via_users_server": "server"}`,
		},
		"signed kept from version 11": {
			[]gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV11, gomatrixserverlib.RoomVersionV12, gomatrixserverlib.RoomVersionStateDAGs},
			`{"signed": {}, "display_name": "alice"}`,
			`{"membership": "join", "join_authorised_via_users_server": "server", "third_party_invite": {"signed": {}}}`,
		},
		"kept empty without signed": {
			[]gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV11},
			`{"display_name": "alice"}`,
			`{"membership": "join", "join_authorised_via_users_server": "server", "third_party_invite": {}}`,
		},
		"dropped when not an object": {
			[]gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV11},
			`"alice"`,
			`{"membership": "join", "join_authorised_via_users_server": "server"}`,
		},
	} {
		for _, version := range test.versions {
			redacted, err := gomatrixserverlib.MustGetRoomVersion(version).RedactEventJSON([]byte(member(test.thirdPartyInvite)))
			require.NoError(t, err, name)
			assert.JSONEq(t, test.content, gjson.GetBytes(redacted, "content").Raw, "%s: %s", name, version)
		}
	}

	redacted, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV11).RedactEventJSON(
		[]byte(`{"type": "m.room.topic", "content": {"topic": "x", "third_party_invite": {"signed": {}}}}`),
	)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, gjson.GetBytes(redacted, "content").Raw, "only member events keep it")
}

// The IDs Synapse 1.156 computes for this event (make_event_from_dict)
func TestThirdPartyInviteJoinEventIDsMatchSynapse(t *testing.T) {
	join := `{"type": "m.room.member", "room_id": "!room:example.com", "sender": "@alice:example.com",
		"state_key": "@alice:example.com", "content": {"membership": "join", "displayname": "Alice",
		"third_party_invite": {"display_name": "alice", "signed": {"mxid": "@alice:example.com", "token": "abc123",
		"signatures": {"id.example.com": {"ed25519:0": "c2lnbmF0dXJl"}}}}}, "depth": 5, "prev_events": ["$prev"],
		"auth_events": ["$create", "$power", "$invite"], "origin_server_ts": 1700000000000,
		"hashes": {"sha256": "aGFzaA"}, "signatures": {"example.com": {"ed25519:1": "c2ln"}}}`
	for version, eventID := range map[string]id.EventID{
		"10": "$ix_2igC0B06rw2LdxPAjk7lVQkPcZBfUygwctBzyKSw",
		"11": "$pT2QQFvYp8uLms8Oxukx0fbJjWVtCebU19m60iAsq5I",
		"12": "$pT2QQFvYp8uLms8Oxukx0fbJjWVtCebU19m60iAsq5I",
	} {
		ev := &types.Event{RoomVersion: version}
		require.NoError(t, json.Unmarshal([]byte(join), ev), version)
		refHash, err := util.GetEventReferenceHash(ev)
		require.NoError(t, err, version)
		assert.Equal(t, eventID, refHash, version)
	}
}

func TestRedactedEventIDMovedToContentInVersion11(t *testing.T) {
	redaction := func(version string) *types.Event {
		ev := &types.Event{RoomVersion: version}
		ev.Type, ev.Redacts, ev.Content = event.EventRedaction, "$top", json.RawMessage(`{"redacts": "$content"}`)
		return ev
	}
	assert.Equal(t, id.EventID("$top"), redaction("10").RedactedEventID())
	assert.Equal(t, id.EventID("$content"), redaction("11").RedactedEventID())
	assert.Equal(t, id.EventID("$content"), redaction(string(gomatrixserverlib.RoomVersionStateDAGs)).RedactedEventID())

	message := redaction("11")
	message.Type = event.EventMessage
	assert.Empty(t, message.RedactedEventID())
}
