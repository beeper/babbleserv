package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) keyForProfile(userID id.UserID) fdb.Key {
	return u.userProfiles.Pack(tuple.Tuple{userID.String()})
}

func (u *UsersDirectory) TxnGetUserProfile(txn fdb.ReadTransaction, userID id.UserID) (*types.UserProfile, error) {
	key := u.keyForProfile(userID)

	b, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	} else if b == nil {
		return nil, nil
	}

	return types.NewUserProfileFromBytes(b)
}

func (u *UsersDirectory) TxnStoreUserProfile(txn fdb.Transaction, userID id.UserID, profile *types.UserProfile) {
	txn.Set(u.keyForProfile(userID), profile.ToMsgpack())
}

func (u *UsersDirectory) keyForProfileChange(version tuple.Versionstamp) fdb.Key {
	key, err := u.profileChanges.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) TxnStoreProfileChange(txn fdb.Transaction, userID id.UserID, profile *types.UserProfile, version tuple.Versionstamp) {
	txn.SetVersionstampedKey(
		u.keyForProfileChange(version),
		tuple.Tuple{userID.String(), profile.ToMsgpack()}.Pack(),
	)
}

// Remove any profile changes from zero through to and including toVersion
func (u *UsersDirectory) TxnClearProfileChanges(txn fdb.Transaction, toVersion tuple.Versionstamp) {
	txn.ClearRange(types.GetVersionRange(u.profileChanges, types.ZeroVersionstamp, toVersion))
}

func (u *UsersDirectory) TxnPaginateProfileChanges(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
) ([]types.UserProfileChange, error) {
	iter := txn.GetRange(
		types.GetVersionRange(u.profileChanges, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	ids := make([]types.UserProfileChange, 0, options.Limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		keyTup, _ := u.profileChanges.Unpack(kv.Key)
		valueTup, _ := tuple.Unpack(kv.Value)
		ids = append(ids, types.UserProfileChange{
			Version: keyTup[0].(tuple.Versionstamp),
			UserID:  id.UserID(valueTup[0].(string)),
			Profile: types.MustNewUserProfileFromBytes(valueTup[1].([]byte)),
		})
	}

	return ids, nil
}
