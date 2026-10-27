package rooms

import (
	"context"
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util"
)

var ErrBotRoomExists = errors.New("the user has a BabbleBot room already")

func (r *RoomsDatabase) keyForBotRoom(userID id.UserID) fdb.Key {
	return r.botRooms.Pack(tuple.Tuple{userID.String()})
}

func (r *RoomsDatabase) GetBotRoom(ctx context.Context, userID id.UserID) (id.RoomID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (id.RoomID, error) {
		b, err := txn.Get(r.keyForBotRoom(userID)).Get()
		return id.RoomID(b), err
	})
}

// txnClaimBotRoom fails with ErrBotRoomExists when another send created the user's room first
func (r *RoomsDatabase) txnClaimBotRoom(txn fdb.Transaction, userID id.UserID, roomID id.RoomID) error {
	if b, err := txn.Get(r.keyForBotRoom(userID)).Get(); err != nil {
		return err
	} else if b != nil && id.RoomID(b) != roomID {
		return fmt.Errorf("%w: %s", ErrBotRoomExists, b)
	}
	txn.Set(r.keyForBotRoom(userID), []byte(roomID))
	return nil
}
