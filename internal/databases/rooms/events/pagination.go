package events

import (
	"context"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (e *EventsDirectory) TxnPaginateAllEventTups(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
) ([]types.EventTupWithVersion, error) {
	iter := txn.GetRange(
		e.RangeForVersion(options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	evIDs := make([]types.EventTupWithVersion, 0, options.Limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
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

	return evIDs, nil
}

func (e *EventsDirectory) TxnPaginateRoomStateEventTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
) ([]types.EventStateTupWithVersion, error) {
	iter := txn.GetRange(
		e.RangeForRoomStateVersion(roomID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	evIDs := make([]types.EventStateTupWithVersion, 0, options.Limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		version := e.KeyToRoomStateVersion(kv.Key)
		stateTup := types.BytesToEventStateTup(kv.Value)
		tup := types.EventStateTupWithVersion{
			Version:       version,
			EventStateTup: stateTup,
		}
		evIDs = append(evIDs, tup)
		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs, nil
}

func (e *EventsDirectory) TxnPaginateRoomEventTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
	filter *mautrix.FilterPart,
) ([]types.EventTupWithVersion, error) {
	return e.txnPaginateRoomEventIDs(
		txn,
		options,
		e.RangeForRoomVersion(roomID, options.From, options.To),
		e.KeyToRoomVersion,
		eventsProvider,
		filter,
	)
}

func (e *EventsDirectory) TxnPaginateLocalRoomEventTups(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	options types.PaginationOptions,
	eventsProvider *TxnEventsProvider,
	filter *mautrix.FilterPart,
) ([]types.EventTupWithVersion, error) {
	return e.txnPaginateRoomEventIDs(
		txn,
		options,
		e.RangeForLocalRoomVersion(roomID, options.From, options.To),
		e.KeyToLocalRoomVersion,
		eventsProvider,
		filter,
	)
}

func (e *EventsDirectory) txnPaginateRoomEventIDs(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
	keyRange fdb.Range,
	keyToVersion func(fdb.Key) tuple.Versionstamp,
	eventsProvider *TxnEventsProvider,
	filter *mautrix.FilterPart,
) ([]types.EventTupWithVersion, error) {
	iter := txn.GetRange(keyRange, options.RangeOptions()).Iterator()
	evIDs := make([]types.EventTupWithVersion, 0, options.Limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		version := keyToVersion(kv.Key)
		tup := types.BytesToEventTup(kv.Value)

		if filter != nil &&
			(len(filter.Types) > 0 && !slices.Contains(filter.Types, tup.Type) ||
				len(filter.Senders) > 0 && !slices.Contains(filter.Senders, tup.Sender) ||
				len(filter.NotTypes) > 0 && slices.Contains(filter.NotTypes, tup.Type) ||
				len(filter.NotSenders) > 0 && slices.Contains(filter.NotSenders, tup.Sender)) {
			zerolog.Ctx(context.TODO()).Trace().
				Any("tup", tup).
				Any("filter", filter).
				Msg("Filtered out pagination tup")
			continue
		}

		evIDs = append(evIDs, types.EventTupWithVersion{
			EventTup: tup,
			Version:  version,
		})

		if eventsProvider != nil {
			eventsProvider.WillGet(tup.EventID)
		}
	}

	return evIDs, nil
}
