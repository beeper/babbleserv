package events

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// TxnStoreRoomStateStep records the room's current state context after the event stored at version
// moved it there.
func (e *EventsDirectory) TxnStoreRoomStateStep(txn fdb.Transaction, roomID id.RoomID, version tuple.Versionstamp, stateCtx types.StateHash) {
	txn.SetVersionstampedKey(e.keyForRoomStateStep(roomID, version), tuple.Tuple{stateCtx[:]}.Pack())
}

// TxnLookupRoomStateAt returns the room's current state context as of version, false before the
// room's first step.
func (e *EventsDirectory) TxnLookupRoomStateAt(txn fdb.ReadTransaction, roomID id.RoomID, version tuple.Versionstamp) (types.StateHash, bool, error) {
	if version == types.ZeroVersionstamp {
		return types.StateHash{}, false, nil
	}
	kvs, err := txn.GetRange(e.rangeForRoomStateAt(roomID, version), fdb.RangeOptions{Reverse: true, Limit: 1}).GetSliceWithError()
	if err != nil || len(kvs) == 0 {
		return types.StateHash{}, false, err
	}
	tup, err := tuple.Unpack(kvs[0].Value)
	if err != nil {
		return types.StateHash{}, false, fmt.Errorf("invalid room state step in %s: %w", roomID, err)
	}
	var stateCtx []byte
	if len(tup) == 1 {
		stateCtx, _ = tup[0].([]byte)
	}
	if len(stateCtx) != len(types.StateHash{}) {
		return types.StateHash{}, false, fmt.Errorf("invalid room state step in %s: %v", roomID, tup)
	}
	return types.StateHash(stateCtx), true, nil
}

func (e *EventsDirectory) keyForRoomStateStep(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(e.roomVersionToState, tuple.Tuple{roomID.String(), version})
}

// rangeForRoomStateAt covers the room's steps up to and including version
func (e *EventsDirectory) rangeForRoomStateAt(roomID id.RoomID, version tuple.Versionstamp) fdb.ExactRange {
	return types.GetVersionRange(e.roomVersionToState, types.ZeroVersionstamp, version, roomID.String())
}
