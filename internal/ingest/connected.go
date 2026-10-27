package ingest

import (
	"context"
	"slices"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
)

// SplitConnected splits a room's events, in topological order, into those citing only events in
// place here or earlier connected events, which publish with nothing fetched, and the rest, whose
// gaps the room's job fills. Prev events are in place when stored, as are auth events, but in a state
// DAG room (MSC4242) prev state events, with the implicit create event, must have state here.
func (j *Jobs) SplitConnected(ctx context.Context, evs []*types.Event) (connected, rest []*types.Event, err error) {
	if len(evs) == 0 {
		return nil, nil, nil
	}
	evs = rooms.SortEventsTopologically(evs)
	storedIDs, withStateIDs := citedEventIDs(evs)
	stored, err := j.db.Rooms.GetStoredEventIDs(ctx, storedIDs)
	if err != nil {
		return nil, nil, err
	}
	withState, err := j.db.Rooms.GetEventIDsWithState(ctx, evs[0].RoomID, withStateIDs)
	if err != nil {
		return nil, nil, err
	}
	connected, rest = splitConnected(evs, stored, withState)
	return connected, rest, nil
}

// citedEventIDs returns, each once, the events the events cite that must be stored, and those that
// must have state
func citedEventIDs(evs []*types.Event) (stored, withState []id.EventID) {
	for _, ev := range evs {
		stored = append(stored, ev.PrevEventIDs...)
		if ev.StateDAGs() {
			withState = append(withState, ev.AuthDependencyIDs()...)
		} else {
			stored = append(stored, ev.AuthDependencyIDs()...)
		}
	}
	slices.Sort(stored)
	slices.Sort(withState)
	return slices.Compact(stored), slices.Compact(withState)
}

// splitConnected splits events in topological order, keeping it in both parts
func splitConnected(evs []*types.Event, stored, withState map[id.EventID]struct{}) (connected, rest []*types.Event) {
	earlier := make(map[id.EventID]struct{}, len(evs))
	missingFrom := func(inPlace map[id.EventID]struct{}) func(id.EventID) bool {
		return func(eventID id.EventID) bool {
			_, found := inPlace[eventID]
			_, connected := earlier[eventID]
			return !found && !connected
		}
	}
	for _, ev := range evs {
		authDependencies := stored
		if ev.StateDAGs() {
			authDependencies = withState
		}
		if slices.ContainsFunc(ev.PrevEventIDs, missingFrom(stored)) || slices.ContainsFunc(ev.AuthDependencyIDs(), missingFrom(authDependencies)) {
			rest = append(rest, ev)
			continue
		}
		earlier[ev.ID] = struct{}{}
		connected = append(connected, ev)
	}
	return connected, rest
}

// splitConnectedTargets orders PDU targets topologically, then splits them as SplitConnected does
func (j *Jobs) splitConnectedTargets(ctx context.Context, pdus []*types.IngestTarget) (connected, gaps []*types.IngestTarget, err error) {
	byID := make(map[id.EventID]*types.IngestTarget, len(pdus))
	evs := make([]*types.Event, len(pdus))
	for i, target := range pdus {
		byID[target.Event.ID] = target
		evs[i] = target.Event
	}
	connectedEvs, gapEvs, err := j.SplitConnected(ctx, evs)
	if err != nil {
		return nil, nil, err
	}
	return targetsOf(connectedEvs, byID), targetsOf(gapEvs, byID), nil
}

func targetsOf(evs []*types.Event, byID map[id.EventID]*types.IngestTarget) []*types.IngestTarget {
	targets := make([]*types.IngestTarget, len(evs))
	for i, ev := range evs {
		targets[i] = byID[ev.ID]
	}
	return targets
}
