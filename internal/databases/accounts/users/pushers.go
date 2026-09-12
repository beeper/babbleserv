package users

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules/pushgateway"
)

func (u *UsersDirectory) keyForUserPusher(userID id.UserID, pushKey string) fdb.Key {
	return u.userPushers.Pack(tuple.Tuple{userID.String(), pushKey})
}

func (u *UsersDirectory) RangeForUserPushers(userID id.UserID) fdb.ExactRange {
	return u.userPushers.Sub(userID.String())
}

func (u *UsersDirectory) TxnGetPushersForUser(txn fdb.ReadTransaction, userID id.UserID) ([]pushgateway.Pusher, error) {
	iter := txn.GetRange(
		u.RangeForUserPushers(userID),
		fdb.RangeOptions{Mode: fdb.StreamingModeWantAll},
	).Iterator()

	pushers := make([]pushgateway.Pusher, 0)
	for iter.Advance() {
		kv := iter.MustGet()
		var pusher pushgateway.Pusher
		if err := json.Unmarshal(kv.Value, &pusher); err != nil {
			return nil, err
		}
		pushers = append(pushers, pusher)
	}
	return pushers, nil
}

func (u *UsersDirectory) TxnSetPusherForUser(txn fdb.Transaction, userID id.UserID, pusher *pushgateway.Pusher) error {
	key := u.keyForUserPusher(userID, pusher.PushKey)
	value, err := json.Marshal(pusher)
	if err != nil {
		return err
	}
	txn.Set(key, value)
	return nil
}

func (u *UsersDirectory) TxnDeletePusherForUser(txn fdb.Transaction, userID id.UserID, pushKey string) {
	key := u.keyForUserPusher(userID, pushKey)
	txn.Clear(key)
}
