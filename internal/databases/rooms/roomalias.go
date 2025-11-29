package rooms

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) GetAliasesForRoom(ctx context.Context, roomID id.RoomID) ([]id.RoomAlias, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.RoomAlias, error) {
		iter := txn.GetRange(
			r.RangeForIDAliases(roomID),
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		ids := make([]id.RoomAlias, 0)
		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			alias := r.IDAliasKeyToRoomAlias(kv.Key)
			ids = append(ids, alias)
		}

		return ids, nil
	})
}

func (r *RoomsDatabase) GetRoomAlias(ctx context.Context, alias id.RoomAlias) (id.RoomID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (id.RoomID, error) {
		key := r.KeyForRoomAlias(alias)
		b := txn.Get(key).MustGet()
		if b == nil {
			return "", nil
		}
		tup, err := tuple.Unpack(b)
		if err != nil {
			return "", err
		}
		roomID := id.RoomID(tup[0].(string))
		return roomID, nil
	})
}

func (r *RoomsDatabase) SetRoomAlias(ctx context.Context, alias id.RoomAlias, roomID id.RoomID, ownerID id.UserID) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*struct{}, error) {
		key := r.KeyForRoomAlias(alias)
		b := txn.Get(key).MustGet()
		if b != nil {
			return nil, types.ErrRoomAliasTaken
		}
		txn.Set(r.KeyForIdAlias(roomID, alias), []byte{})
		txn.Set(key, tuple.Tuple{roomID.String(), ownerID.String()}.Pack())
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) DeleteRoomAlias(ctx context.Context, alias id.RoomAlias, ownerID id.UserID) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*struct{}, error) {
		key := r.KeyForRoomAlias(alias)
		b := txn.Get(key).MustGet()
		if b == nil {
			return nil, types.ErrRoomAliasNotFound
		}
		tup, err := tuple.Unpack(b)
		if err != nil {
			return nil, err
		}
		roomID := id.RoomID(tup[0].(string))
		actualOwnerID := id.UserID(tup[1].(string))
		if ownerID != actualOwnerID {
			return nil, fmt.Errorf("cannot delete another users alias")
		}
		txn.Clear(r.KeyForIdAlias(roomID, alias))
		txn.Clear(key)
		return nil, nil
	})
	return err
}
