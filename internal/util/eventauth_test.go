package util_test

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const stateDAGsVersion = string(gomatrixserverlib.RoomVersionStateDAGs)

func stateDAGEvent(eventID id.EventID, stateKey *string, prevStateEventIDs ...id.EventID) *types.Event {
	return &types.Event{
		PartialEvent:      types.PartialEvent{RoomID: "!room", Sender: "@alice:example.com", Type: event.StateTopic, StateKey: stateKey, Content: json.RawMessage(`{}`)},
		ID:                eventID,
		RoomVersion:       stateDAGsVersion,
		PrevStateEventIDs: prevStateEventIDs,
	}
}

func TestStateDAGCreateWithPrevStateEventsRejected(t *testing.T) {
	empty := ""
	create := &types.Event{
		PartialEvent: types.PartialEvent{Type: event.StateCreate, StateKey: &empty, Sender: "@alice:example.com", Content: json.RawMessage(`{"room_version":"org.matrix.msc4242.12"}`)},
		RoomVersion:  stateDAGsVersion,
		PrevEventIDs: []id.EventID{},
	}
	require.NoError(t, util.HashAndSignEvent(create, "example.com", "ed25519:test", ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))))
	create.RoomID = create.DomainlessRoomID()
	noState := func(types.StateTup) (*types.Event, error) { return nil, nil }
	authErr, err := util.Authorize(create, noState)
	require.NoError(t, err)
	require.NoError(t, authErr)

	create.PrevStateEventIDs = []id.EventID{"$other"}
	authErr, err = util.Authorize(create, noState)
	require.NoError(t, err)
	assert.ErrorContains(t, authErr, "prev_state_events")

	create.RoomVersion = "12"
	authErr, err = util.Authorize(create, noState)
	require.NoError(t, err)
	assert.NoError(t, authErr, "only state DAG rooms have the rule")
}

func TestAuthorizePrevStateEvents(t *testing.T) {
	stateKey := ""
	stored := map[id.EventID]*types.Event{
		"$create":  stateDAGEvent("$create", &stateKey),
		"$topic":   stateDAGEvent("$topic", &stateKey, "$create"),
		"$message": stateDAGEvent("$message", nil, "$create"),
		"$other":   stateDAGEvent("$other", &stateKey, "$create"),
		"$rejected": func() *types.Event {
			ev := stateDAGEvent("$rejected", &stateKey, "$create")
			ev.Rejected = true
			return ev
		}(),
	}
	stored["$other"].RoomID = "!other"
	lookup := func(eventID id.EventID) (*types.Event, error) { return stored[eventID], nil }

	for name, test := range map[string]struct {
		cites    []id.EventID
		rejected string
	}{
		"state events of the room": {cites: []id.EventID{"$create", "$topic"}},
		"none, as the create":      {},
		"an event of another room": {cites: []id.EventID{"$topic", "$other"}, rejected: "another room"},
		"a message":                {cites: []id.EventID{"$message"}, rejected: "not a state event"},
		"a rejected state event":   {cites: []id.EventID{"$topic", "$rejected"}, rejected: "rejected"},
	} {
		authErr, err := util.AuthorizePrevStateEvents(stateDAGEvent("$ev", nil, test.cites...), lookup)
		require.NoError(t, err, name)
		if test.rejected == "" {
			assert.NoError(t, authErr, name)
		} else {
			assert.ErrorContains(t, authErr, test.rejected, name)
		}
	}

	_, err := util.AuthorizePrevStateEvents(stateDAGEvent("$ev", nil, "$unknown"), lookup)
	assert.ErrorIs(t, err, util.ErrPrevStateEventMissing)

	lookupErr := errors.New("read failed")
	_, err = util.AuthorizePrevStateEvents(stateDAGEvent("$ev", nil, "$topic"), func(id.EventID) (*types.Event, error) {
		return nil, lookupErr
	})
	assert.ErrorIs(t, err, lookupErr)

	v12 := stateDAGEvent("$ev", nil, "$message")
	v12.RoomVersion = "12"
	authErr, err := util.AuthorizePrevStateEvents(v12, lookup)
	assert.NoError(t, err)
	assert.NoError(t, authErr, "other room versions have no prev state events to check")
}

// Rejecting an event citing a rejected one rejects every state event built on it in turn, while a
// branch citing only accepted events is unaffected
func TestPrevStateEventRejectionCascades(t *testing.T) {
	stateKey := ""
	chain := []*types.Event{
		stateDAGEvent("$create", &stateKey),
		stateDAGEvent("$bad", &stateKey, "$create"),
		stateDAGEvent("$child", &stateKey, "$bad"),
		stateDAGEvent("$sibling", &stateKey, "$create"),
		stateDAGEvent("$grandchild", &stateKey, "$child", "$sibling"),
		stateDAGEvent("$message", nil, "$sibling"),
	}
	chain[1].Rejected = true
	evaluated := make(map[id.EventID]*types.Event)
	lookup := func(eventID id.EventID) (*types.Event, error) { return evaluated[eventID], nil }
	for _, ev := range chain {
		if !ev.Rejected {
			authErr, err := util.AuthorizePrevStateEvents(ev, lookup)
			require.NoError(t, err, ev.ID)
			ev.Rejected = authErr != nil
		}
		evaluated[ev.ID] = ev
	}
	rejected := map[id.EventID]bool{}
	for _, ev := range chain {
		rejected[ev.ID] = ev.Rejected
	}
	assert.Equal(t, map[id.EventID]bool{
		"$create": false, "$bad": true, "$child": true, "$sibling": false, "$grandchild": true, "$message": false,
	}, rejected)
}
