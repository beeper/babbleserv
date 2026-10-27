package ingest

import (
	"context"
	"maps"
	"slices"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
)

// SplitConnected splits a room's events, in topological order, into those whose prev and auth events
// are all stored here or earlier connected events, which publish with nothing fetched, and the rest,
// whose gaps the room's job fills
func (j *Jobs) SplitConnected(ctx context.Context, evs []*types.Event) (connected, rest []*types.Event, err error) {
	evs = rooms.SortEventsTopologically(evs)
	stored, err := j.db.Rooms.GetStoredEventIDs(ctx, citedEventIDs(evs))
	if err != nil {
		return nil, nil, err
	}
	connected, rest = splitConnected(evs, stored)
	return connected, rest, nil
}

func citedEventIDs(evs []*types.Event) []id.EventID {
	var cited []id.EventID
	for _, ev := range evs {
		cited = append(cited, ev.PrevEventIDs...)
		cited = append(cited, ev.AuthDependencyIDs()...)
	}
	slices.Sort(cited)
	return slices.Compact(cited)
}

// splitConnected splits events in topological order, keeping it in both parts
func splitConnected(evs []*types.Event, stored map[id.EventID]struct{}) (connected, rest []*types.Event) {
	inPlace := make(map[id.EventID]struct{}, len(stored)+len(evs))
	maps.Copy(inPlace, stored)
	missing := func(eventID id.EventID) bool {
		_, found := inPlace[eventID]
		return !found
	}
	for _, ev := range evs {
		if slices.ContainsFunc(ev.PrevEventIDs, missing) || slices.ContainsFunc(ev.AuthDependencyIDs(), missing) {
			rest = append(rest, ev)
			continue
		}
		inPlace[ev.ID] = struct{}{}
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
