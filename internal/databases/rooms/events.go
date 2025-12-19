package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Check if we have an event (that we accepted)
func (r *RoomsDatabase) DoesEventExist(ctx context.Context, eventID id.EventID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		key := r.events.KeyForIDToVersion(eventID)
		if b, err := txn.Get(key).Get(); err != nil {
			return false, err
		} else if b == nil {
			return false, nil
		} else {
			return true, nil
		}
	})
}

func (r *RoomsDatabase) MustDoesEventExist(ctx context.Context, eventID id.EventID) bool {
	if ret, err := r.DoesEventExist(ctx, eventID); err != nil {
		panic(err)
	} else {
		return ret
	}
}

func (r *RoomsDatabase) GetEvent(ctx context.Context, eventID id.EventID) (*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Event, error) {
		return r.events.TxnGetEvent(txn, eventID), nil
	})
}

// Get the auth chain for a given event by fetching it's auth events and their auth events, etc recursively
func (r *RoomsDatabase) GetEventAuthChain(ctx context.Context, eventID id.EventID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		reqEv, err := eventsProvider.Get(eventID)
		if err != nil {
			return nil, err
		} else if reqEv == nil {
			return nil, types.ErrEventNotFound
		}
		evs, err := r.events.TxnGetAuthChainForEvents(txn, []*types.Event{reqEv}, eventsProvider)
		if err != nil {
			return nil, err
		}
		util.SortEventList(evs)
		return evs, nil
	})
}

func (r *RoomsDatabase) PaginateAllEventTups(ctx context.Context, options types.PaginationOptions) ([]types.EventTupWithVersion, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]types.EventTupWithVersion, error) {
		return r.events.TxnPaginateAllEventTups(txn, options, nil), nil
	})
}
