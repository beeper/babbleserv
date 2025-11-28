package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) TxnIsUserJoinedRoom(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) (bool, error) {
	value, err := txn.Get(u.KeyForMembership(userID, roomID)).Get()
	if err != nil {
		return false, err
	} else if value == nil {
		return false, nil
	}
	membershipTup := types.BytesToMembershipTup(value)
	return membershipTup.Membership == event.MembershipJoin, nil
}

func (u *UsersDirectory) TxnMustIsUserJoinedRoom(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) bool {
	if ret, err := u.TxnIsUserJoinedRoom(txn, userID, roomID); err != nil {
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
		u.RangeForMemberships(userID),
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
		membershipTup := types.BytesToMembershipTup(kv.Value)
		memberships[membershipTup.RoomID] = membershipTup
	}

	return memberships, nil
}

func (u *UsersDirectory) TxnLookupUserOutlierMemberships(
	txn fdb.ReadTransaction,
	userID id.UserID,
) (types.Memberships, error) {
	iter := txn.GetRange(
		u.RangeForOutlierMemberships(userID),
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
		membershipTup := types.BytesToMembershipTup(kv.Value)
		memberships[membershipTup.RoomID] = membershipTup
	}

	return memberships, nil
}

func (u *UsersDirectory) TxnLookupUserMembershipChanges(
	txn fdb.ReadTransaction,
	userID id.UserID,
	options types.PaginationOptions,
) (types.MembershipChanges, error) {
	iter := txn.GetRange(
		u.RangeForMembershipChanges(userID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	changes := make(types.MembershipChanges, 0)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		membershipTup := types.BytesToMembershipTup(kv.Value)
		version := u.KeyToMembershipChangeVersion(kv.Key)
		changes = append(changes, types.MembershipTupWithVersion{
			MembershipTup: membershipTup,
			Version:       version,
		})
	}

	return changes, nil
}
