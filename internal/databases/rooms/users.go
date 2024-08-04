package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) IsUserInRoom(ctx context.Context, userID id.UserID, roomID id.RoomID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		return r.users.TxnIsUserInRoom(txn, userID, roomID)
	})
}

func (r *RoomsDatabase) GetUserMemberships(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.users.TxnLookupUserMemberships(txn, userID)
	})
}

func (r *RoomsDatabase) GetUserOutlierMemberships(ctx context.Context, userID id.UserID) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.users.TxnLookupUserOutlierMemberships(txn, userID)
	})
}
