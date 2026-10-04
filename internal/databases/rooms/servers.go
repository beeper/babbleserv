package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// IsServerJoined reports whether the server has joined members in the room: this server when the room
// record counts any, another server when its joined member count is above zero too, as nothing is
// served for a room this server is not in.
func (r *RoomsDatabase) IsServerJoined(ctx context.Context, roomID id.RoomID, serverName string) (bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (bool, error) {
		var count fdb.FutureByteSlice
		if serverName != r.config.ServerName {
			count = txn.Get(r.servers.KeyForJoinedCount(roomID, serverName))
		}
		if room, err := roomOrNil(r.txnGetRoom(txn, roomID)); err != nil || !isServerJoined(room) {
			return false, err
		} else if count == nil {
			return true, nil
		}
		b, err := count.Get()
		return servers.JoinedCountOf(b) > 0, err
	})
}

// isServerJoined reports whether this server has joined members in the room, nil for a room without
// a record
func isServerJoined(room *types.Room) bool {
	return room != nil && room.LocalMembers > 0
}

func (r *RoomsDatabase) GetServerMemberships(ctx context.Context, serverName string) (types.Memberships, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		return r.servers.TxnLookupServerMemberships(txn, serverName)
	})
}

// RoomServers returns the servers with joined members in the room, this one included. Once this
// server leaves, the others are those of the room's current state when it left.
func (r *RoomsDatabase) RoomServers(ctx context.Context, roomID id.RoomID) ([]string, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]string, error) {
		return r.servers.TxnLookupRoomServers(txn, roomID)
	})
}
