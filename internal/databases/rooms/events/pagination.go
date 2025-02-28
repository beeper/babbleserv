package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (e *EventsDirectory) TxnPaginateAllEventRoomIDTups(
	txn fdb.ReadTransaction,
	fromVersion, toVersion tuple.Versionstamp,
	limit int,
	eventsProvider *TxnEventsProvider,
) ([]types.EventIDTupWithVersion, error) {
	iter := txn.GetRange(
		e.RangeForVersion(fromVersion, toVersion),
		fdb.RangeOptions{
			Limit: limit,
		},
	).Iterator()

	evIDs := make([]types.EventIDTupWithVersion, 0, limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		version := e.KeyToVersion(kv.Key)
		tup := types.EventIDTupWithVersion{
			EventIDTup: types.ValueToEventIDTup(kv.Value),
			Version:    version,
		}
		evIDs = append(evIDs, tup)
		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs, nil
}

func (e *EventsDirectory) TxnPaginateRoomEventIDTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
	limit int,
	eventsProvider *TxnEventsProvider,
) ([]types.EventIDTupWithVersion, error) {
	iter := txn.GetRange(
		e.RangeForRoomVersion(roomID, fromVersion, toVersion),
		fdb.RangeOptions{
			Limit: limit,
		},
	).Iterator()

	evIDs := make([]types.EventIDTupWithVersion, 0, limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		version := e.KeyToRoomVersion(kv.Key)
		tup := types.EventIDTupWithVersion{
			EventIDTup: types.EventIDTup{
				EventID: id.EventID(kv.Value),
			},
			Version: version,
		}
		evIDs = append(evIDs, tup)
		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs, nil
}
