package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) TxnIsUserInRoom(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) (bool, error) {
	value, err := txn.Get(u.KeyForUserMembership(userID, roomID)).Get()
	if err != nil {
		return false, err
	} else if value == nil {
		return false, nil
	}
	membershipTup := types.ValueToMembershipTup(value)
	return membershipTup.Membership == event.MembershipJoin, nil
}

func (u *UsersDirectory) TxnMustIsUserInRoom(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) bool {
	if ret, err := u.TxnIsUserInRoom(txn, userID, roomID); err != nil {
		panic(err)
	} else {
		return ret
	}
}

func (u *UsersDirectory) TxnLookupUserMemberships(
	txn fdb.ReadTransaction,
	userID id.UserID,
) (types.Memberships, error) {
	iter := txn.GetRange(
		u.RangeForUserMemberships(userID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	memberships := make(types.Memberships, 10)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		membershipTup := types.ValueToMembershipTup(kv.Value)
		memberships[membershipTup.RoomID] = membershipTup
	}

	return memberships, nil
}

func (u *UsersDirectory) TxnLookupUserOutlierMemberships(
	txn fdb.ReadTransaction,
	userID id.UserID,
) (types.Memberships, error) {
	iter := txn.GetRange(
		u.RangeForUserOutlierMemberships(userID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	memberships := make(types.Memberships)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		membershipTup := types.ValueToMembershipTup(kv.Value)
		memberships[membershipTup.RoomID] = membershipTup
	}

	return memberships, nil
}

func (u *UsersDirectory) TxnLookupUserMembershipChanges(
	txn fdb.ReadTransaction,
	userID id.UserID,
	fromVersion, toVersion tuple.Versionstamp,
) (types.MembershipChanges, error) {
	return nil, nil
}
