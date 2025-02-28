package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/vmihailenco/msgpack/v5"
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

func (r *RoomsDatabase) GetServerNamesWithPositions(ctx context.Context) ([]string, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]string, error) {
		iter := txn.GetRange(
			r.servers.RangeForServerPositions(),
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		serverNames := make([]string, 0)

		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			serverNames = append(serverNames, r.servers.PositionKeyToServer(kv.Key))
		}

		return serverNames, nil
	})
}

func (r *RoomsDatabase) GetServerPositions(ctx context.Context, serverName string) (types.VersionMap, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.VersionMap, error) {
		key := r.servers.KeyForServerPosition(serverName)
		b, err := txn.Get(key).Get()
		if err != nil {
			return nil, err
		} else if b == nil {
			return nil, nil
		}
		var versions types.VersionMap
		if err := msgpack.Unmarshal(b, &versions); err != nil {
			return nil, err
		}
		return versions, nil
	})
}

func (r *RoomsDatabase) UpdateServerPositions(
	ctx context.Context,
	serverName string,
	versions types.VersionMap,
	checkUpdateLock func(fdb.Transaction),
) error {
	data, err := msgpack.Marshal(versions)
	if err != nil {
		return err
	}
	_, err = util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*struct{}, error) {
		// Ensure lock is still valid before writing data
		checkUpdateLock(txn)

		key := r.servers.KeyForServerPosition(serverName)
		txn.Set(key, data)
		return nil, nil
	})
	return err
}
