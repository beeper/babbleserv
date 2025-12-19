package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (e *EventsDirectory) TxnIsRoomEncrypted(txn fdb.ReadTransaction, roomID id.RoomID) bool {
	b := txn.Get(e.KeyForRoomCurrentStateTup(roomID, event.StateEncryption, "")).MustGet()
	return b != nil
}

func (e *EventsDirectory) TxnFilterJoinedMembershipsWithEncryption(
	txn fdb.ReadTransaction,
	memberships types.Memberships,
) (types.Memberships, error) {
	// Find joins and kick off fetches for the room encryption event state tup
	futs := make(map[id.RoomID]fdb.FutureByteSlice, len(memberships))
	for roomID, membershipTup := range memberships {
		if membershipTup.Membership == event.MembershipJoin {
			futs[roomID] = txn.Get(e.KeyForRoomCurrentStateTup(roomID, event.StateEncryption, ""))
		}
	}

	// Now make new memberships for only rooms with an encryption event
	newMemberships := make(types.Memberships, len(futs))
	for roomID, fut := range futs {
		b, err := fut.Get()
		if err != nil {
			return nil, err
		} else if b != nil {
			newMemberships[roomID] = memberships[roomID]
		}
	}

	return newMemberships, nil
}
