package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (s *ServersDirectory) TxnGetMembership(
	txn fdb.ReadTransaction,
	serverName string,
	roomID id.RoomID,
) *types.MembershipTup {
	if b := txn.Get(s.keyForMembership(serverName, roomID)).MustGet(); b == nil {
		return nil
	} else {
		tup := types.BytesToMembershipTup(b)
		return &tup
	}
}

func (s *ServersDirectory) TxnIsServerJoinedRoom(
	txn fdb.ReadTransaction,
	serverName string,
	roomID id.RoomID,
) bool {
	mtup := s.TxnGetMembership(txn, serverName, roomID)
	if mtup == nil {
		return false
	}
	return mtup.Membership == event.MembershipJoin
}

func (s *ServersDirectory) TxnLookupServerMemberships(
	txn fdb.ReadTransaction,
	serverName string,
) (types.Memberships, error) {
	iter := txn.GetRange(
		s.rangeForMemberships(serverName),
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

func (s *ServersDirectory) TxnLookupServerMembershipChanges(
	txn fdb.ReadTransaction,
	serverName string,
	options types.PaginationOptions,
) (types.MembershipChanges, error) {
	iter := txn.GetRange(
		s.rangeForMembershipChanges(serverName, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	changes := make(types.MembershipChanges, 0)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		membershipTup := types.BytesToMembershipTup(kv.Value)
		version := s.keyToMembershipChangeVersion(kv.Key)
		changes = append(changes, types.MembershipTupWithVersion{
			MembershipTup: membershipTup,
			Version:       version,
		})
	}

	return changes, nil
}

func (s *ServersDirectory) TxnStoreServerMembership(
	txn fdb.Transaction,
	roomID id.RoomID,
	serverName string,
	mtup types.MembershipTup,
	version tuple.Versionstamp,
) {
	mtupBytes := types.MembershipTupToBytes(mtup)

	txn.SetVersionstampedKey(s.keyForMembershipChange(serverName, version), mtupBytes)

	membershipKey := s.keyForMembership(serverName, roomID)
	if mtup.Membership == event.MembershipJoin {
		txn.Set(membershipKey, mtupBytes)
	} else {
		txn.Clear(membershipKey)
	}
}

func (s *ServersDirectory) TxnDeleteServerMembership(
	txn fdb.Transaction,
	roomID id.RoomID,
	serverName string,
	version tuple.Versionstamp,
) {
	txn.Clear(s.keyForMembership(serverName, roomID))
	txn.Clear(s.keyForMembershipChange(serverName, version))
}
