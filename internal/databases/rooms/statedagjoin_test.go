package rooms

import (
	"maps"
	"slices"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// stagedStateDAG builds stateless state events citing the events they map to in prev_state_events
func stagedStateDAG(dag map[id.EventID][]id.EventID) map[id.EventID]*types.Event {
	evs := make(map[id.EventID]*types.Event, len(dag))
	for eventID, prevStateEventIDs := range dag {
		ev := stateDAGTestEvent(eventID, true, prevStateEventIDs...)
		ev.RoomID, ev.RoomVersion = "!create", string(gomatrixserverlib.RoomVersionStateDAGs)
		if eventID == "$create" {
			ev.Type = event.StateCreate
		}
		evs[eventID] = ev
	}
	return evs
}

func walkStaged(t *testing.T, dag map[id.EventID]*types.Event, from ...id.EventID) *stateDAGWalk {
	t.Helper()
	return walkPending(t, dag, from, nil)
}

func walkPending(t *testing.T, dag map[id.EventID]*types.Event, from, pending []id.EventID) *stateDAGWalk {
	t.Helper()
	walk, err := walkUnreplayed("!create", from, pending, true, func(eventIDs []id.EventID) (map[id.EventID]*types.Event, error) {
		assert.LessOrEqual(t, len(eventIDs), stateDAGWalkChunk)
		found := make(map[id.EventID]*types.Event)
		for _, eventID := range eventIDs {
			found[eventID] = dag[eventID]
		}
		return found, nil
	})
	require.NoError(t, err)
	return walk
}

// The walk follows prev_state_events, the implicit create among them, and records what is missing
func TestWalkUnreplayed(t *testing.T) {
	dag := stagedStateDAG(map[id.EventID][]id.EventID{
		"$create": nil, "$power": {"$create"}, "$a": {"$power"}, "$b": {"$power"},
		"$merge": {"$a", "$b"}, "$citesGap": {"$merge", "$withheld"}, "$replayed": {"$power"},
		"$other": nil, "$outlier": {"$power"}, "$citesOther": {"$other", "$outlier"},
	})
	dag["$replayed"].BeforeState, dag["$replayed"].AfterState = types.StateHash{1}, types.StateHash{2}
	dag["$other"].RoomID = "!other"
	dag["$outlier"].Outlier = true

	connected := walkStaged(t, dag, "$create", "$merge", "$replayed")
	assert.Empty(t, connected.missing)
	assert.ElementsMatch(t, []id.EventID{"$create", "$power", "$a", "$b", "$merge"}, slices.Collect(maps.Keys(connected.cites)),
		"an event with state here ends the walk")
	assert.Equal(t, []id.EventID{"$create", "$a", "$b"}, connected.cites["$merge"], "an event cites the create event implicitly")
	assert.Empty(t, connected.cites["$create"])

	gap := walkStaged(t, dag, "$citesGap", "$citesOther")
	assert.ElementsMatch(t, []id.EventID{"$withheld", "$outlier"}, slices.Collect(maps.Keys(gap.missing)),
		"an event not stored, or stored only as an outlier, is missing")
	assert.NotContains(t, gap.cites, id.EventID("$other"), "another room's event ends the walk")
}

// Pending events, which the job publishes rather than replays, end the walk
func TestWalkUnreplayedThroughUnpublished(t *testing.T) {
	dag := stagedStateDAG(map[id.EventID][]id.EventID{
		"$create": nil, "$power": {"$create"}, "$replayed": {"$power"}, "$staged": {"$replayed"},
		"$citesTarget": {"$target"}, "$tip": {"$staged", "$citesTarget"},
	})
	for _, published := range []id.EventID{"$create", "$power"} {
		dag[published].BeforeState, dag[published].AfterState = types.StateHash{1}, types.StateHash{2}
	}
	dag["$replayed"].BeforeState, dag["$replayed"].AfterState = types.StateHash{2}, types.StateHash{3}
	dag["$replayed"].Unpublished = true

	walk := walkPending(t, dag, []id.EventID{"$tip"}, []id.EventID{"$target"})
	assert.ElementsMatch(t, []id.EventID{"$tip", "$staged", "$replayed", "$citesTarget"}, slices.Collect(maps.Keys(walk.cites)),
		"a published event ends the walk")
	assert.Equal(t, map[id.EventID]struct{}{"$replayed": {}}, walk.unpublished)
	assert.Equal(t, map[id.EventID]struct{}{"$target": {}}, walk.pending)
	assert.Empty(t, walk.missing, "a pending event is not missing")
	assert.Same(t, dag["$staged"], walk.evs["$staged"])

	order, unreplayable := replayOrder(walk)
	assert.Equal(t, []id.EventID{"$replayed", "$staged"}, order, "an unpublished event orders with the rest, its replay skips it")
	assert.Equal(t, 2, unreplayable, "events reaching a pending one wait for it")
}

func TestReplayOrder(t *testing.T) {
	walk := &stateDAGWalk{
		cites: map[id.EventID][]id.EventID{
			"$create": nil, "$power": {"$create"}, "$b": {"$create", "$power"}, "$a": {"$create", "$power", "$power"},
			"$merge": {"$create", "$a", "$b"}, "$tip": {"$create", "$merge", "$replayed"},
			"$citesMissing": {"$create", "$power", "$withheld"}, "$citesThat": {"$create", "$citesMissing"},
			"$cycleA": {"$cycleB"}, "$cycleB": {"$cycleA"},
		},
		missing: map[id.EventID]struct{}{"$withheld": {}},
	}
	order, unreplayable := replayOrder(walk)
	assert.Equal(t, []id.EventID{"$create", "$power", "$a", "$b", "$merge", "$tip"}, order,
		"each after what it cites, the lowest event ID first of those ready")
	assert.Equal(t, 4, unreplayable, "events reaching a missing one, and a cycle, are left out")

	order, unreplayable = replayOrder(&stateDAGWalk{})
	assert.Empty(t, order)
	assert.Zero(t, unreplayable)
}
