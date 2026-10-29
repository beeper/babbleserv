package ingest

import (
	"context"
	"slices"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
)

// SplitConnected splits off the events citing only events in place here or connected before them,
// which publish with nothing fetched. In a state DAG room (MSC4242) a prev state event is in place
// once published here, or stored in another room, which section 2 rejects citing, and a pulled
// event's prev events need not be.
func (j *Jobs) SplitConnected(ctx context.Context, evs []*types.Event, pulled map[id.EventID]struct{}) (connected, rest []*types.Event, err error) {
	if len(evs) == 0 {
		return nil, nil, nil
	}
	evs = rooms.SortEventsTopologically(evs)
	storedIDs, judgedIDs := citedEventIDs(evs, pulled)
	stored, err := j.db.Rooms.GetStoredEventIDs(ctx, storedIDs)
	if err != nil {
		return nil, nil, err
	}
	judged, err := j.db.Rooms.GetPrevStateEventIDsInPlace(ctx, evs[0].RoomID, judgedIDs)
	if err != nil {
		return nil, nil, err
	}
	connected, rest = splitConnected(evs, stored, judged, pulled)
	return connected, rest, nil
}

func citedEventIDs(evs []*types.Event, pulled map[id.EventID]struct{}) (stored, prevState []id.EventID) {
	for _, ev := range evs {
		switch {
		case !ev.StateDAGs():
			stored = append(stored, ev.PrevEventIDs...)
			stored = append(stored, ev.AuthDependencyIDs()...)
		case isPulled(ev, pulled):
			prevState = append(prevState, ev.AuthDependencyIDs()...)
		default:
			stored = append(stored, ev.PrevEventIDs...)
			prevState = append(prevState, ev.AuthDependencyIDs()...)
		}
	}
	slices.Sort(stored)
	slices.Sort(prevState)
	return slices.Compact(stored), slices.Compact(prevState)
}

func isPulled(ev *types.Event, pulled map[id.EventID]struct{}) bool {
	_, found := pulled[ev.ID]
	return found
}

func splitConnected(evs []*types.Event, stored, prevStateInPlace, pulled map[id.EventID]struct{}) (connected, rest []*types.Event) {
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
			gap = slices.ContainsFunc(ev.AuthDependencyIDs(), missingFrom(prevStateInPlace)) ||
				!isPulled(ev, pulled) && slices.ContainsFunc(ev.PrevEventIDs, missingFrom(stored))
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

func (j *Jobs) splitConnectedTargets(ctx context.Context, pdus []*types.IngestTarget) (connected, gaps []*types.IngestTarget, err error) {
	byID := make(map[id.EventID]*types.IngestTarget, len(pdus))
	evs := make([]*types.Event, len(pdus))
	pulled := make(map[id.EventID]struct{})
	for i, target := range pdus {
		byID[target.Event.ID] = target
		evs[i] = target.Event
		if target.Pulled {
			pulled[target.Event.ID] = struct{}{}
		}
	}
	connectedEvs, gapEvs, err := j.SplitConnected(ctx, evs, pulled)
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
