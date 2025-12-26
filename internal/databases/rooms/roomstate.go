package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) IsRoomEncrypted(ctx context.Context, roomID id.RoomID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		isEncrypted := r.events.TxnIsRoomEncrypted(txn, roomID)
		return isEncrypted, nil
	})
}

func (r *RoomsDatabase) GetCurrentRoomStateEvent(ctx context.Context, roomID id.RoomID, stateTup types.StateTup) (*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		return r.events.TxnGetCurrentRoomStateEvent(txn, roomID, stateTup, eventsProvider), nil
	})
}

func (r *RoomsDatabase) GetCurrentRoomMemberships(ctx context.Context, roomID id.RoomID) (types.RoomMemberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.RoomMemberships, error) {
		return r.events.TxnLookupCurrentRoomMemberships(txn, roomID, nil), nil
	})
}

func (r *RoomsDatabase) GetRoomStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupRoomStateAndMemberMapAtEvent(txn, roomID, eventID, nil), nil
	})
}

func (r *RoomsDatabase) GetRoomAuthStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupRoomAuthStateMapAtEvent(ctx, txn, roomID, eventID, nil), nil
	})
}

func (r *RoomsDatabase) GetRoomSpecificRoomMemberStateMapAtEvent(ctx context.Context, roomID id.RoomID, userIDs []id.UserID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		return r.events.TxnLookupSpecificRoomMemberStateMapAtEvent(ctx, txn, roomID, userIDs, eventID, nil), nil
	})
}

func (r *RoomsDatabase) GetCurrentRoomStrippedStateEvents(ctx context.Context, roomID id.RoomID) ([]*types.PartialEvent, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.PartialEvent, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap := r.events.TxnLookupCurrentRoomStrippedStateStateMap(txn, roomID, eventsProvider)
		evs := make([]*types.Event, 0, len(stateMap))
		for _, evID := range stateMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		util.SortEventList(evs)
		return util.EventsToPartialEvents(evs), nil
	})
}

func (r *RoomsDatabase) GetCurrentRoomMemberEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	if memberEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		memberMap := r.events.TxnLookupCurrentRoomMemberStateMap(txn, roomID, eventsProvider)
		evs := make([]*types.Event, 0, len(memberMap))
		for _, evID := range memberMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		return evs, nil
	}); err != nil {
		return nil, err
	} else {
		util.SortEventList(memberEvs)
		return memberEvs, nil
	}
}

func (r *RoomsDatabase) GetCurrentRoomStateEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	stateEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap := r.events.TxnLookupCurrentRoomStateAndMemberMap(txn, roomID, eventsProvider)
		evs := make([]*types.Event, 0, len(stateMap))
		for _, evID := range stateMap {
			evs = append(evs, eventsProvider.MustGet(evID))
		}
		return evs, nil
	})
	if err != nil {
		return nil, err
	}

	util.SortEventList(stateEvs)
	return stateEvs, nil
}

type stateWithAuthChain struct {
	StateEvents []*types.Event
	AuthChain   []*types.Event
}

func (r *RoomsDatabase) GetCurrentRoomStateEventsWithAuthChain(ctx context.Context, roomID id.RoomID) (stateWithAuthChain, error) {
	var res stateWithAuthChain
	var err error

	res.StateEvents, err = r.GetCurrentRoomStateEvents(ctx, roomID)
	if err != nil {
		return res, err
	}

	if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn).WithEvents(res.StateEvents...)
		if authChain, err := r.events.TxnGetAuthChainForEvents(txn, res.StateEvents, eventsProvider); err != nil {
			return nil, err
		} else {
			res.AuthChain = authChain
		}
		return nil, nil
	}); err != nil {
		return res, err
	}

	util.SortEventList(res.StateEvents)
	util.SortEventList(res.AuthChain)
	return res, nil
}

// Get room state (state events + their auth chain) at a given event. This is broken down into
// multuple read transactions so we don't hit the 5s limit. Since events are immutableish we don't
// need the consistency guarantees of a single transaction.
func (r *RoomsDatabase) GetRoomStateWithAuthChainAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (stateWithAuthChain, error) {
	var res stateWithAuthChain

	if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap := r.events.TxnLookupRoomStateAndMemberMapAtEvent(txn, roomID, eventID, eventsProvider)
		res.StateEvents = make([]*types.Event, 0, len(stateMap))
		for _, eventID := range stateMap {
			res.StateEvents = append(res.StateEvents, eventsProvider.MustGet(eventID))
		}
		return nil, nil
	}); err != nil {
		return res, err
	}

	if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn).WithEvents(res.StateEvents...)
		if authChain, err := r.events.TxnGetAuthChainForEvents(txn, res.StateEvents, eventsProvider); err != nil {
			return nil, err
		} else {
			res.AuthChain = authChain
		}
		return nil, nil
	}); err != nil {
		return res, err
	}

	util.SortEventList(res.StateEvents)
	util.SortEventList(res.AuthChain)
	return res, nil
}

type stateWithAuthChainIDs struct {
	StateEventIDs []id.EventID
	AuthChainIDs  []id.EventID
}

func (r *RoomsDatabase) GetRoomStateWithAuthChainIDsAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (stateWithAuthChainIDs, error) {
	var res stateWithAuthChainIDs

	roomStateAtEvent, err := r.GetRoomStateWithAuthChainAtEvent(ctx, roomID, eventID)
	if err != nil {
		return res, err
	}

	res.StateEventIDs = make([]id.EventID, 0, len(roomStateAtEvent.StateEvents))
	for _, ev := range roomStateAtEvent.StateEvents {
		res.StateEventIDs = append(res.StateEventIDs, ev.ID)
	}
	res.AuthChainIDs = make([]id.EventID, 0, len(roomStateAtEvent.AuthChain))
	for _, ev := range roomStateAtEvent.AuthChain {
		res.AuthChainIDs = append(res.AuthChainIDs, ev.ID)
	}

	return res, nil
}
