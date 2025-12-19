package system

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (s *SystemDatabase) KeyForIteratorPositions(iterator string) fdb.Key {
	return s.iteratorPositions.Pack(tuple.Tuple{iterator})
}

func (s *SystemDatabase) GetAllIteratorPositions(ctx context.Context) (map[string]tuple.Versionstamp, error) {
	return util.DoReadTransaction(ctx, s.db, func(txn fdb.ReadTransaction) (map[string]tuple.Versionstamp, error) {
		kvs := txn.GetRange(s.iteratorPositions, fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		}).GetSliceOrPanic()
		positions := make(map[string]tuple.Versionstamp, len(kvs))
		for _, kv := range kvs {
			tup, _ := s.iteratorPositions.Unpack(kv.Key)
			positions[tup[0].(string)] = types.MustBytesToVersionstamp(kv.Value)
		}
		return positions, nil
	})
}

func (s *SystemDatabase) GetIteratorPositions(ctx context.Context, key string) (tuple.Versionstamp, error) {
	return util.DoReadTransaction(ctx, s.db, func(txn fdb.ReadTransaction) (tuple.Versionstamp, error) {
		key := s.KeyForIteratorPositions(key)
		val, err := txn.Get(key).Get()
		if err != nil {
			return types.ZeroVersionstamp, err
		} else if val == nil {
			return types.ZeroVersionstamp, nil
		}
		return types.BytesToVersionstamp(val)
	})
}

func (s *SystemDatabase) UpdateIteratorPositions(
	ctx context.Context,
	key string,
	version tuple.Versionstamp,
	checkUpdateLock func(fdb.Transaction),
) error {
	_, err := util.DoWriteTransaction(ctx, s.db, func(txn fdb.Transaction) (*struct{}, error) {
		// Ensure that our lock is still valid before writing data
		checkUpdateLock(txn)

		key := s.KeyForIteratorPositions(key)
		txn.Set(key, types.MustVersionstampToBytes(version))
		return nil, nil
	})
	return err
}
