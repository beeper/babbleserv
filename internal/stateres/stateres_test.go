package stateres

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
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

const testRoomID id.RoomID = "!room:example.com"

const testRoomVersion = "11"

const (
	alice   id.UserID = "@alice:example.com"
	bob     id.UserID = "@bob:example.com"
	charlie id.UserID = "@charlie:example.com"
	dave    id.UserID = "@dave:example.com"
	evelyn  id.UserID = "@evelyn:example.com"
	zara    id.UserID = "@zara:example.com"
)

const (
	join    = `{"membership":"join"}`
	leave   = `{"membership":"leave"}`
	ban     = `{"membership":"ban"}`
	message = `{"msgtype":"m.text","body":"hi"}`
)

var (
	topicTup     = types.StateTup{Type: event.StateTopic}
	nameTup      = types.StateTup{Type: event.StateRoomName}
	joinRulesTup = types.StateTup{Type: event.StateJoinRules}
)

var memberTup = types.MemberStateTup

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func powerLevels(users map[id.UserID]int, extra ...map[string]any) string {
	content := map[string]any{"users": users}
	for _, fields := range extra {
		maps.Copy(content, fields)
	}
	return mustJSON(content)
}

func joinRule(rule string) string {
	return mustJSON(map[string]string{"join_rule": rule})
}

func topic(text string) string {
	return mustJSON(map[string]string{"topic": text})
}

// testRoom builds a room DAG in memory. Each event's state before is its prev event's state after,
// resolved when there are several, and its auth events are picked from that state the way the
// auth events selection algorithm does.
type testRoom struct {
	t           *testing.T
	roomVersion string
	events      map[id.EventID]*types.Event
	stateAfter  map[id.EventID]types.StateMap
	authChains  map[id.EventID]map[id.EventID]struct{}
	timestamp   int64
}

func newTestRoom(t *testing.T) *testRoom {
	return &testRoom{
		t:           t,
		roomVersion: testRoomVersion,
		events:      make(map[id.EventID]*types.Event),
		stateAfter:  make(map[id.EventID]types.StateMap),
		authChains:  make(map[id.EventID]map[id.EventID]struct{}),
	}
}

func (r *testRoom) getEvent(eventID id.EventID) (*types.Event, error) {
	return r.events[eventID], nil
}

// authChain is the event's auth chain, without the event itself. An unknown event has none.
func (r *testRoom) authChain(eventID id.EventID) map[id.EventID]struct{} {
	if chain, ok := r.authChains[eventID]; ok {
		return chain
	}
	chain := make(map[id.EventID]struct{})
	if ev := r.events[eventID]; ev != nil {
		for _, authEventID := range ev.AuthEventIDs {
			chain[authEventID] = struct{}{}
			maps.Copy(chain, r.authChain(authEventID))
		}
	}
	r.authChains[eventID] = chain
	return chain
}

func (r *testRoom) stateMap(eventIDs []id.EventID) types.StateMap {
	state := make(types.StateMap, len(eventIDs))
	for _, eventID := range eventIDs {
		state[r.events[eventID].StateTup()] = eventID
	}
	return state
}

func (r *testRoom) stateBefore(prevs []id.EventID) types.StateMap {
	switch len(prevs) {
	case 0:
		return types.StateMap{}
	case 1:
		return maps.Clone(r.stateAfter[prevs[0]])
	}
	states := make([]types.StateMap, len(prevs))
	for i, prev := range prevs {
		states[i] = r.stateAfter[prev]
	}
	return r.resolve(states...)
}

func (r *testRoom) newEvent(
	eventID id.EventID,
	sender id.UserID,
	evType event.Type,
	stateKey *string,
	content string,
	prevs ...id.EventID,
) (*types.Event, types.StateMap) {
	var depth int64
	for _, prev := range prevs {
		depth = max(depth, r.events[prev].Depth)
	}
	r.timestamp++
	ev := &types.Event{
		PartialEvent: types.PartialEvent{
			RoomID:    testRoomID,
			Sender:    sender,
			Type:      evType,
			StateKey:  stateKey,
			Content:   json.RawMessage(content),
			Timestamp: r.timestamp,
		},
		ID:           eventID,
		RoomVersion:  r.roomVersion,
		Depth:        depth + 1,
		PrevEventIDs: prevs,
	}
	before := r.stateBefore(prevs)
	for _, tup := range gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.PDU{ev.PDU()}).Tuples() {
		stateTup := types.StateTup{Type: event.NewEventType(tup.EventType), StateKey: tup.StateKey}
		if stateTup == createTup && util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) {
			continue
		}
		if authEventID, ok := before[stateTup]; ok {
			ev.AuthEventIDs = append(ev.AuthEventIDs, authEventID)
		}
	}
	return ev, before
}

func (r *testRoom) store(ev *types.Event, before types.StateMap) {
	require.NotContains(r.t, r.events, ev.ID)
	after := before
	if ev.StateKey != nil {
		after[ev.StateTup()] = ev.ID
	}
	r.events[ev.ID] = ev
	r.stateAfter[ev.ID] = after
}

func (r *testRoom) add(
	eventID id.EventID,
	sender id.UserID,
	evType event.Type,
	stateKey *string,
	content string,
	prevs ...id.EventID,
) *types.Event {
	ev, before := r.newEvent(eventID, sender, evType, stateKey, content, prevs...)
	r.store(ev, before)
	return ev
}

func (r *testRoom) allowed(ev *types.Event, before types.StateMap) bool {
	resolver := newResolver(r.roomVersion, r.getEvent)
	resolver.partial = before
	authErr, err := resolver.authorize(ev)
	require.NoError(r.t, err)
	return authErr == nil
}

// requireChangedState asserts that the state at eventID, ignoring tuples that still have their
// value from $START, is exactly the expected events.
func (r *testRoom) requireChangedState(eventID id.EventID, expected ...id.EventID) {
	r.t.Helper()
	expectedState := make(types.StateMap, len(expected))
	for _, expectedID := range expected {
		expectedState[r.events[expectedID].StateTup()] = expectedID
	}
	start := r.stateAfter["$START"]
	changed := make(types.StateMap)
	for tup, value := range r.stateAfter[eventID] {
		if _, ok := expectedState[tup]; ok || start[tup] != value {
			changed[tup] = value
		}
	}
	require.Equal(r.t, expectedState, changed)
}

// newSynapseRoom builds the starting graph of Synapse's state resolution v2 tests
// (tests/state/test_v2.py), which are the examples the algorithm was designed against. Events
// in cases built on it are added in the order Synapse's harness creates them, so the
// origin_server_ts tiebreaks match.
func newSynapseRoom(t *testing.T) *testRoom {
	r := newTestRoom(t)
	r.add("$CREATE", alice, event.StateCreate, new(""), `{"room_version":"11"}`)
	r.add("$IMA", alice, event.StateMember, new(alice.String()), join, "$CREATE")
	r.add("$IPOWER", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100}), "$IMA")
	r.add("$IJR", alice, event.StateJoinRules, new(""), joinRule("public"), "$IPOWER")
	r.add("$IMB", bob, event.StateMember, new(bob.String()), join, "$IJR")
	r.add("$IMC", charlie, event.StateMember, new(charlie.String()), join, "$IMB")
	r.add("$IMZ", zara, event.StateMember, new(zara.String()), join, "$IMC")
	r.add("$START", zara, event.EventMessage, nil, message, "$IMZ")
	return r
}

func TestResolveTrivialInputs(t *testing.T) {
	delta, err := ResolveSparse(testRoomVersion, SparseInput{}, func(id.EventID) (*types.Event, error) {
		return nil, errors.New("unexpected event lookup")
	})
	require.NoError(t, err)
	assert.NotNil(t, delta)
	assert.Empty(t, delta)

	r := newSynapseRoom(t)
	state := r.stateAfter["$START"]
	assert.Equal(t, state, r.resolve(state))
}

func TestResolveUnsupportedRoomVersions(t *testing.T) {
	r := newSynapseRoom(t)
	state := r.stateAfter["$START"]
	input := r.sparseInput([]types.StateMap{state, state})

	_, err := ResolveSparse("1", input, r.getEvent)
	require.ErrorIs(t, err, ErrUnsupportedAlgorithm)

	_, err = ResolveSparse("not-a-version", input, r.getEvent)
	var unsupported gomatrixserverlib.UnsupportedRoomVersionError
	require.ErrorAs(t, err, &unsupported)
}

func TestResolveSynapseVectors(t *testing.T) {
	// Power events are ordered by sender power level first: the ban is authorized before Bob's
	// power levels event, which then fails because Bob is banned.
	t.Run("ban_vs_pl", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$PA", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$START")
		r.add("$MA", alice, event.StateMember, new(alice.String()), join, "$PA")
		r.add("$MB", alice, event.StateMember, new(bob.String()), ban, "$MA")
		r.add("$PB", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$PA")
		r.add("$END", zara, event.EventMessage, nil, message, "$MB", "$PB")
		r.requireChangedState("$END", "$PA", "$MA", "$MB")
	})

	// $PB is in neither state, only in the auth difference, and must be authorized before $PC
	// for Charlie to hold the power to send it.
	t.Run("offtopic_pl", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$PA", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$START")
		r.add("$PB", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50, charlie: 50}), "$PA")
		r.add("$PC", charlie, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50, charlie: 0}), "$PB")
		r.add("$END", zara, event.EventMessage, nil, message, "$PC", "$PA")
		r.requireChangedState("$END", "$PC")
	})

	// Two branches set different power levels. Alice's $PA2 outranks Bob's $PB and demotes Bob,
	// so Bob's topic from the losing branch is authorized against $PA2 and rejected.
	t.Run("topic_basic", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$START")
		r.add("$PA1", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$T1")
		r.add("$PB", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$PA1")
		r.add("$T2", alice, event.StateTopic, new(""), topic("T2"), "$PA1")
		r.add("$PA2", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 0}), "$T2")
		r.add("$T3", bob, event.StateTopic, new(""), topic("T3"), "$PB")
		r.add("$END", zara, event.EventMessage, nil, message, "$PA2", "$T3")
		r.requireChangedState("$END", "$PA2", "$T2")
	})

	// The ban is a power event applied before the topics, so Bob's $T2 fails and the topic
	// resets to $T1.
	t.Run("topic_reset", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$START")
		r.add("$PA", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$T1")
		r.add("$T2", bob, event.StateTopic, new(""), topic("T2"), "$PA")
		r.add("$MB", alice, event.StateMember, new(bob.String()), ban, "$T2")
		r.add("$END", zara, event.EventMessage, nil, message, "$MB", "$T1")
		r.requireChangedState("$END", "$T1", "$MB", "$PA")
	})

	t.Run("topic", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$START")
		r.add("$PA1", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$T1")
		r.add("$PB", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$PA1")
		r.add("$T2", alice, event.StateTopic, new(""), topic("T2"), "$PA1")
		r.add("$PA2", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 0}), "$T2")
		r.add("$T3", bob, event.StateTopic, new(""), topic("T3"), "$PB")
		r.add("$MZ1", zara, event.EventMessage, nil, message, "$PA2", "$T3")
		r.add("$T4", alice, event.StateTopic, new(""), topic("T4"), "$MZ1")
		r.add("$END", zara, event.EventMessage, nil, message, "$T4", "$MZ1")
		r.requireChangedState("$END", "$T4", "$PA2")
	})

	// $T3 is based on a later mainline power levels event than $T4, so it is applied last and
	// wins despite its earlier origin_server_ts.
	t.Run("mainline_sort", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$START")
		r.add("$PA1", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$T1")
		r.add("$PB", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$PA1")
		r.add("$T2", alice, event.StateTopic, new(""), topic("T2"), "$PA1")
		r.add("$PA2", alice, event.StatePowerLevels, new(""), powerLevels(
			map[id.UserID]int{alice: 100, bob: 50},
			map[string]any{"events": map[string]int{event.StatePowerLevels.Type: 100}},
		), "$T2")
		r.add("$T3", bob, event.StateTopic, new(""), topic("T3"), "$PA2")
		r.add("$T4", alice, event.StateTopic, new(""), topic("T4"), "$PB")
		r.add("$END", zara, event.EventMessage, nil, message, "$T3", "$T4")
		r.requireChangedState("$END", "$T3", "$PA2")
	})
}

// A tuple present in one input and absent from another is conflicted, which gomatrixserverlib's
// flattened ResolveConflicts misses: it keeps Evelyn's join in both cases.
func TestResolveAbsenceConflict(t *testing.T) {
	// Synapse's test_join_rule_evasion.
	t.Run("join rule change rejects a join from the other branch", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$JR", alice, event.StateJoinRules, new(""), joinRule("private"), "$START")
		r.add("$ME", evelyn, event.StateMember, new(evelyn.String()), join, "$START")
		r.add("$END", zara, event.EventMessage, nil, message, "$JR", "$ME")
		r.requireChangedState("$END", "$JR")
	})

	t.Run("join from one branch is kept when nothing forbids it", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T", alice, event.StateTopic, new(""), topic("T"), "$START")
		r.add("$ME", evelyn, event.StateMember, new(evelyn.String()), join, "$START")
		r.add("$END", zara, event.EventMessage, nil, message, "$T", "$ME")
		r.requireChangedState("$END", "$T", "$ME")
	})
}

func TestResolveMembershipConflict(t *testing.T) {
	t.Run("ban beats a later join", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$BAN", alice, event.StateMember, new(bob.String()), ban, "$START")
		r.add("$RENAME", bob, event.StateMember, new(bob.String()), `{"membership":"join","displayname":"Bob"}`, "$START")
		r.add("$END", zara, event.EventMessage, nil, message, "$BAN", "$RENAME")
		r.requireChangedState("$END", "$BAN")
	})

	// The kick is a power event and is applied first, then Bob's join from the other branch is
	// authorized as a rejoin, which only the public room allows.
	for _, tc := range []struct {
		rule     string
		expected id.EventID
	}{
		{"public", "$RENAME"},
		{"invite", "$KICK"},
	} {
		t.Run("kick against a later join in a "+tc.rule+" room", func(t *testing.T) {
			r := newSynapseRoom(t)
			r.add("$JR", alice, event.StateJoinRules, new(""), joinRule(tc.rule), "$START")
			r.add("$KICK", alice, event.StateMember, new(bob.String()), leave, "$JR")
			r.add("$RENAME", bob, event.StateMember, new(bob.String()), `{"membership":"join","displayname":"Bob"}`, "$JR")
			r.add("$END", zara, event.EventMessage, nil, message, "$KICK", "$RENAME")
			r.requireChangedState("$END", "$JR", tc.expected)
		})
	}

	for _, later := range []id.EventID{"$LEAVE", "$RENAME"} {
		t.Run("later of a leave and a join wins: "+later.String(), func(t *testing.T) {
			r := newSynapseRoom(t)
			addLeave := func() {
				r.add("$LEAVE", bob, event.StateMember, new(bob.String()), leave, "$START")
			}
			addRename := func() {
				r.add("$RENAME", bob, event.StateMember, new(bob.String()), `{"membership":"join","displayname":"Bob"}`, "$START")
			}
			if later == "$LEAVE" {
				addRename()
				addLeave()
			} else {
				addLeave()
				addRename()
			}
			r.add("$END", zara, event.EventMessage, nil, message, "$LEAVE", "$RENAME")
			r.requireChangedState("$END", later)
		})
	}
}

func TestResolveAuthDifferenceControlEvent(t *testing.T) {
	r := newSynapseRoom(t)
	r.add("$PA1", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$START")
	r.add("$PA2", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100, bob: 50, charlie: 50}), "$PA1")
	r.add("$MSG", zara, event.EventMessage, nil, message, "$START")
	input := r.sparseInput([]types.StateMap{r.stateAfter["$MSG"], r.stateAfter["$PA2"]})
	assert.Equal(t, map[types.StateTup][]id.EventID{powerLevelsTup: {"$IPOWER", "$PA2"}}, input.Conflicts)

	// $PA1 is in neither state, only in $PA2's auth chain. Bob's join is there too, but it is
	// common state and stays the base of the resolution.
	assert.ElementsMatch(t, []id.EventID{"$PA1", "$IMB"}, input.AuthDifference)

	// Bob can only send $PA2 under $PA1, so $PA2 wins only if $PA1 was authorized before it.
	delta, err := ResolveSparse(testRoomVersion, input, r.getEvent)
	require.NoError(t, err)
	assert.Equal(t, types.StateMap{powerLevelsTup: "$PA2"}, delta)

	input.AuthDifference = []id.EventID{"$IMB"}
	delta, err = ResolveSparse(testRoomVersion, input, r.getEvent)
	require.NoError(t, err)
	assert.Empty(t, delta)
}

// $T1 was sent before the room had power levels, so it never reaches the mainline of the resolved
// $PL and sorts before $T2, though its timestamp is later: $T2 is applied last.
func TestResolveMainlineEventWithoutPowerLevelsAncestor(t *testing.T) {
	r := newTestRoom(t)
	r.add("$CREATE", alice, event.StateCreate, new(""), `{"room_version":"11"}`)
	r.add("$IMA", alice, event.StateMember, new(alice.String()), join, "$CREATE")
	r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$IMA")
	r.add("$PL", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{alice: 100}), "$IMA")
	r.add("$T2", alice, event.StateTopic, new(""), topic("T2"), "$PL")
	r.events["$T1"].Timestamp = r.events["$T2"].Timestamp + 1
	assert.NotContains(t, r.authChain("$T1"), id.EventID("$PL"))

	resolved := r.resolve(r.stateAfter["$T1"], r.stateAfter["$T2"])
	assert.Equal(t, id.EventID("$T2"), resolved[topicTup])
	assert.Equal(t, id.EventID("$PL"), resolved[powerLevelsTup])
}

// Evelyn's join is only known through her topic's auth events. The join rule change rejects the
// join itself, so the topic is authorized with the declared join as the fallback for Evelyn's
// membership, which is allowed for an outlier and not for a rejected event.
func TestResolveDeclaredAuthFallback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		markJoin      func(*types.Event)
		expectedTopic id.EventID
	}{
		{"outlier auth event is used", func(ev *types.Event) { ev.Outlier = true }, "$TE"},
		{"rejected auth event is not used", func(ev *types.Event) { ev.Rejected = true }, "$T0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSynapseRoom(t)
			r.add("$PTOPIC", alice, event.StatePowerLevels, new(""), powerLevels(
				map[id.UserID]int{alice: 100},
				map[string]any{"events": map[string]int{event.StateTopic.Type: 0}},
			), "$START")
			r.add("$T0", alice, event.StateTopic, new(""), topic("T0"), "$PTOPIC")
			r.add("$JE", evelyn, event.StateMember, new(evelyn.String()), join, "$T0")
			r.add("$TE", evelyn, event.StateTopic, new(""), topic("TE"), "$JE")
			r.add("$JR", alice, event.StateJoinRules, new(""), joinRule("invite"), "$T0")
			tc.markJoin(r.events["$JE"])

			withoutJoin := maps.Clone(r.stateAfter["$TE"])
			delete(withoutJoin, memberTup(evelyn))
			resolved := r.resolve(withoutJoin, r.stateAfter["$JR"])

			assert.Equal(t, tc.expectedTopic, resolved[topicTup])
			assert.Equal(t, id.EventID("$JR"), resolved[joinRulesTup])
			assert.NotContains(t, resolved, memberTup(evelyn))
		})
	}
}

// Bob's leave is rejected, so authorizing his topic falls back to the join the topic cites.
func TestResolveRejectedStateFallsBackToDeclaredAuthEvent(t *testing.T) {
	r := newSynapseRoom(t)
	r.add("$PTOPIC", alice, event.StatePowerLevels, new(""), powerLevels(
		map[id.UserID]int{alice: 100},
		map[string]any{"events": map[string]int{event.StateTopic.Type: 0}},
	), "$START")
	r.add("$LEAVE", bob, event.StateMember, new(bob.String()), leave, "$PTOPIC")
	r.add("$T0", alice, event.StateTopic, new(""), topic("T0"), "$LEAVE")
	r.add("$TB", bob, event.StateTopic, new(""), topic("TB"), "$PTOPIC")
	r.events["$LEAVE"].Rejected = true

	withLeave := maps.Clone(r.stateAfter["$TB"])
	withLeave[memberTup(bob)] = "$LEAVE"
	resolved := r.resolve(withLeave, r.stateAfter["$T0"])
	assert.Equal(t, id.EventID("$TB"), resolved[topicTup])
}

func TestSenderPowerLevelCreator(t *testing.T) {
	for _, tc := range []struct {
		roomVersion string
		creator     id.UserID
	}{
		{"10", bob},
		{"11", alice},
	} {
		t.Run("room version "+tc.roomVersion, func(t *testing.T) {
			create := &types.Event{
				PartialEvent: types.PartialEvent{
					RoomID: testRoomID, Sender: alice, Type: event.StateCreate, StateKey: new(""),
					Content: json.RawMessage(mustJSON(map[string]string{"creator": bob.String(), "room_version": tc.roomVersion})),
				},
				ID:          "$create",
				RoomVersion: tc.roomVersion,
			}
			events := map[id.EventID]*types.Event{create.ID: create}
			resolver := newResolver(tc.roomVersion, func(eventID id.EventID) (*types.Event, error) {
				return events[eventID], nil
			})
			for _, sender := range []id.UserID{alice, bob} {
				member := &types.Event{
					PartialEvent: types.PartialEvent{RoomID: testRoomID, Sender: sender, Type: event.StateMember, StateKey: new(sender.String()), Content: json.RawMessage(join)},
					ID:           id.EventID("$join-" + sender.Localpart()),
					RoomVersion:  tc.roomVersion,
					AuthEventIDs: []id.EventID{create.ID},
				}
				level, err := resolver.senderPowerLevel(member)
				require.NoError(t, err)
				if sender == tc.creator {
					assert.EqualValues(t, creatorPowerLevel, level, sender)
				} else {
					assert.Zero(t, level, sender)
				}
			}
		})
	}
}

func TestResolveInconsistentInputs(t *testing.T) {
	r := newSynapseRoom(t)
	r.add("$T", alice, event.StateTopic, new(""), topic("T"), "$START")
	r.add("$MSG", zara, event.EventMessage, nil, message, "$START")
	withTopic, withoutTopic := r.stateAfter["$T"], r.stateAfter["$MSG"]

	t.Run("missing auth difference event is skipped", func(t *testing.T) {
		input := r.sparseInput([]types.StateMap{withoutTopic, withTopic})
		input.AuthDifference = append(input.AuthDifference, "$unknown")
		delta, err := ResolveSparse(testRoomVersion, input, r.getEvent)
		require.NoError(t, err)
		assert.Equal(t, types.StateMap{topicTup: "$T"}, delta)
	})

	t.Run("missing conflicted state event is an error", func(t *testing.T) {
		unknownTopic := maps.Clone(withoutTopic)
		unknownTopic[topicTup] = "$unknown"
		_, err := r.resolveWith(r.getEvent, withTopic, unknownTopic)
		require.ErrorContains(t, err, "$unknown")
	})

	t.Run("conflicted state event listed under another tuple is an error", func(t *testing.T) {
		misfiled := maps.Clone(withoutTopic)
		misfiled[nameTup] = "$T"
		_, err := r.resolveWith(r.getEvent, withTopic, misfiled)
		require.ErrorContains(t, err, "$T")
	})

	t.Run("event from another room version is an error", func(t *testing.T) {
		r := newSynapseRoom(t)
		r.add("$T", alice, event.StateTopic, new(""), topic("T"), "$START")
		r.events["$T"].RoomVersion = "10"
		_, err := r.resolveWith(r.getEvent, r.stateAfter["$T"], r.stateAfter["$START"])
		require.ErrorContains(t, err, "room version")
	})

	t.Run("event lookup errors are returned", func(t *testing.T) {
		failure := errors.New("database unavailable")
		getEvent := func(eventID id.EventID) (*types.Event, error) {
			if eventID == "$IPOWER" {
				return nil, failure
			}
			return r.getEvent(eventID)
		}
		_, err := r.resolveWith(getEvent, withTopic, withoutTopic)
		require.ErrorIs(t, err, failure)
	})
}

var generatedUsers = []id.UserID{bob, charlie, dave}

type eventKind int

const (
	powerLevelsChange eventKind = iota
	joinRulesChange
	moderation
	topicChange
	nameChange
	ownMembership
)

type proposedEvent struct {
	sender   id.UserID
	evType   event.Type
	stateKey *string
	content  string
}

// forkGenerator builds random forks to compare against gomatrixserverlib. The pinned
// gomatrixserverlib departs from the spec in ways the Synapse vectors expose: it orders power
// events with lower sender power levels first, tiebreaks the mainline order on the number of
// steps taken to reach the mainline, and leaves everything but conflicted events out of the auth
// difference. The forks stay clear of those cases: only the room creator sends power events, only
// the first branch changes power levels, each branch changes a tuple at most once, and the shared
// history never changes the join rules, whose events do not cite their predecessors. Every tuple
// exists before the fork, so no input lacks a tuple another has.
type forkGenerator struct {
	rng    *rand.Rand
	room   *testRoom
	nextID int
}

func (g *forkGenerator) pick(values ...int) int {
	return values[g.rng.IntN(len(values))]
}

func (g *forkGenerator) user() id.UserID {
	return generatedUsers[g.rng.IntN(len(generatedUsers))]
}

func (g *forkGenerator) anyone() id.UserID {
	if g.rng.IntN(len(generatedUsers)+1) == 0 {
		return alice
	}
	return g.user()
}

func (g *forkGenerator) powerLevels() string {
	users := map[id.UserID]int{alice: 100}
	for _, userID := range generatedUsers {
		users[userID] = g.pick(0, 25, 50, 75)
	}
	return powerLevels(users, map[string]any{
		"state_default": g.pick(0, 50),
		"kick":          g.pick(25, 50, 75),
		"ban":           g.pick(25, 50, 75),
		"events": map[string]int{
			event.StateTopic.Type:       g.pick(0, 25, 50, 75),
			event.StateRoomName.Type:    g.pick(0, 25, 50, 75),
			event.StatePowerLevels.Type: g.pick(50, 75, 100),
			event.StateJoinRules.Type:   g.pick(50, 75, 100),
		},
	})
}

func (g *forkGenerator) base() id.EventID {
	r := g.room
	r.add("$create", alice, event.StateCreate, new(""), `{"room_version":"11"}`)
	r.add("$alice", alice, event.StateMember, new(alice.String()), join, "$create")
	r.add("$pl", alice, event.StatePowerLevels, new(""), g.powerLevels(), "$alice")
	extremityID := r.add("$jr", alice, event.StateJoinRules, new(""), joinRule("public"), "$pl").ID
	for _, userID := range generatedUsers {
		extremityID = r.add(id.EventID("$join-"+userID.Localpart()), userID, event.StateMember, new(userID.String()), join, extremityID).ID
	}
	extremityID = r.add("$topic", alice, event.StateTopic, new(""), topic("base"), extremityID).ID
	return r.add("$name", alice, event.StateRoomName, new(""), `{"name":"base"}`, extremityID).ID
}

func (g *forkGenerator) propose(state types.StateMap, kinds []eventKind) proposedEvent {
	switch kinds[g.rng.IntN(len(kinds))] {
	case powerLevelsChange:
		return proposedEvent{alice, event.StatePowerLevels, new(""), g.powerLevels()}
	case joinRulesChange:
		rule := []string{"public", "invite"}[g.rng.IntN(2)]
		return proposedEvent{alice, event.StateJoinRules, new(""), joinRule(rule)}
	case moderation:
		membership := []string{leave, ban}[g.rng.IntN(2)]
		return proposedEvent{alice, event.StateMember, new(g.user().String()), membership}
	case topicChange:
		return proposedEvent{g.anyone(), event.StateTopic, new(""), topic(fmt.Sprint(g.rng.Int()))}
	case nameChange:
		return proposedEvent{g.anyone(), event.StateRoomName, new(""), mustJSON(map[string]int{"name": g.rng.Int()})}
	default:
		userID := g.user()
		if g.room.events[state[memberTup(userID)]].Membership() == event.MembershipJoin && g.rng.IntN(2) == 0 {
			return proposedEvent{userID, event.StateMember, new(userID.String()), leave}
		}
		content := mustJSON(map[string]any{"membership": "join", "displayname": fmt.Sprint(g.rng.Int())})
		return proposedEvent{userID, event.StateMember, new(userID.String()), content}
	}
}

func (g *forkGenerator) tryAdd(extremityID id.EventID, proposed proposedEvent) (*types.Event, bool) {
	g.nextID++
	ev, before := g.room.newEvent(
		id.EventID(fmt.Sprintf("$e%d", g.nextID)),
		proposed.sender, proposed.evType, proposed.stateKey, proposed.content, extremityID,
	)
	if !g.room.allowed(ev, before) {
		return nil, false
	}
	g.room.store(ev, before)
	return ev, true
}

func (g *forkGenerator) history(extremityID id.EventID, steps int) id.EventID {
	kinds := []eventKind{powerLevelsChange, moderation, topicChange, nameChange, ownMembership}
	for range steps {
		if ev, ok := g.tryAdd(extremityID, g.propose(g.room.stateAfter[extremityID], kinds)); ok {
			extremityID = ev.ID
		}
	}
	return extremityID
}

// fork grows the branches in turn, so their origin_server_ts values interleave.
func (g *forkGenerator) fork(from id.EventID, branches, steps int) []id.EventID {
	extremities := make([]id.EventID, branches)
	changed := make([]map[types.StateTup]struct{}, branches)
	for i := range extremities {
		extremities[i] = from
		changed[i] = make(map[types.StateTup]struct{})
	}
	for range steps {
		for i, extremityID := range extremities {
			kinds := []eventKind{joinRulesChange, moderation, topicChange, nameChange, ownMembership}
			if i == 0 {
				kinds = append(kinds, powerLevelsChange)
			}
			proposed := g.propose(g.room.stateAfter[extremityID], kinds)
			tup := types.StateTup{Type: proposed.evType, StateKey: *proposed.stateKey}
			if _, ok := changed[i][tup]; ok {
				continue
			}
			if ev, ok := g.tryAdd(extremityID, proposed); ok {
				changed[i][tup] = struct{}{}
				extremities[i] = ev.ID
			}
		}
	}
	return extremities
}

// testdata/materialized_forks.json holds what Synapse 1.161.0 resolves for each of the 1,000
// generated forks. Their origin_server_ts values are distinct, so Synapse's own event IDs break no
// ties.
func TestResolveFrozenForks(t *testing.T) {
	t.Skip("generated inputs depend on Synapse auth rules deferred until after state storage")

	raw, err := os.ReadFile("testdata/materialized_forks.json")
	require.NoError(t, err)
	var frozen map[string][][]id.EventID
	require.NoError(t, json.Unmarshal(raw, &frozen))
	for _, variant := range []struct {
		name         string
		branches     int
		historySteps int
	}{
		{"two branches", 2, 0},
		{"three branches", 3, 0},
		{"two branches after shared history", 2, 8},
		{"three branches after shared history", 3, 8},
	} {
		t.Run(variant.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(42, uint64(variant.branches*100+variant.historySteps)))
			for iteration := range 250 {
				g := &forkGenerator{rng: rng, room: newTestRoom(t)}
				extremities := g.fork(g.history(g.base(), variant.historySteps), variant.branches, 2+rng.IntN(5))
				states := make([]types.StateMap, len(extremities))
				for i, extremityID := range extremities {
					states[i] = g.room.stateAfter[extremityID]
				}
				_, conflicted := separate(states)
				for tup := range conflicted {
					for _, state := range states {
						require.Contains(t, state, tup)
					}
				}

				resolved := g.room.resolve(states...)
				require.Equal(t, frozen[variant.name][iteration], slices.Sorted(maps.Values(resolved)))
			}
		})
	}
}

func TestResolveStoredRejection(t *testing.T) {
	r := newSynapseRoom(t)
	topicEv := r.add("$T", alice, event.StateTopic, new(""), topic("T"), "$START")
	r.add("$MSG", zara, event.EventMessage, nil, message, "$START")
	states := []types.StateMap{r.stateAfter["$T"], r.stateAfter["$MSG"]}

	// The topic passes auth, but its stored rejection must prevent selection.
	require.Equal(t, topicEv.ID, r.resolve(states...)[topicTup])
	topicEv.Rejected = true

	for range 2 {
		assert.NotContains(t, r.resolve(states...), topicTup)
		assert.True(t, topicEv.Rejected)
	}
}

func TestResolveRejectedPowerCannotAuthorizeLaterCandidate(t *testing.T) {
	for _, source := range []string{"conflicted state", "auth difference"} {
		t.Run(source, func(t *testing.T) {
			r := newSynapseRoom(t)
			power := r.add("$P", alice, event.StatePowerLevels, new(""),
				powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$START")
			var candidate *types.Event
			if source == "conflicted state" {
				candidate = r.add("$T", bob, event.StateTopic, new(""), topic("T"), "$P")
			} else {
				candidate = r.add("$P2", bob, event.StatePowerLevels, new(""), powerLevels(
					map[id.UserID]int{alice: 100, bob: 50, charlie: 25},
				), "$P")
			}
			r.add("$MSG", zara, event.EventMessage, nil, message, "$START")
			states := []types.StateMap{r.stateAfter[candidate.ID], r.stateAfter["$MSG"]}

			require.Equal(t, candidate.ID, r.resolve(states...)[candidate.StateTup()])
			power.Rejected = true

			resolved := r.resolve(states...)
			assert.Equal(t, id.EventID("$IPOWER"), resolved[powerLevelsTup])
			// Bob has no power under $IPOWER, so neither his topic nor $P2 can pass auth.
			assert.NotContains(t, resolved, topicTup)
			assert.True(t, power.Rejected)
		})
	}
}

func TestResolveSoftFailedPowerStillParticipates(t *testing.T) {
	r := newSynapseRoom(t)
	power := r.add("$P", alice, event.StatePowerLevels, new(""),
		powerLevels(map[id.UserID]int{alice: 100, bob: 50}), "$START")
	topicEv := r.add("$T", bob, event.StateTopic, new(""), topic("T"), "$P")
	r.add("$MSG", zara, event.EventMessage, nil, message, "$START")
	power.SoftFailed = true

	resolved := r.resolve(r.stateAfter[topicEv.ID], r.stateAfter["$MSG"])
	assert.Equal(t, power.ID, resolved[powerLevelsTup])
	assert.Equal(t, topicEv.ID, resolved[topicTup])
	assert.True(t, power.SoftFailed)
	assert.False(t, power.Rejected)
}
