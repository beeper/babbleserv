package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) IsUserJoinedRoom(ctx context.Context, userID id.UserID, roomID id.RoomID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		return r.users.TxnIsUserJoinedRoom(txn, userID, roomID), nil
	})
}

func (r *RoomsDatabase) GetUserMembership(ctx context.Context, userID id.UserID, roomID id.RoomID) (*types.MembershipTup, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.MembershipTup, error) {
		return r.users.TxnGetMembership(txn, userID, roomID), nil
	})
}

func (r *RoomsDatabase) GetUserMemberships(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.users.TxnLookupUserMemberships(txn, userID), nil
	})
}

func (r *RoomsDatabase) GetUserJoinedMembershipsWithEncryption(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		memberships := r.users.TxnLookupUserMemberships(txn, userID)
		return r.events.TxnFilterJoinedMembershipsWithEncryption(txn, memberships)
	})
}

func (r *RoomsDatabase) WasUserJoinedRoomAtEvent(ctx context.Context, userID id.UserID, roomID id.RoomID, evID id.EventID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		stateMap := r.events.TxnLookupSpecificRoomMemberStateMapAtEvent(ctx, txn, roomID, []id.UserID{userID}, evID, eventsProvider)
		ev := eventsProvider.MustGet(stateMap[types.StateTup{
			Type:     event.StateMember,
			StateKey: userID.String(),
		}])
		if ev == nil {
			return false, nil
		}
		return ev.Membership() == event.MembershipJoin, nil
	})
}
