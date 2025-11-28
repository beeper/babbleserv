package rooms

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (r *RoomsDatabase) txnGetVersionsForMemberships(txn fdb.ReadTransaction, memberships types.Memberships) (map[id.RoomID]tuple.Versionstamp, error) {
	gets := make(map[id.RoomID]fdb.FutureByteSlice, len(memberships))
	for roomID := range memberships {
		gets[roomID] = txn.Get(r.KeyForRoomVersion(roomID))
	}
	roomToVersions := make(map[id.RoomID]tuple.Versionstamp, len(memberships))
	for roomID := range memberships {
		eVersionB, err := gets[roomID].Get()
		if err != nil {
			return nil, err
		} else if eVersionB != nil {
			roomToVersions[roomID] = types.MustBytesToVersionstamp(eVersionB)
		}
	}

	return roomToVersions, nil
}
