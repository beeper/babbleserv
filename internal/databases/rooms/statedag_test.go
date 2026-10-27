package rooms

import (
	"fmt"
	"slices"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func stateDAGTestEvent(eventID id.EventID, state bool, prevStateEventIDs ...id.EventID) *types.Event {
	ev := &types.Event{ID: eventID, PrevStateEventIDs: prevStateEventIDs}
	ev.RoomID, ev.Type = sendTestRoomID, event.EventMessage
	if state {
		ev.Type, ev.StateKey = event.StateTopic, new("")
	}
	return ev
}

func TestAdvanceStateExtremities(t *testing.T) {
	rejected := stateDAGTestEvent("$rejected", true, "$power")
	rejected.Rejected = true
	softFailed := stateDAGTestEvent("$soft", true, "$power")
	softFailed.SoftFailed = true

	for name, test := range map[string]struct {
		extremities []id.EventID
		ev          *types.Event
		expected    []id.EventID
	}{
		"create starts the state DAG":             {nil, stateDAGTestEvent("$create", true), []id.EventID{"$create"}},
		"state event replaces what it cites":      {[]id.EventID{"$power"}, stateDAGTestEvent("$topic", true, "$power"), []id.EventID{"$topic"}},
		"state event merges a fork":               {[]id.EventID{"$a", "$b"}, stateDAGTestEvent("$merge", true, "$a", "$b"), []id.EventID{"$merge"}},
		"state event citing some leaves the rest": {[]id.EventID{"$a", "$b", "$c"}, stateDAGTestEvent("$d", true, "$a", "$c"), []id.EventID{"$b", "$d"}},
		"message changes nothing":                 {[]id.EventID{"$power"}, stateDAGTestEvent("$message", false, "$power"), []id.EventID{"$power"}},
		"rejected event changes nothing":          {[]id.EventID{"$power"}, rejected, []id.EventID{"$power"}},
		"soft failed event changes nothing":       {[]id.EventID{"$power"}, softFailed, []id.EventID{"$power"}},
	} {
		assert.Equal(t, test.expected, advanceStateExtremities(test.extremities, test.ev), name)
	}
}

func TestStateExtremitiesAdvanceAlongABatch(t *testing.T) {
	batch := []*types.Event{
		stateDAGTestEvent("$create", true),
		stateDAGTestEvent("$join", true, "$create"),
		stateDAGTestEvent("$message", false, "$join"),
		stateDAGTestEvent("$power", true, "$join"),
		stateDAGTestEvent("$reply", false, "$power"),
	}
	var extremities []id.EventID
	for _, ev := range batch {
		assert.Equal(t, extremities, ev.PrevStateEventIDs, ev.ID)
		extremities = advanceStateExtremities(extremities, ev)
	}
	assert.Equal(t, []id.EventID{"$power"}, extremities)
}

func TestCitedExtremitiesAreThoseStoredLast(t *testing.T) {
	var extremities []id.EventID
	for i := range types.MaxStateDAGPrevEvents + 2 {
		extremities = append(extremities, id.EventID(fmt.Sprintf("$%02d", i)))
	}
	assert.Equal(t, extremities[:3], citedExtremities(extremities[:3]))
	cited := citedExtremities(extremities)
	assert.Equal(t, extremities[2:], cited)
	cited[0] = "$changed"
	assert.Equal(t, id.EventID("$02"), extremities[2], "the extremities are left as they are")
	assert.Equal(t, []id.EventID{}, citedExtremities(nil), "the create cites an empty list")
}

func TestCitedPrevStateEventsAddTheSupersededMembership(t *testing.T) {
	var full []id.EventID
	for i := range types.MaxStateDAGPrevEvents + 2 {
		full = append(full, id.EventID(fmt.Sprintf("$%02d", i)))
	}
	for name, test := range map[string]struct {
		extremities []id.EventID
		superseded  id.EventID
		cited       []id.EventID
		current     bool
	}{
		"no membership cites every extremity":              {[]id.EventID{"$a", "$b"}, "", []id.EventID{"$a", "$b"}, true},
		"over the cap with no membership cites some":       {full, "", full[2:], false},
		"a membership that is an extremity adds nothing":   {[]id.EventID{"$a", "$invite"}, "$invite", []id.EventID{"$a", "$invite"}, true},
		"a membership behind the extremities is added":     {[]id.EventID{"$topic"}, "$invite", []id.EventID{"$topic", "$invite"}, false},
		"at the cap the extremity stored first makes room": {full[2:], "$invite", append(slices.Clone(full[3:]), "$invite"), false},
		"over the cap the extremity stored first makes room": {
			full, "$invite", append(slices.Clone(full[3:]), "$invite"), false,
		},
		"an extremity left out over the cap is added in place of the first": {
			full, full[0], append(slices.Clone(full[3:]), full[0]), false,
		},
	} {
		cited, current := citedPrevStateEvents(test.extremities, test.superseded)
		assert.Equal(t, test.cited, cited, name)
		assert.Equal(t, test.current, current, name)
		assert.LessOrEqual(t, len(cited), types.MaxStateDAGPrevEvents, name)
	}
}

func TestChangesRemoteMembership(t *testing.T) {
	member := func(sender, target id.UserID) *types.Event {
		ev := &types.Event{}
		ev.Type, ev.Sender, ev.StateKey = event.StateMember, sender, new(target.String())
		return ev
	}
	assert.True(t, changesRemoteMembership(member("@alice:here", "@dave:there"), "here"), "a kick or invite of another server's user")
	assert.False(t, changesRemoteMembership(member("@dave:there", "@dave:there"), "here"), "a make_* template is the target's own")
	assert.False(t, changesRemoteMembership(member("@alice:here", "@bob:here"), "here"), "a local user's membership")
	assert.False(t, changesRemoteMembership(stateDAGTestEvent("$topic", true), "here"))
}

func TestOnlyInvitesAndKnocksAreRescinded(t *testing.T) {
	for membership, rescinded := range map[event.Membership]bool{
		event.MembershipInvite: true,
		event.MembershipKnock:  true,
		event.MembershipJoin:   false,
		event.MembershipBan:    false,
		event.MembershipLeave:  false,
		"":                     false,
	} {
		held := types.StateEntry{EventID: "$held", Membership: membership}
		assert.Equal(t, rescinded, rescindedMembership(held) == "$held", membership)
	}
}

func TestOnlyMessagesSoftFailInStateDAGRooms(t *testing.T) {
	message, state := stateDAGTestEvent("$message", false), stateDAGTestEvent("$topic", true)
	for _, ev := range []*types.Event{message, state} {
		ev.RoomVersion = string(gomatrixserverlib.RoomVersionStateDAGs)
	}
	assert.True(t, softFails(message))
	assert.False(t, softFails(state), "a state event always joins the state DAG")

	message.RoomVersion, state.RoomVersion = "12", "12"
	assert.True(t, softFails(message))
	assert.True(t, softFails(state), "every event soft fails in other room versions")
}

func stateDAG(dag map[id.EventID][]id.EventID) map[id.EventID]*types.Event {
	evs := make(map[id.EventID]*types.Event, len(dag))
	for eventID, prevStateEventIDs := range dag {
		ev := stateDAGTestEvent(eventID, true, prevStateEventIDs...)
		ev.BeforeState, ev.AfterState = types.StateHash{1}, types.StateHash{2}
		evs[eventID] = ev
	}
	return evs
}

func walk(t *testing.T, dag map[id.EventID]*types.Event, earliest, latest []id.EventID, limit int) []id.EventID {
	t.Helper()
	var reads [][]id.EventID
	evs, err := walkStateDAG(sendTestRoomID, earliest, latest, limit, func(eventIDs []id.EventID) (map[id.EventID]*types.Event, error) {
		reads = append(reads, eventIDs)
		found := make(map[id.EventID]*types.Event)
		for _, eventID := range eventIDs {
			if ev, ok := dag[eventID]; ok {
				found[eventID] = ev
			}
		}
		return found, nil
	})
	require.NoError(t, err)
	for _, read := range reads {
		assert.True(t, slices.IsSorted(read), "each hop is read in the order it is returned")
	}
	return util.EventsToIDs(evs)
}

// MSC4242's examples: events are ordered by the fewest hops from the latest events, then by ID
func TestWalkStateDAGOrdersByHopsThenEventID(t *testing.T) {
	forks := stateDAG(map[id.EventID][]id.EventID{
		"$A": nil, "$B": {"$A"}, "$C": {"$A"}, "$D": {"$B"}, "$E": {"$C"},
	})
	assert.Equal(t, []id.EventID{"$B", "$C", "$A"}, walk(t, forks, nil, []id.EventID{"$D", "$E"}, 10))
	assert.Equal(t, []id.EventID{"$B", "$C", "$A"}, walk(t, forks, nil, []id.EventID{"$E", "$D"}, 10))
	assert.Equal(t, []id.EventID{"$B"}, walk(t, forks, nil, []id.EventID{"$E", "$D"}, 1), "[B, C] not [C, B], cut at the limit")

	merged := stateDAG(map[id.EventID][]id.EventID{
		"$A": nil, "$B": {"$A"}, "$C": {"$A"}, "$D": {"$C", "$B"}, "$E": {"$C"},
	})
	assert.Equal(t, []id.EventID{"$B", "$C", "$A"}, walk(t, merged, nil, []id.EventID{"$D", "$E"}, 10))
}

func TestWalkStateDAG(t *testing.T) {
	dag := stateDAG(map[id.EventID][]id.EventID{
		"$create": nil, "$join": {"$create"}, "$a": {"$join"}, "$b": {"$a"}, "$z": {"$join"}, "$rejected": {"$join"},
		"$tip": {"$b", "$rejected"}, "$other": {"$z"},
	})
	dag["$rejected"].Rejected = true
	message := stateDAGTestEvent("$message", false, "$tip", "$z")
	message.BeforeState, message.AfterState = types.StateHash{1}, types.StateHash{1}
	dag["$message"] = message
	dag["$other"].RoomID = "!other:example.com"
	dag["$staged"] = stateDAGTestEvent("$staged", true, "$create")

	assert.Equal(t, []id.EventID{"$tip", "$z", "$b", "$a"}, walk(t, dag, []id.EventID{"$join"}, []id.EventID{"$message"}, 10),
		"a message's prev state events, then theirs, back to the earliest events, through no rejected event")
	assert.Equal(t, []id.EventID{"$join", "$create"}, walk(t, dag, nil, []id.EventID{"$b", "$a"}, 10),
		"the latest events are not returned, even reached from another")
	assert.Empty(t, walk(t, dag, nil, []id.EventID{"$unknown", "$other", "$staged"}, 10),
		"nothing is walked from events unknown, of another room or without state")
	assert.Equal(t, []id.EventID{"$a", "$join"}, walk(t, dag, []id.EventID{"$create"}, []id.EventID{"$b", "$z"}, 2))
}
