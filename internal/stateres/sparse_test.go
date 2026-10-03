package stateres

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
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

// sparseInput derives what production reads from stored state and the auth graph, with the first
// state as the base.
func (r *testRoom) sparseInput(states []types.StateMap) SparseInput {
	_, conflicts := separate(states)
	chains := make([]map[id.EventID]struct{}, len(states))
	for i, state := range states {
		chains[i] = make(map[id.EventID]struct{})
		for _, eventID := range state {
			maps.Copy(chains[i], r.authChain(eventID))
		}
	}
	input := SparseInput{
		Conflicts:      conflicts,
		AuthDifference: authDifference(chains),
		Lookup: func(tups []types.StateTup) (types.StateMap, error) {
			values := make(types.StateMap, len(tups))
			for _, tup := range tups {
				values[tup] = states[0][tup]
			}
			return values, nil
		},
	}
	if util.RoomVersionHas(r.roomVersion, func(impl gomatrixserverlib.IRoomVersion) bool {
		return impl.StateResAlgorithm() == gomatrixserverlib.StateResV2_1
	}) {
		input.ConflictedSubgraph = r.conflictedSubgraph(conflicts)
	}
	return input
}

// conflictedSubgraph is every event that is both an auth ancestor and an auth descendant of
// conflicted events, counting the conflicted events themselves.
func (r *testRoom) conflictedSubgraph(conflicts map[types.StateTup][]id.EventID) []id.EventID {
	var conflicted []id.EventID
	for _, eventIDs := range conflicts {
		conflicted = append(conflicted, eventIDs...)
	}
	ancestors := make(map[id.EventID]struct{})
	for _, eventID := range conflicted {
		ancestors[eventID] = struct{}{}
		maps.Copy(ancestors, r.authChain(eventID))
	}
	var subgraph []id.EventID
	for eventID := range ancestors {
		chain := r.authChain(eventID)
		if slices.ContainsFunc(conflicted, func(conflictedID id.EventID) bool {
			_, descends := chain[conflictedID]
			return descends || conflictedID == eventID
		}) {
			subgraph = append(subgraph, eventID)
		}
	}
	return subgraph
}

// resolveWith resolves the states with ResolveSparse and returns the whole resolved state rather
// than the delta against the first.
func (r *testRoom) resolveWith(getEvent GetEventFunc, states ...types.StateMap) (types.StateMap, error) {
	delta, err := ResolveSparse(r.roomVersion, r.sparseInput(states), getEvent)
	if err != nil {
		return nil, err
	}
	return applyDelta(states[0], delta), nil
}

func (r *testRoom) resolve(states ...types.StateMap) types.StateMap {
	resolved, err := r.resolveWith(r.getEvent, states...)
	require.NoError(r.t, err)
	return resolved
}

func applyDelta(base, delta types.StateMap) types.StateMap {
	resolved := maps.Clone(base)
	for tup, eventID := range delta {
		if eventID == "" {
			delete(resolved, tup)
		} else {
			resolved[tup] = eventID
		}
	}
	return resolved
}

// separate returns the tuples that have the same event in every state, and for every other tuple
// the distinct events found for it. A tuple missing from any state is conflicted, even when every
// state that has it agrees.
func separate(states []types.StateMap) (types.StateMap, map[types.StateTup][]id.EventID) {
	unconflicted := make(types.StateMap)
	conflicted := make(map[types.StateTup][]id.EventID)
	for _, state := range states {
		for tup := range state {
			if _, done := unconflicted[tup]; done {
				continue
			}
			if _, done := conflicted[tup]; done {
				continue
			}
			var eventIDs []id.EventID
			missing := false
			for _, other := range states {
				eventID, ok := other[tup]
				if !ok {
					missing = true
				} else if !slices.Contains(eventIDs, eventID) {
					eventIDs = append(eventIDs, eventID)
				}
			}
			if !missing && len(eventIDs) == 1 {
				unconflicted[tup] = eventIDs[0]
			} else {
				conflicted[tup] = eventIDs
			}
		}
	}
	return unconflicted, conflicted
}

// authDifference is the union of the auth chains minus their intersection.
func authDifference(chains []map[id.EventID]struct{}) []id.EventID {
	counts := make(map[id.EventID]int)
	for _, chain := range chains {
		for eventID := range chain {
			counts[eventID]++
		}
	}
	var difference []id.EventID
	for eventID, count := range counts {
		if count < len(chains) {
			difference = append(difference, eventID)
		}
	}
	return difference
}

func TestSeparate(t *testing.T) {
	a := types.StateMap{createTup: "$create", memberTup(bob): "$bob", topicTup: "$t1", nameTup: "$name"}
	b := types.StateMap{createTup: "$create", memberTup(bob): "$bob", topicTup: "$t2"}
	c := types.StateMap{createTup: "$create", memberTup(bob): "$bob", topicTup: "$t1", memberTup(evelyn): "$evelyn"}

	unconflicted, conflicted := separate([]types.StateMap{a, b, c})
	assert.Equal(t, types.StateMap{createTup: "$create", memberTup(bob): "$bob"}, unconflicted)
	assert.Equal(t, map[types.StateTup][]id.EventID{
		topicTup:          {"$t1", "$t2"},
		nameTup:           {"$name"},
		memberTup(evelyn): {"$evelyn"},
	}, conflicted)
}

// v2.1 authorizes from an empty state, so the common $PL1 only builds the mainline if it is
// authorized again, as part of the conflicted subgraph between $MB2 and $MB3. That mainline puts
// $T1, sent under $PL0, before $T2, sent under $PL1, despite $T1's later timestamp.
func TestResolveV12ConflictedSubgraph(t *testing.T) {
	r := newTestRoom(t)
	r.roomVersion = "12"
	createID := id.EventID("$" + testRoomID[1:])
	r.add(createID, alice, event.StateCreate, new(""), `{"room_version":"12"}`)
	r.add("$MA", alice, event.StateMember, new(alice.String()), join, createID)
	r.add("$PL0", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{bob: 100}), "$MA")
	r.add("$JR", alice, event.StateJoinRules, new(""), joinRule("public"), "$PL0")
	r.add("$MB1", bob, event.StateMember, new(bob.String()), join, "$JR")
	r.add("$MB2", bob, event.StateMember, new(bob.String()), `{"membership":"join","displayname":"2"}`, "$MB1")
	r.add("$T1", alice, event.StateTopic, new(""), topic("T1"), "$MB2")
	r.add("$PL1", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{bob: 100, charlie: 50}), "$T1")
	r.add("$MB3", bob, event.StateMember, new(bob.String()), `{"membership":"join","displayname":"3"}`, "$PL1")
	r.add("$T2", alice, event.StateTopic, new(""), topic("T2"), "$MB3")
	r.events["$T1"].Timestamp = r.events["$T2"].Timestamp + 1

	states := []types.StateMap{r.stateAfter["$PL1"], r.stateAfter["$T2"]}
	input := r.sparseInput(states)
	assert.Equal(t, []id.EventID{"$PL1"}, input.AuthDifference)
	assert.ElementsMatch(t, []id.EventID{"$MB2", "$T1", "$PL1", "$MB3", "$T2"}, input.ConflictedSubgraph)

	resolved := r.resolve(states...)
	assert.Equal(t, id.EventID("$T2"), resolved[topicTup])
	assert.Equal(t, id.EventID("$MB3"), resolved[memberTup(bob)])
}

// MSC4297: the original join rule is in the auth chain of every input, so outside the auth
// difference, and lies between the conflicting power levels $PL0 and $PL2, through Bob's join.
// Alice's demotion of Dave sorts before Dave's join rule changes by her creator power and rejects
// both, so only authorizing the original join rule again, as part of the conflicted subgraph, leaves
// the room a join rule.
func TestResolveV12SubgraphBeyondTheAuthDifference(t *testing.T) {
	r := newTestRoom(t)
	r.roomVersion = "12"
	createID := id.EventID("$" + testRoomID[1:])
	r.add(createID, alice, event.StateCreate, new(""), `{"room_version":"12"}`)
	r.add("$MA", alice, event.StateMember, new(alice.String()), join, createID)
	r.add("$PL0", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{bob: 100, dave: 100}), "$MA")
	r.add("$JR", alice, event.StateJoinRules, new(""), joinRule("public"), "$PL0")
	r.add("$MB", bob, event.StateMember, new(bob.String()), join, "$JR")
	r.add("$MD", dave, event.StateMember, new(dave.String()), join, "$MB")
	r.add("$JR1", dave, event.StateJoinRules, new(""), joinRule("public"), "$MD")
	r.add("$JR2", dave, event.StateJoinRules, new(""), joinRule("invite"), "$MD")
	r.add("$PL1", alice, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{bob: 100, dave: 0}), "$JR2")
	r.add("$PL2", bob, event.StatePowerLevels, new(""), powerLevels(map[id.UserID]int{bob: 100, charlie: 50, dave: 0}), "$PL1")

	states := []types.StateMap{r.stateAfter["$JR1"], r.stateAfter["$PL2"]}
	input := r.sparseInput(states)
	assert.Equal(t, map[types.StateTup][]id.EventID{joinRulesTup: {"$JR1", "$JR2"}, powerLevelsTup: {"$PL0", "$PL2"}}, input.Conflicts)
	assert.NotContains(t, input.AuthDifference, id.EventID("$JR"))
	assert.Contains(t, input.ConflictedSubgraph, id.EventID("$JR"))

	resolved := r.resolve(states...)
	assert.Equal(t, id.EventID("$JR"), resolved[joinRulesTup])
	assert.Equal(t, id.EventID("$PL2"), resolved[powerLevelsTup])
}

// Base entries, an absence included, are read instead of looking the tuples up again.
func TestResolveSparseReadsBaseBeforeLookup(t *testing.T) {
	r := newSynapseRoom(t)
	r.add("$T", alice, event.StateTopic, new(""), topic("T"), "$START")
	input := r.sparseInput([]types.StateMap{r.stateAfter["$START"], r.stateAfter["$T"]})
	lookup := input.Lookup
	lookedUp := make(types.StateMap)
	input.Lookup = func(tups []types.StateTup) (types.StateMap, error) {
		values, err := lookup(tups)
		maps.Copy(lookedUp, values)
		return values, err
	}
	delta, err := ResolveSparse(testRoomVersion, input, r.getEvent)
	require.NoError(t, err)
	require.Equal(t, types.StateMap{topicTup: "$T"}, delta)
	require.Contains(t, lookedUp, topicTup)
	require.Empty(t, lookedUp[topicTup])

	input.Base = maps.Clone(lookedUp)
	input.Lookup = func([]types.StateTup) (types.StateMap, error) {
		return nil, errors.New("unexpected lookup")
	}
	delta, err = ResolveSparse(testRoomVersion, input, r.getEvent)
	require.NoError(t, err)
	assert.Equal(t, types.StateMap{topicTup: "$T"}, delta)
	assert.Equal(t, lookedUp, input.Base)
}

func TestSparseResolutionDoesNotLoadUnrelated100kMembers(t *testing.T) {
	r := newTestRoom(t)
	g := &forkGenerator{room: r, rng: rand.New(rand.NewPCG(1, 2))}
	baseID := g.base()
	left := r.add("$left", alice, event.StateTopic, new(""), topic("left"), baseID)
	right := r.add("$right", alice, event.StateTopic, new(""), topic("right"), baseID)
	base := maps.Clone(r.stateAfter[left.ID])
	for i := range 100000 {
		base[memberTup(id.UserID(fmt.Sprintf("@unrelated%d:example.com", i)))] = id.EventID(fmt.Sprintf("$unrelated%d", i))
	}
	loaded, lookups := 0, 0
	delta, err := ResolveSparse("11", SparseInput{Conflicts: map[types.StateTup][]id.EventID{topicTup: {left.ID, right.ID}}, Lookup: func(keys []types.StateTup) (types.StateMap, error) {
		out := make(types.StateMap)
		for _, tup := range keys {
			lookups++
			require.NotContains(t, tup.StateKey, "unrelated")
			out[tup] = base[tup]
		}
		return out, nil
	}}, func(eventID id.EventID) (*types.Event, error) {
		loaded++
		require.NotContains(t, eventID.String(), "unrelated")
		return r.getEvent(eventID)
	})
	require.NoError(t, err)
	require.Equal(t, types.StateMap{topicTup: right.ID}, delta)
	require.Less(t, loaded, 10)
	require.Less(t, lookups, 10)
}
