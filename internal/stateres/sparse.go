package stateres

import (
	"fmt"
	"maps"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// SparseInput holds only the differing tuples and graph candidates. Lookup
// reads tuples from the first input, including an empty ID for an absent tuple.
// Base holds tuples of the first input already read, the same way, so they are
// not looked up again. Conflicts retains absences, so a tuple present in only
// some inputs conflicts.
type SparseInput struct {
	Conflicts          map[types.StateTup][]id.EventID
	AuthDifference     []id.EventID
	ConflictedSubgraph []id.EventID
	Lookup             func([]types.StateTup) (types.StateMap, error)
	Base               types.StateMap
}

// ResolveSparse returns a delta against the first input. Common state is read
// on demand and is never enumerated or copied into the result. Room versions
// before state resolution v2 return ErrUnsupportedAlgorithm. Inconsistent inputs
// are errors: a conflicted event that getEvent cannot find or that is not the
// event for its tuple, and any event of the full conflicted set from another
// room version. An auth difference or subgraph event that getEvent cannot find
// is skipped. ResolveSparse never changes the events supplied by getEvent.
func ResolveSparse(roomVersion string, input SparseInput, getEvent GetEventFunc) (types.StateMap, error) {
	if err := CheckAlgorithm(roomVersion); err != nil {
		return nil, err
	}
	if len(input.Conflicts) == 0 {
		return types.StateMap{}, nil
	}
	base := maps.Clone(input.Base)
	if base == nil {
		base = make(types.StateMap)
	}
	lookup := func(tup types.StateTup) (id.EventID, error) {
		if eventID, ok := base[tup]; ok {
			return eventID, nil
		}
		values, err := input.Lookup([]types.StateTup{tup})
		if err != nil {
			return "", err
		}
		base[tup] = values[tup] // Cache absence too.
		return base[tup], nil
	}
	common := func(tup types.StateTup) (id.EventID, error) {
		if _, conflicted := input.Conflicts[tup]; conflicted {
			return "", nil
		}
		return lookup(tup)
	}
	r := newResolver(roomVersion, getEvent)
	r.partial = make(types.StateMap)
	// v2.1 starts iterative authorization from an empty state.
	if !util.RoomVersionHas(roomVersion, func(impl gomatrixserverlib.IRoomVersion) bool {
		return impl.StateResAlgorithm() == gomatrixserverlib.StateResV2_1
	}) {
		r.common = common
	}
	full := make(map[id.EventID]*types.Event)
	for tup, ids := range input.Conflicts {
		for _, eventID := range ids {
			if eventID == "" {
				continue
			}
			ev, err := r.events.get(eventID)
			if err != nil {
				return nil, err
			}
			if ev == nil || ev.StateKey == nil || ev.StateTup() != tup {
				return nil, fmt.Errorf("missing or mismatched conflicted event %s for %v", eventID, tup)
			}
			if err := r.checkRoomVersion(ev); err != nil {
				return nil, err
			}
			full[eventID] = ev
		}
	}
	inSubgraph := make(map[id.EventID]bool, len(input.ConflictedSubgraph))
	for _, eventID := range input.ConflictedSubgraph {
		inSubgraph[eventID] = true
	}
	for _, eventID := range slices.Concat(input.AuthDifference, input.ConflictedSubgraph) {
		if _, loaded := full[eventID]; loaded {
			continue
		}
		ev, err := r.events.get(eventID)
		if err != nil {
			return nil, err
		}
		if ev == nil || ev.StateKey == nil {
			continue
		}
		if err := r.checkRoomVersion(ev); err != nil {
			return nil, err
		}
		// The strict auth difference excludes exact common state events,
		// matching the inclusive-root convention used by Synapse.
		// v2.1 must include common events lying in the conflicted subgraph.
		if !inSubgraph[eventID] {
			commonID, err := common(ev.StateTup())
			if err != nil {
				return nil, err
			}
			if commonID == eventID {
				continue
			}
		}
		full[eventID] = ev
	}
	if err := r.run(full); err != nil {
		return nil, err
	}
	// Only conflicts and tuples touched by candidates can differ. Common state
	// wins at the end, even if a candidate temporarily replaced it for auth.
	for tup := range input.Conflicts {
		if _, touched := r.partial[tup]; !touched {
			r.partial[tup] = ""
		}
	}
	delta := make(types.StateMap)
	for tup, resolvedID := range r.partial {
		baseID, err := lookup(tup)
		if err != nil {
			return nil, err
		}
		if _, conflicted := input.Conflicts[tup]; !conflicted && baseID != "" {
			resolvedID = baseID
		}
		if baseID != resolvedID {
			delta[tup] = resolvedID
		}
	}
	return delta, nil
}
