package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (e *EventsDirectory) TxnPaginateAllEventTups(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
) []types.EventTupWithVersion {
	iter := txn.GetRange(
		e.rangeForVersion(options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	evIDs := make([]types.EventTupWithVersion, 0, options.Limit)

	for iter.Advance() {
		kv := iter.MustGet()
		version := e.KeyToVersion(kv.Key)
		tup := types.EventTupWithVersion{
			EventTup: types.BytesToEventTup(kv.Value),
			Version:  version,
		}
		evIDs = append(evIDs, tup)
		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs
}

func (e *EventsDirectory) TxnPaginateRoomEventTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
) []types.EventTupWithVersion {
	return e.txnPaginateRoomEventIDs(
		txn,
		options,
		e.rangeForRoomVersion(roomID, options.From, options.To),
		e.KeyToRoomVersion,
		eventsProvider,
	)
}

func (e *EventsDirectory) TxnPaginateLocalRoomEventTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
) []types.EventTupWithVersion {
	return e.txnPaginateRoomEventIDs(
		txn,
		options,
		e.rangeForLocalRoomVersion(roomID, options.From, options.To),
		e.KeyToLocalRoomVersion,
		eventsProvider,
	)
}

func (e *EventsDirectory) txnPaginateRoomEventIDs(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
	keyRange fdb.Range,
	keyToVersion func(fdb.Key) tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
) []types.EventTupWithVersion {
	iter := txn.GetRange(keyRange, options.RangeOptions()).Iterator()
	evIDs := make([]types.EventTupWithVersion, 0, options.Limit)

	for iter.Advance() {
		kv := iter.MustGet()
		tup := types.EventTupWithVersion{
			EventTup: types.BytesToEventTup(kv.Value),
			Version:  keyToVersion(kv.Key),
		}
		evIDs = append(evIDs, tup)
		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs
}
