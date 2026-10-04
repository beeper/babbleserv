package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Memberships of local users (id.UserID, id.RoomID) -> types.MembershipRow, cleared when the user's
// member key leaves the room's state
//

func (u *UsersDirectory) KeyForMembership(userID id.UserID, roomID id.RoomID) fdb.Key {
	return u.memberships.Pack(tuple.Tuple{userID.String(), roomID.String()})
}

func (u *UsersDirectory) rangeForMemberships(userID id.UserID) fdb.Range {
	return u.memberships.Sub(userID.String())
}

func (u *UsersDirectory) TxnStoreMembership(txn fdb.Transaction, userID id.UserID, roomID id.RoomID, row types.MembershipRow) {
	txn.Set(u.KeyForMembership(userID, roomID), types.MembershipRowToBytes(row))
}

func (u *UsersDirectory) TxnDeleteMembership(txn fdb.Transaction, userID id.UserID, roomID id.RoomID) {
	txn.Clear(u.KeyForMembership(userID, roomID))
}

func membershipRowOf(b []byte) *types.MembershipRow {
	if b == nil {
		return nil
	}
	row := types.BytesToMembershipRow(b)
	return &row
}

// MembershipRowsOf waits for reads of users' rows, nil for none
func MembershipRowsOf[K comparable](futures map[K]fdb.FutureByteSlice) (map[K]*types.MembershipRow, error) {
	rows := make(map[K]*types.MembershipRow, len(futures))
	for key, future := range futures {
		b, err := future.Get()
		if err != nil {
			return nil, err
		}
		rows[key] = membershipRowOf(b)
	}
	return rows, nil
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

func (u *UsersDirectory) KeyForMembershipChange(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(u.membershipChanges, tuple.Tuple{userID.String(), version})
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
	txn.SetVersionstampedKey(u.KeyForMembershipChange(userID, version), types.MembershipTupToBytes(tup))
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

// TxnLookupUserMembershipRows returns the memberships of at most limit current-membership rows,
// including terminal memberships, and reports whether the range had more rows.
// It is used by privacy-sensitive user-directory filtering, where silently
// skipping leave rows would make the transaction's scan bound ineffective.
func (u *UsersDirectory) TxnLookupUserMembershipRows(
	txn fdb.ReadTransaction,
	userID id.UserID,
	limit int,
) ([]types.MembershipTup, bool) {
	if limit <= 0 {
		return []types.MembershipTup{}, true
	}
	kvs := txn.GetRange(u.rangeForMemberships(userID), fdb.RangeOptions{
		Limit: limit + 1,
		Mode:  fdb.StreamingModeExact,
	}).GetSliceOrPanic()
	more := len(kvs) > limit
	if more {
		kvs = kvs[:limit]
	}
	memberships := make([]types.MembershipTup, 0, len(kvs))
	for _, kv := range kvs {
		memberships = append(memberships, types.BytesToMembershipTup(kv.Value))
	}
	return memberships, more
}
