package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) IsServerInRoom(ctx context.Context, serverName string, roomID id.RoomID) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		return r.servers.TxnIsServerInRoom(txn, serverName, roomID)
	})
}

func (r *RoomsDatabase) GetServerMemberships(ctx context.Context, serverName string) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.servers.TxnLookupServerMemberships(txn, serverName)
	})
}

func (r *RoomsDatabase) GetCurrentRoomServers(ctx context.Context, roomID id.RoomID) ([]string, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]string, error) {
		return r.events.TxnLookupCurrentRoomServers(txn, roomID)
	})
}
