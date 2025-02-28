package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const eventsIteratorPositionKey = "EventsIteratorPosition"

func (r *RoomsDatabase) GetEventsIteratorPosition(ctx context.Context) (tuple.Versionstamp, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (tuple.Versionstamp, error) {
		key := r.root.Pack(tuple.Tuple{eventsIteratorPositionKey})
		val, err := txn.Get(key).Get()
		if err != nil {
			return types.ZeroVersionstamp, err
		} else if val == nil {
			return types.ZeroVersionstamp, nil
		}
		return types.ValueToVersionstamp(val)
	})
}

func (r *RoomsDatabase) UpdateEventsIteratorPosition(
	ctx context.Context,
	version tuple.Versionstamp,
	checkUpdateLock func(fdb.Transaction),
) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*struct{}, error) {
		// Ensure that our lock is still valid before writing data
		checkUpdateLock(txn)

		key := r.root.Pack(tuple.Tuple{eventsIteratorPositionKey})
		txn.Set(key, types.VersionstampToValue(version))
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) EventsIteratorPaginateEvents(
	ctx context.Context,
	fromVersion tuple.Versionstamp,
	limit int,
) ([]types.EventIDTupWithVersion, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]types.EventIDTupWithVersion, error) {
		return r.events.TxnPaginateAllEventRoomIDTups(txn, fromVersion, types.ZeroVersionstamp, limit, nil)
	})
}
