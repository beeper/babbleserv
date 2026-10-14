package system

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

func (s *SystemDatabase) GetServerNamesWithPositions(ctx context.Context) ([]string, error) {
	return util.DoReadTransaction(ctx, s.db, func(txn fdb.ReadTransaction) ([]string, error) {
		iter := txn.GetRange(
			s.servers.RangeForServerPositions(),
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
			serverNames = append(serverNames, s.servers.PositionKeyToServer(kv.Key))
		}

		return serverNames, nil
	})
}

func (s *SystemDatabase) GetServerPositions(ctx context.Context, serverName string) (types.VersionMap, error) {
	return util.DoReadTransaction(ctx, s.db, func(txn fdb.ReadTransaction) (types.VersionMap, error) {
		key := s.servers.KeyForServerPosition(serverName)
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

func (s *SystemDatabase) UpdateServerPositions(
	ctx context.Context,
	serverName string,
	versions types.VersionMap,
	lock lock.Lock,
) error {
	data, err := msgpack.Marshal(versions)
	if err != nil {
		return err
	}
	return lock.Transact(ctx, func(txn fdb.Transaction) error {
		txn.Set(s.servers.KeyForServerPosition(serverName), data)
		return nil
	})
}
