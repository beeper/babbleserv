package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) TxnDeleteUserMembership(txn fdb.Transaction, userID id.UserID, roomID id.RoomID, version tuple.Versionstamp) {
	txn.Clear(u.keyForMembership(userID, roomID))
	txn.Clear(u.keyForMembershipChange(userID, version))
}

// Memberships (id.UserID, id.RoomID) -> types.MembershipTup
//

func (u *UsersDirectory) keyForMembership(userID id.UserID, roomID id.RoomID) fdb.Key {
	return u.memberships.Pack(tuple.Tuple{userID.String(), roomID.String()})
}

func (u *UsersDirectory) rangeForMemberships(userID id.UserID) fdb.Range {
	return u.memberships.Sub(userID.String())
}

func (u *UsersDirectory) TxnStoreMembership(txn fdb.Transaction, userID id.UserID, roomID id.RoomID, tup types.MembershipTup) {
	txn.Set(u.keyForMembership(userID, roomID), types.MembershipTupToBytes(tup))
}

func (u *UsersDirectory) TxnGetMembership(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) *types.MembershipTup {
	if b := txn.Get(u.keyForMembership(userID, roomID)).MustGet(); b == nil {
		return nil
	} else {
		tup := types.BytesToMembershipTup(b)
		return &tup
	}
}

func (u *UsersDirectory) TxnIsUserJoinedRoom(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
) bool {
	mtup := u.TxnGetMembership(txn, userID, roomID)
	if mtup == nil {
		return false
	}
	return mtup.Membership == event.MembershipJoin
}

func (u *UsersDirectory) TxnLookupUserMemberships(
	txn fdb.ReadTransaction,
	userID id.UserID,
) types.Memberships {
	iter := txn.GetRange(
		u.rangeForMemberships(userID),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	memberships := make(types.Memberships, 10)
	for iter.Advance() {
		kv := iter.MustGet()
		membershipTup := types.BytesToMembershipTup(kv.Value)
		memberships[membershipTup.RoomID] = membershipTup
	}

	return memberships
}

// Membership changes (id.UserID, tuple.Versionstamp) -> types.MembershipTup
//

func (u *UsersDirectory) keyToMembershipChangeVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := u.membershipChanges.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (u *UsersDirectory) keyForMembershipChange(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	key, err := u.membershipChanges.PackWithVersionstamp(tuple.Tuple{
		userID.String(), version,
	})
	if err != nil {
		panic(err)
	}
	return key
}

func (u *UsersDirectory) rangeForMembershipChanges(
	userID id.UserID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(u.membershipChanges, fromVersion, toVersion, userID.String())
}

func (u *UsersDirectory) TxnStoreMembershipChange(
	txn fdb.Transaction,
	userID id.UserID,
	version tuple.Versionstamp,
	tup types.MembershipTup,
) {
	txn.SetVersionstampedKey(u.keyForMembershipChange(userID, version), types.MembershipTupToBytes(tup))
}

func (u *UsersDirectory) TxnLookupUserMembershipChanges(
	txn fdb.ReadTransaction,
	userID id.UserID,
	options types.PaginationOptions,
) types.MembershipChanges {
	iter := txn.GetRange(
		u.rangeForMembershipChanges(userID, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	changes := make(types.MembershipChanges, 0)
	for iter.Advance() {
		kv := iter.MustGet()
		membershipTup := types.BytesToMembershipTup(kv.Value)
		version := u.keyToMembershipChangeVersion(kv.Key)
		changes = append(changes, types.MembershipTupWithVersion{
			MembershipTup: membershipTup,
			Version:       version,
		})
	}

	return changes
}
