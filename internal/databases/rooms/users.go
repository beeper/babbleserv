package rooms

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/samber/lo"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/users"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) IsUserJoinedRoom(ctx context.Context, userID id.UserID, roomID id.RoomID) (bool, error) {
	memberships, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (map[id.UserID]types.MembershipTup, error) {
		return r.txnGetRoomUserMemberships(txn, roomID, []id.UserID{userID})
	})
	return memberships[userID].Membership == event.MembershipJoin, err
}

// txnRoomMembers returns the event and membership each user's member tuple holds in the room's
// current state
func (r *RoomsDatabase) txnRoomMembers(txn fdb.ReadTransaction, room *types.Room, userIDs []id.UserID) (types.StateEntries, error) {
	if room == nil || room.CurrentState.IsZero() || len(userIDs) == 0 {
		return types.StateEntries{}, nil
	}
	return r.state.NewBatch(room.ID).TxnLookupEntries(txn, room.CurrentState, events.MemberStateTups(userIDs))
}

// txnGetRoomUserMemberships returns each user's membership of the room, leaving out users without
// one. While this server has no joined members, outlier membership rows take precedence over the
// room's current state.
func (r *RoomsDatabase) txnGetRoomUserMemberships(txn fdb.ReadTransaction, roomID id.RoomID, userIDs []id.UserID) (map[id.UserID]types.MembershipTup, error) {
	room, err := roomOrNil(r.txnGetRoom(txn.Snapshot(), roomID))
	if err != nil {
		return nil, err
	}
	localMembers := 0
	if room != nil {
		localMembers = room.LocalMembers
	}
	rows := make(map[id.UserID]*types.MembershipRow)
	if localMembers == 0 {
		futures := make(map[id.UserID]fdb.FutureByteSlice, len(userIDs))
		for _, userID := range userIDs {
			futures[userID] = txn.Get(r.users.KeyForMembership(userID, roomID))
		}
		if rows, err = users.MembershipRowsOf(futures); err != nil {
			return nil, err
		}
	}
	var fromState []id.UserID
	for _, userID := range userIDs {
		if row := rows[userID]; localMembers > 0 || row == nil || !row.Outlier {
			fromState = append(fromState, userID)
		}
	}
	members, err := r.txnRoomMembers(txn, room, fromState)
	if err != nil {
		return nil, err
	}

	memberships := make(map[id.UserID]types.MembershipTup, len(userIDs))
	for _, userID := range userIDs {
		if row := rows[userID]; localMembers == 0 && row != nil && row.Outlier {
			memberships[userID] = row.MembershipTup
		} else if member := members[types.MemberStateTup(userID)]; member.EventID != "" {
			memberships[userID] = types.MembershipTup{EventID: member.EventID, RoomID: roomID, Membership: member.Membership}
		}
	}
	return memberships, nil
}

// txnUsersJoined reports whether each user is joined to the room, see txnGetRoomUserMemberships
func (r *RoomsDatabase) txnUsersJoined(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	userIDs []id.UserID,
) (map[id.UserID]bool, error) {
	memberships, err := r.txnGetRoomUserMemberships(txn, roomID, userIDs)
	if err != nil {
		return nil, err
	}
	joined := make(map[id.UserID]bool, len(userIDs))
	for userID, membership := range memberships {
		joined[userID] = membership.Membership == event.MembershipJoin
	}
	return joined, nil
}

// GetCurrentMembershipAndEvent returns the user's membership of the room, see txnGetRoomUserMemberships,
// and its event, nil for none.
func (r *RoomsDatabase) GetCurrentMembershipAndEvent(ctx context.Context, userID id.UserID, roomID id.RoomID) (*types.MembershipTup, *types.Event, error) {
	var ev *types.Event
	mtup, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.MembershipTup, error) {
		memberships, err := r.txnGetRoomUserMemberships(txn, roomID, []id.UserID{userID})
		if err != nil {
			return nil, err
		}
		mtup, found := memberships[userID]
		if !found {
			return nil, nil
		}
		if ev = r.events.TxnGetEvent(txn, mtup.EventID); ev == nil {
			return nil, fmt.Errorf("missing membership event: %s", mtup.EventID)
		}
		return &mtup, nil
	})
	return mtup, ev, err
}

// GetUserMemberships returns the user's memberships: a local user's rows, or for a remote user, who
// has none, their members in the rooms this server shares with their server, see remoteUserMemberships.
func (r *RoomsDatabase) GetUserMemberships(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	if !r.isLocalUser(userID) {
		return r.remoteUserMemberships(ctx, userID, false)
	}
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.users.TxnLookupUserMemberships(txn, userID), nil
	})
}

func (r *RoomsDatabase) GetUserJoinedMemberships(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	memberships, err := r.GetUserMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	return lo.PickBy(memberships, func(uid id.RoomID, mtup types.MembershipTup) bool {
		return mtup.Membership == event.MembershipJoin
	}), nil
}

// JoinedRooms returns the rooms the user is joined to, see GetUserMemberships
func (r *RoomsDatabase) JoinedRooms(ctx context.Context, userID id.UserID) ([]id.RoomID, error) {
	memberships, err := r.GetUserJoinedMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(memberships)), nil
}

// GetUserJoinedMembershipsWithEncryption returns the user's joined memberships, see
// GetUserMemberships, of rooms whose current state holds an encryption event
func (r *RoomsDatabase) GetUserJoinedMembershipsWithEncryption(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	if !r.isLocalUser(userID) {
		return r.remoteUserMemberships(ctx, userID, true)
	}
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		memberships := lo.PickBy(r.users.TxnLookupUserMemberships(txn, userID), func(_ id.RoomID, mtup types.MembershipTup) bool {
			return mtup.Membership == event.MembershipJoin
		})
		futures := make(map[id.RoomID]fdb.FutureByteSlice, len(memberships))
		for roomID := range memberships {
			futures[roomID] = txn.Get(r.KeyForRoom(roomID))
		}
		for roomID, future := range futures {
			if roomBytes, err := future.Get(); err != nil {
				return nil, err
			} else if roomBytes == nil || !types.MustNewRoomFromBytes(roomBytes, roomID).Encrypted {
				delete(memberships, roomID)
			}
		}
		return memberships, nil
	})
}

// UsersSharingEncryptedRoom reports which of the users share an encrypted room with a local user:
// those joined in the current state of an encrypted room the local user's rows hold them joined to.
// It looks every user up in each such room, see lookupRoomMembers, and never reads another user's
// rooms.
func (r *RoomsDatabase) UsersSharingEncryptedRoom(ctx context.Context, localUserID id.UserID, userIDs []id.UserID) (map[id.UserID]bool, error) {
	if !r.isLocalUser(localUserID) {
		return nil, fmt.Errorf("%s is not a local user", localUserID)
	}
	encrypted, err := r.GetUserJoinedMembershipsWithEncryption(ctx, localUserID)
	if err != nil {
		return nil, err
	}
	rooms, err := r.lookupRoomMembers(ctx, slices.Collect(maps.Keys(encrypted)), userIDs)
	if err != nil {
		return nil, err
	}
	joined := make(map[id.UserID]bool)
	for _, lookup := range rooms {
		if !lookup.room.Encrypted {
			continue
		}
		for tup, member := range lookup.members {
			if member.Membership == event.MembershipJoin {
				joined[id.UserID(tup.StateKey)] = true
			}
		}
	}
	return joined, nil
}

// Rooms whose current state one read transaction looks users up in, and such transactions run at once
const (
	roomLookupChunk       = 50
	roomLookupConcurrency = 8
)

// roomMembersLookup is a room's record and the entries of the users looked up in its current state
type roomMembersLookup struct {
	room    *types.Room
	members types.StateEntries
}

// remoteUserMemberships looks a remote user up in the current state of each room this server shares
// with the user's server, since remote users have no membership rows. It reads a room record and
// member entry per shared room, optionally keeping only joins in encrypted rooms.
func (r *RoomsDatabase) remoteUserMemberships(ctx context.Context, userID id.UserID, encryptedJoinsOnly bool) (types.Memberships, error) {
	shared, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.servers.TxnLookupServerMemberships(txn, userID.Homeserver())
	})
	if err != nil {
		return nil, err
	}
	rooms, err := r.lookupRoomMembers(ctx, slices.Collect(maps.Keys(shared)), []id.UserID{userID})
	if err != nil {
		return nil, err
	}
	memberships := make(types.Memberships, len(rooms))
	memberTup := types.MemberStateTup(userID)
	for roomID, lookup := range rooms {
		member := lookup.members[memberTup]
		if member.EventID == "" || encryptedJoinsOnly && (!lookup.room.Encrypted || member.Membership != event.MembershipJoin) {
			continue
		}
		memberships[roomID] = types.MembershipTup{EventID: member.EventID, RoomID: roomID, Membership: member.Membership}
	}
	return memberships, nil
}

// lookupRoomMembers looks users up in the current state of each room this server is joined to, see
// inRoomChunks. Rooms without a record or this server's joined members are left out.
func (r *RoomsDatabase) lookupRoomMembers(ctx context.Context, roomIDs []id.RoomID, userIDs []id.UserID) (map[id.RoomID]roomMembersLookup, error) {
	return inRoomChunks(ctx, roomIDs, func(ctx context.Context, chunk []id.RoomID) (map[id.RoomID]roomMembersLookup, error) {
		return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (map[id.RoomID]roomMembersLookup, error) {
			return r.txnLookupRoomMembers(txn, chunk, userIDs)
		})
	})
}

// inRoomChunks reads the rooms roomLookupChunk at a time, each chunk in a read transaction of its
// own, roomLookupConcurrency at once, and merges what they return. The first error stops it.
func inRoomChunks[T any](
	ctx context.Context,
	roomIDs []id.RoomID,
	read func(ctx context.Context, chunk []id.RoomID) (map[id.RoomID]T, error),
) (map[id.RoomID]T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(map[id.RoomID]T, len(roomIDs))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	running := make(chan struct{}, roomLookupConcurrency)
	for chunk := range slices.Chunk(slices.Sorted(slices.Values(roomIDs)), roomLookupChunk) {
		running <- struct{}{}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-running }()
			chunkFound, err := read(ctx, chunk)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
			maps.Copy(found, chunkFound)
		})
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return found, firstErr
}

func (r *RoomsDatabase) txnLookupRoomMembers(
	txn fdb.ReadTransaction,
	roomIDs []id.RoomID,
	userIDs []id.UserID,
) (map[id.RoomID]roomMembersLookup, error) {
	futures := make([]fdb.FutureByteSlice, len(roomIDs))
	for i, roomID := range roomIDs {
		futures[i] = txn.Get(r.KeyForRoom(roomID))
	}
	found := make(map[id.RoomID]roomMembersLookup, len(roomIDs))
	for i, roomID := range roomIDs {
		roomBytes, err := futures[i].Get()
		if err != nil {
			return nil, err
		} else if roomBytes == nil {
			continue
		}
		room, err := types.NewRoomFromBytes(roomBytes, roomID)
		if err != nil {
			return nil, err
		} else if !isServerJoined(room) {
			continue
		}
		members, err := r.txnRoomMembers(txn, room, userIDs)
		if err != nil {
			return nil, err
		}
		found[roomID] = roomMembersLookup{room: room, members: members}
	}
	return found, nil
}

func (r *RoomsDatabase) WasUserJoinedRoomAtEvent(ctx context.Context, userID id.UserID, roomID id.RoomID, evID id.EventID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		ev := r.events.TxnGetEvent(txn, evID)
		if !ev.HasStateIn(roomID) {
			return false, nil
		}
		// Check the state after the event, so the user's own join is included, and also check the
		// state before if we're looking at our own membership, so we can see our own leave/ban.
		stateCtxs := []types.StateHash{ev.AfterState}
		isOwnMembership := ev.Type == event.StateMember && ev.StateKey != nil && *ev.StateKey == userID.String()
		if isOwnMembership && !ev.BeforeState.IsZero() {
			stateCtxs = append(stateCtxs, ev.BeforeState)
		}

		stateBatch := r.state.NewBatch(roomID)
		memberTups := events.MemberStateTups([]id.UserID{userID})
		for _, stateCtx := range stateCtxs {
			members, err := stateBatch.TxnLookupEntries(txn, stateCtx, memberTups)
			if err != nil {
				return false, err
			} else if members[memberTups[0]].Membership == event.MembershipJoin {
				return true, nil
			}
		}
		return false, nil
	})
}
