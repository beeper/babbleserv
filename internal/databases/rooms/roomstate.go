package rooms

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) txnGetRoom(txn fdb.ReadTransaction, roomID id.RoomID) (*types.Room, error) {
	roomBytes, err := txn.Get(r.KeyForRoom(roomID)).Get()
	if err != nil {
		return nil, err
	} else if roomBytes == nil {
		return nil, fmt.Errorf("%w: %s", types.ErrRoomNotFound, roomID)
	}
	return types.NewRoomFromBytes(roomBytes, roomID)
}

func roomOrNil(room *types.Room, err error) (*types.Room, error) {
	if errors.Is(err, types.ErrRoomNotFound) {
		return nil, nil
	}
	return room, err
}

func roomCurrentState(room *types.Room) types.StateHash {
	if room == nil || room.CurrentState.IsZero() {
		return state.EmptyContext
	}
	return room.CurrentState
}

func (r *RoomsDatabase) txnCurrentState(txn fdb.ReadTransaction, roomID id.RoomID) (types.StateHash, error) {
	room, err := roomOrNil(r.txnGetRoom(txn, roomID))
	return roomCurrentState(room), err
}

func (r *RoomsDatabase) IsRoomEncrypted(ctx context.Context, roomID id.RoomID) (bool, error) {
	room, err := r.GetRoom(ctx, roomID)
	return room != nil && room.Encrypted, err
}

// RoomState returns the events of the room's current state, sorted
func (r *RoomsDatabase) RoomState(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		current, err := r.txnCurrentState(txn, roomID)
		if err != nil {
			return nil, err
		}
		state, err := r.state.NewBatch(roomID).TxnIterateEntries(txn, current)
		if err != nil {
			return nil, err
		}
		return txnGetStateEvents(r.events.NewTxnEventsProvider(ctx, txn), state.EventIDs())
	})
}

// RoomStateEvent returns the event of a tuple of the room's current state, nil for none
func (r *RoomsDatabase) RoomStateEvent(ctx context.Context, roomID id.RoomID, tup types.StateTup) (*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Event, error) {
		evs, err := r.txnRoomStateEvents(ctx, txn, roomID, []types.StateTup{tup})
		if err != nil || len(evs) == 0 {
			return nil, err
		}
		return evs[0], nil
	})
}

// RoomStrippedState returns the state of the room's current state that invites and knocks carry
func (r *RoomsDatabase) RoomStrippedState(ctx context.Context, roomID id.RoomID) ([]*types.PartialEvent, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.PartialEvent, error) {
		evs, err := r.txnRoomStateEvents(ctx, txn, roomID, events.StrippedStateTups)
		return util.EventsToPartialEvents(evs), err
	})
}

// txnRoomStateEvents returns the events of those of the tuples in the room's current state, sorted
func (r *RoomsDatabase) txnRoomStateEvents(ctx context.Context, txn fdb.ReadTransaction, roomID id.RoomID, tups []types.StateTup) ([]*types.Event, error) {
	current, err := r.txnCurrentState(txn, roomID)
	if err != nil {
		return nil, err
	}
	state, err := r.state.NewBatch(roomID).TxnLookupEntries(txn, current, tups)
	if err != nil {
		return nil, err
	}
	return txnGetStateEvents(r.events.NewTxnEventsProvider(ctx, txn), state.EventIDs())
}

// RoomMembers returns the members of the room's current state with any of the memberships, every
// member when none is given
func (r *RoomsDatabase) RoomMembers(ctx context.Context, roomID id.RoomID, memberships ...event.Membership) (types.RoomMemberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.RoomMemberships, error) {
		current, err := r.txnCurrentState(txn, roomID)
		if err != nil {
			return nil, err
		}
		members, err := r.state.NewBatch(roomID).TxnIterateMembers(txn, current)
		if err != nil {
			return nil, err
		}
		roomMembers := make(types.RoomMemberships, len(members))
		for userID, member := range members {
			if len(memberships) == 0 || slices.Contains(memberships, member.Membership) {
				roomMembers[userID] = types.MembershipTup{EventID: member.EventID, RoomID: roomID, Membership: member.Membership}
			}
		}
		return roomMembers, nil
	})
}

// RoomMemberEvents returns the member events of the room's current state, sorted
func (r *RoomsDatabase) RoomMemberEvents(ctx context.Context, roomID id.RoomID) ([]*types.Event, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		current, err := r.txnCurrentState(txn, roomID)
		if err != nil {
			return nil, err
		}
		members, err := r.state.NewBatch(roomID).TxnIterateMembers(txn, current)
		if err != nil {
			return nil, err
		}
		stateMap := make(types.StateMap, len(members))
		for userID, member := range members {
			stateMap[types.MemberStateTup(userID)] = member.EventID
		}
		return txnGetStateEvents(r.events.NewTxnEventsProvider(ctx, txn), stateMap)
	})
}

// LocalJoinedMembers returns this server's joined members of the room
func (r *RoomsDatabase) LocalJoinedMembers(ctx context.Context, roomID id.RoomID) (types.RoomMemberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.RoomMemberships, error) {
		rows, err := txn.GetRange(r.localMembers.Sub(roomID.String()), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		members := make(types.RoomMemberships, len(rows))
		for _, row := range rows {
			userID, member, err := r.localMemberOf(row)
			if err != nil {
				return nil, err
			}
			members[userID] = member
		}
		return members, nil
	})
}

var errInvalidLocalMember = errors.New("invalid local joined member row")

// localMemberOf decodes a row of the room's local joined members
func (r *RoomsDatabase) localMemberOf(kv fdb.KeyValue) (id.UserID, types.MembershipTup, error) {
	keyTup, err := r.localMembers.Unpack(kv.Key)
	userID, userOK := "", false
	if err == nil && len(keyTup) == 2 {
		userID, userOK = keyTup[1].(string)
	}
	tup, err := tuple.Unpack(kv.Value)
	if err != nil || len(tup) != 3 || !userOK {
		return "", types.MembershipTup{}, fmt.Errorf("%w: %x", errInvalidLocalMember, []byte(kv.Key))
	}
	eventID, eventOK := tup[0].(string)
	roomID, roomOK := tup[1].(string)
	membership, membershipOK := tup[2].(string)
	if !eventOK || !roomOK || !membershipOK {
		return "", types.MembershipTup{}, fmt.Errorf("%w: %x", errInvalidLocalMember, []byte(kv.Key))
	}
	return id.UserID(userID), types.MembershipTup{EventID: id.EventID(eventID), RoomID: id.RoomID(roomID), Membership: event.Membership(membership)}, nil
}

// LocalMembersJoinedWith returns the room's local members joined in the state after a join, the
// joiner included, when the join's step, stored at version, made this server joined, otherwise none.
func (r *RoomsDatabase) LocalMembersJoinedWith(ctx context.Context, join *types.Event, version tuple.Versionstamp) (types.RoomMemberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.RoomMemberships, error) {
		change, err := txn.Get(r.servers.KeyForMembershipChange(r.config.ServerName, version)).Get()
		if err != nil || change == nil {
			return nil, err
		} else if mtup := types.BytesToMembershipTup(change); mtup.RoomID != join.RoomID || mtup.Membership != event.MembershipJoin {
			return nil, nil
		}
		rows, err := txn.GetRange(r.localMembers.Sub(join.RoomID.String()), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		userIDs := make([]id.UserID, 0, len(rows))
		for _, row := range rows {
			userID, _, err := r.localMemberOf(row)
			if err != nil {
				return nil, err
			}
			userIDs = append(userIDs, userID)
		}
		entries, err := r.state.NewBatch(join.RoomID).TxnLookupEntries(txn, join.AfterState, events.MemberStateTups(userIDs))
		if err != nil {
			return nil, err
		}
		members := make(types.RoomMemberships, len(entries))
		for tup, entry := range entries {
			if entry.Membership == event.MembershipJoin {
				members[id.UserID(tup.StateKey)] = types.MembershipTup{EventID: entry.EventID, RoomID: join.RoomID, Membership: entry.Membership}
			}
		}
		return members, nil
	})
}

func (r *RoomsDatabase) txnGetEventWithState(txn fdb.ReadTransaction, roomID id.RoomID, eventID id.EventID) (*types.Event, error) {
	ev := r.events.TxnGetEvent(txn, eventID)
	if ev == nil || ev.RoomID != roomID {
		return nil, types.ErrEventNotFound
	} else if !ev.HasStateIn(roomID) {
		return nil, types.ErrStateUnavailable
	}
	return ev, nil
}

func (r *RoomsDatabase) txnGetStateAtEvent(txn fdb.ReadTransaction, roomID id.RoomID, eventID id.EventID) (*types.Event, types.StateMap, error) {
	ev, err := r.txnGetEventWithState(txn, roomID, eventID)
	if err != nil {
		return nil, nil, err
	}
	state, err := r.state.NewBatch(roomID).TxnIterateEntries(txn, ev.BeforeState)
	return ev, state.EventIDs(), err
}

func (r *RoomsDatabase) lookupStateAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
	tups []types.StateTup,
) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		ev, err := r.txnGetEventWithState(txn, roomID, eventID)
		if err != nil {
			return nil, err
		}
		state, err := r.state.NewBatch(roomID).TxnLookupEntries(txn, ev.BeforeState, tups)
		return state.EventIDs(), err
	})
}

func (r *RoomsDatabase) GetRoomStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.StateMap, error) {
		_, stateMap, err := r.txnGetStateAtEvent(txn, roomID, eventID)
		return stateMap, err
	})
}

func (r *RoomsDatabase) GetRoomAuthStateMapAtEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (types.StateMap, error) {
	return r.lookupStateAtEvent(ctx, roomID, eventID, events.AuthStateTupsWithMembers(nil))
}

func (r *RoomsDatabase) GetRoomSpecificRoomMemberStateMapAtEvent(ctx context.Context, roomID id.RoomID, userIDs []id.UserID, eventID id.EventID) (types.StateMap, error) {
	return r.lookupStateAtEvent(ctx, roomID, eventID, events.MemberStateTups(userIDs))
}

// Reads the events of a state map, sorted
func txnGetStateEvents(eventsProvider *events.TxnEventsProvider, stateMap types.StateMap) ([]*types.Event, error) {
	evs, err := eventsProvider.GetAll(slices.Collect(maps.Values(stateMap)))
	if err != nil {
		return nil, fmt.Errorf("failed to get state: %w", err)
	}
	util.SortEventList(evs)
	return evs, nil
}

type stateWithAuthChain struct {
	StateEvents []*types.Event
	AuthChain   []*types.Event
}

// Adds the auth chain of the sorted state events, read in a transaction of its own
func (r *RoomsDatabase) withAuthChain(ctx context.Context, stateEvs []*types.Event) (stateWithAuthChain, error) {
	authChain, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn).WithEvents(stateEvs...)
		return r.events.TxnGetAuthChainForEvents(txn, stateEvs, eventsProvider)
	})
	if err != nil {
		return stateWithAuthChain{}, err
	}
	util.SortEventList(authChain)
	return stateWithAuthChain{StateEvents: stateEvs, AuthChain: authChain}, nil
}

// GetCurrentRoomStateWithAuthChain returns the room's current state with its auth chain
func (r *RoomsDatabase) GetCurrentRoomStateWithAuthChain(ctx context.Context, roomID id.RoomID) (stateWithAuthChain, error) {
	stateEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		room, err := r.txnGetRoom(txn, roomID)
		if err != nil {
			return nil, err
		} else if room.CurrentState.IsZero() {
			return nil, types.ErrStateUnavailable
		}
		state, err := r.state.NewBatch(roomID).TxnIterateEntries(txn, room.CurrentState)
		if err != nil {
			return nil, err
		}
		return txnGetStateEvents(r.events.NewTxnEventsProvider(ctx, txn), state.EventIDs())
	})
	if err != nil {
		return stateWithAuthChain{}, err
	}
	return r.withAuthChain(ctx, stateEvs)
}

func (r *RoomsDatabase) GetRoomStateWithAuthChainAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (stateWithAuthChain, error) {
	stateEvs, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Event, error) {
		_, stateMap, err := r.txnGetStateAtEvent(txn, roomID, eventID)
		if err != nil {
			return nil, err
		}
		return txnGetStateEvents(r.events.NewTxnEventsProvider(ctx, txn), stateMap)
	})
	if err != nil {
		return stateWithAuthChain{}, err
	}
	return r.withAuthChain(ctx, stateEvs)
}

type stateWithAuthChainIDs struct {
	StateEventIDs []id.EventID
	AuthChainIDs  []id.EventID
}

// Walks the auth event IDs stored for each event rather than reading the events
func (r *RoomsDatabase) GetRoomStateWithAuthChainIDsAtEvent(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
) (stateWithAuthChainIDs, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (stateWithAuthChainIDs, error) {
		ev, stateMap, err := r.txnGetStateAtEvent(txn, roomID, eventID)
		if err != nil {
			return stateWithAuthChainIDs{}, err
		}
		stateIDs := slices.AppendSeq(make([]id.EventID, 0, len(stateMap)), maps.Values(stateMap))
		slices.Sort(stateIDs)

		// The auth chain of the state is what a walk from it reaches over at least one auth event:
		// the auth events of every event the walk reaches
		authEventIDs := make(events.AuthEventIDs, len(stateIDs))
		reached, err := r.events.TxnGetAuthChainIDs(txn, stateIDs, authEventIDs)
		if err != nil {
			return stateWithAuthChainIDs{}, err
		}
		authChain := make(map[id.EventID]struct{}, len(reached))
		for eventID := range reached {
			for _, authEventID := range authEventIDs[eventID] {
				authChain[authEventID] = struct{}{}
			}
		}
		// In a room whose ID derives from the create event, every other event cites it implicitly
		if createID := ev.ImplicitCreateEventID(); createID != "" {
			for eventID := range reached {
				if eventID != createID {
					authChain[createID] = struct{}{}
					break
				}
			}
		}

		authChainIDs := slices.AppendSeq(make([]id.EventID, 0, len(authChain)), maps.Keys(authChain))
		slices.Sort(authChainIDs)
		return stateWithAuthChainIDs{StateEventIDs: stateIDs, AuthChainIDs: authChainIDs}, nil
	})
}
