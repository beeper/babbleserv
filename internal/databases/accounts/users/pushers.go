package users

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules/pushgateway"
)

func (u *UsersDirectory) keyForUserPusher(userID id.UserID, appID pushgateway.PusherAppID, pushKey string) fdb.Key {
	return u.userPushers.Pack(tuple.Tuple{userID.String(), string(appID), pushKey})
}

func (u *UsersDirectory) keyForUserPusherDevice(userID id.UserID, appID pushgateway.PusherAppID, pushKey string) fdb.Key {
	return u.userPusherDevices.Pack(tuple.Tuple{userID.String(), string(appID), pushKey})
}

func (u *UsersDirectory) keyForPusherIdentityUser(appID pushgateway.PusherAppID, pushKey string, userID id.UserID) fdb.Key {
	return u.pusherUsersByIdentity.Pack(tuple.Tuple{string(appID), pushKey, userID.String()})
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

func (u *UsersDirectory) TxnSetPusherForUser(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	pusher *pushgateway.Pusher,
	appendPusher bool,
) error {
	value, err := json.Marshal(pusher)
	if err != nil {
		return err
	}
	if !appendPusher {
		iter := txn.GetRange(
			u.pusherUsersByIdentity.Sub(string(pusher.AppID), pusher.PushKey),
			fdb.RangeOptions{Mode: fdb.StreamingModeWantAll},
		).Iterator()
		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return err
			}
			tup, err := u.pusherUsersByIdentity.Unpack(kv.Key)
			if err != nil {
				return err
			}
			otherUserID := id.UserID(tup[2].(string))
			if otherUserID != userID {
				u.TxnDeletePusherForUser(txn, otherUserID, pusher.AppID, pusher.PushKey)
			}
		}
	}
	txn.Set(u.keyForUserPusher(userID, pusher.AppID, pusher.PushKey), value)
	txn.Set(u.keyForUserPusherDevice(userID, pusher.AppID, pusher.PushKey), []byte(deviceID))
	txn.Set(u.keyForPusherIdentityUser(pusher.AppID, pusher.PushKey, userID), nil)
	return nil
}

func (u *UsersDirectory) TxnDeletePusherForUser(txn fdb.Transaction, userID id.UserID, appID pushgateway.PusherAppID, pushKey string) {
	txn.Clear(u.keyForUserPusher(userID, appID, pushKey))
	txn.Clear(u.keyForUserPusherDevice(userID, appID, pushKey))
	txn.Clear(u.keyForPusherIdentityUser(appID, pushKey, userID))
}
