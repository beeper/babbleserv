package types_test

import (
	"encoding/json"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

func TestMembershipRowRoundTrips(t *testing.T) {
	for _, row := range []types.MembershipRow{
		{MembershipTup: types.MembershipTup{EventID: "$join", RoomID: "!room", Membership: event.MembershipJoin}},
		{MembershipTup: types.MembershipTup{EventID: "$invite", RoomID: "!room", Membership: event.MembershipInvite}, Outlier: true},
	} {
		assert.Equal(t, row, types.BytesToMembershipRow(types.MembershipRowToBytes(row)))
		assert.Equal(t, row.MembershipTup, types.BytesToMembershipTup(types.MembershipRowToBytes(row)))
	}
}
