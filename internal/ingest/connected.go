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
// gaps the room's job fills. Prev events are in place when stored, as are auth events. In a state
// DAG room (MSC4242) only the prev state events, with the implicit create event, must be in place,
// with state here or stored in another room, which the MSC's section 2 rejects citing: unknown prev
// events only leave the event a room extremity.
func (j *Jobs) SplitConnected(ctx context.Context, evs []*types.Event) (connected, rest []*types.Event, err error) {
	if len(evs) == 0 {
		return nil, nil, nil
	}
	evs = rooms.SortEventsTopologically(evs)
	storedIDs, judgedIDs := citedEventIDs(evs)
	stored, err := j.db.Rooms.GetStoredEventIDs(ctx, storedIDs)
	if err != nil {
		return nil, nil, err
	}
	judged, err := j.db.Rooms.GetPrevStateEventIDsInPlace(ctx, evs[0].RoomID, judgedIDs)
	if err != nil {
		return nil, nil, err
	}
	connected, rest = splitConnected(evs, stored, judged)
	return connected, rest, nil
}

// citedEventIDs returns, each once, the events the events cite that must be stored, and the prev
// state events of state DAG events
func citedEventIDs(evs []*types.Event) (stored, prevState []id.EventID) {
	for _, ev := range evs {
		if ev.StateDAGs() {
			prevState = append(prevState, ev.AuthDependencyIDs()...)
		} else {
			stored = append(stored, ev.PrevEventIDs...)
			stored = append(stored, ev.AuthDependencyIDs()...)
		}
	}
	slices.Sort(stored)
	slices.Sort(prevState)
	return slices.Compact(stored), slices.Compact(prevState)
}

// splitConnected splits events in topological order, keeping it in both parts
func splitConnected(evs []*types.Event, stored, prevStateInPlace map[id.EventID]struct{}) (connected, rest []*types.Event) {
	earlier := make(map[id.EventID]struct{}, len(evs))
	missingFrom := func(inPlace map[id.EventID]struct{}) func(id.EventID) bool {
		return func(eventID id.EventID) bool {
			_, found := inPlace[eventID]
			_, connected := earlier[eventID]
			return !found && !connected
		}
	}
	for _, ev := range evs {
		var gap bool
		if ev.StateDAGs() {
			gap = slices.ContainsFunc(ev.AuthDependencyIDs(), missingFrom(prevStateInPlace))
		} else {
			gap = slices.ContainsFunc(ev.PrevEventIDs, missingFrom(stored)) || slices.ContainsFunc(ev.AuthDependencyIDs(), missingFrom(stored))
		}
		if gap {
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
