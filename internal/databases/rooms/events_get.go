package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
)

func (r *RoomsDatabase) GetEvent(ctx context.Context, evID id.EventID) (*types.Event, error) {
	if result, err := r.db.ReadTransact(func(txn fdb.ReadTransaction) (any, error) {
		key := r.events.KeyForEvent(evID)
		b := txn.Get(key).MustGet()
		ev := types.MustNewEventFromBytes(b)
		return ev, nil
	}); err != nil {
		return nil, err
	} else {
		return result.(*types.Event), nil
	}
}

// Get the auth chain for a given event by fetching it's auth events and their auth events, etc recursively
func (r *RoomsDatabase) GetEventAuthChain(ctx context.Context, eventID id.EventID) ([]*types.Event, error) {
	if result, err := r.db.ReadTransact(func(txn fdb.ReadTransaction) (any, error) {
		// First grab the event
		key := r.events.KeyForEvent(eventID)
		b := txn.Get(key).MustGet()
		reqEv := types.MustNewEventFromBytes(b)

		// Now we can start walking the map
		eventIDToFut := make(map[id.EventID]fdb.FutureByteSlice)
		events := make(map[id.EventID]*types.Event)

		// Seed the id -> future and start fetching each direct auth event
		for _, evID := range reqEv.AuthEventIDs {
			if _, found := eventIDToFut[evID]; !found {
				eventIDToFut[evID] = txn.Get(r.events.KeyForEvent(evID))
			}
		}

		for {
			// Exit once we've no more futures to process
			if len(eventIDToFut) == 0 {
				break
			}

			for evID, fut := range eventIDToFut {
				ev := types.MustNewEventFromBytes(fut.MustGet())

				// Add to fetched, remove from futures
				events[evID] = ev
				delete(eventIDToFut, evID)

				// Now loop through the auth events auth events, if we're not fetching or have
				// fetched it, start fetching it in another future and continue.
				for _, evID := range ev.AuthEventIDs {
					if _, found := eventIDToFut[evID]; found {
						continue
					}
					if _, found := events[evID]; found {
						continue
					}
					eventIDToFut[evID] = txn.Get(r.events.KeyForEvent(evID))
				}
			}
		}

		authChain := make([]*types.Event, 0, len(events))
		for _, ev := range events {
			authChain = append(authChain, ev)
		}

		return authChain, nil
	}); err != nil {
		return nil, err
	} else {
		return result.([]*types.Event), nil
	}
}
