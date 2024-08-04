package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Get the auth chain for one or more events, that is all the auth events for each input event and each of their auth events, and so on, recursively. Introduced in Room V2:
// https://spec.matrix.org/v1.10/rooms/v2/#definitions
func (e *EventsDirectory) TxnGetAuthChainForEvents(
	txn fdb.ReadTransaction,
	evs []*types.Event,
	eventsProvider *TxnEventsProvider,
) ([]*types.Event, error) {
	eventIDToFut := make(map[id.EventID]fdb.FutureByteSlice, len(evs))
	authEvs := make(map[id.EventID]*types.Event, len(evs))

	// Start fetching each direct auth event from each input event
	for _, reqEv := range evs {
		for _, evID := range reqEv.AuthEventIDs {
			if _, found := eventIDToFut[evID]; !found {
				eventIDToFut[evID] = eventsProvider.WillGet(evID)
			}
		}
	}

	for {
		// Exit once we've no more futures to process
		if len(eventIDToFut) == 0 {
			break
		}

		for evID := range eventIDToFut {
			ev, err := eventsProvider.Get(evID)
			if err != nil {
				return nil, err
			}

			// Add to fetched, remove from futures
			authEvs[evID] = ev
			delete(eventIDToFut, evID)

			// Now loop through the auth events auth events, if we're not fetching
			// or have fetched it, start fetching it in another future and continue.
			for _, evID := range ev.AuthEventIDs {
				if _, found := eventIDToFut[evID]; found {
					continue
				}
				if _, found := authEvs[evID]; found {
					continue
				}
				eventIDToFut[evID] = eventsProvider.WillGet(evID)
			}
		}
	}

	authChain := make([]*types.Event, 0, len(authEvs))
	for _, ev := range authEvs {
		authChain = append(authChain, ev)
	}
	return authChain, nil
}
