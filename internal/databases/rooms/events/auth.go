package events

import (
	"fmt"
	"maps"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// AuthEventIDs maps event IDs to their auth event IDs. Chain walks sharing one map read each
// event's auth event IDs once, and events not yet stored can be added to it up front.
type AuthEventIDs map[id.EventID][]id.EventID

// TxnGetAuthChainIDs returns the seeds and every event reachable from them over auth events. The
// walk is breadth first with one batch of reads per level for events missing from known, which it
// fills in.
func (e *EventsDirectory) TxnGetAuthChainIDs(
	txn fdb.ReadTransaction,
	seeds []id.EventID,
	known AuthEventIDs,
) (map[id.EventID]struct{}, error) {
	if known == nil {
		known = make(AuthEventIDs)
	}

	chain := make(map[id.EventID]struct{}, len(seeds))
	frontier := make([]id.EventID, 0, len(seeds))
	for _, eventID := range seeds {
		if _, found := chain[eventID]; !found {
			chain[eventID] = struct{}{}
			frontier = append(frontier, eventID)
		}
	}

	_, load := e.authHeaderReader(txn, nil)
	for len(frontier) > 0 {
		var missing []id.EventID
		for _, eventID := range frontier {
			if _, found := known[eventID]; !found {
				missing = append(missing, eventID)
			}
		}
		headers, err := load(missing)
		if err != nil {
			return nil, fmt.Errorf("failed to get auth chain: %w", err)
		}
		for i, h := range headers {
			known[missing[i]] = h.AuthEventIDs
		}
		var next []id.EventID
		for _, eventID := range frontier {
			for _, authEventID := range known[eventID] {
				if _, found := chain[authEventID]; !found {
					chain[authEventID] = struct{}{}
					next = append(next, authEventID)
				}
			}
		}
		frontier = next
	}

	return chain, nil
}

// Get the auth chain for one or more events, that is all the auth events for each input event and each of their auth events, and so on, recursively. Introduced in Room V2:
// https://spec.matrix.org/v1.10/rooms/v2/#definitions
func (e *EventsDirectory) TxnGetAuthChainForEvents(
	txn fdb.ReadTransaction,
	evs []*types.Event,
	eventsProvider *TxnEventsProvider,
) ([]*types.Event, error) {
	// Events held by the provider may not be stored yet, such as a federated batch being authorized
	known := eventsProvider.authEventIDs()
	var seeds []id.EventID
	for _, ev := range evs {
		known[ev.ID] = ev.AuthEventIDs
		seeds = append(seeds, ev.AuthDependencyIDs()...)
	}

	chainIDs, err := e.TxnGetAuthChainIDs(txn, seeds, known)
	if err != nil {
		return nil, err
	}

	authChain, err := eventsProvider.GetAll(slices.Collect(maps.Keys(chainIDs)))
	if err != nil {
		return nil, fmt.Errorf("failed to get auth chain: %w", err)
	}
	return authChain, nil
}
