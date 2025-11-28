package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (s *ServersDirectory) TxnIsServerInRoom(
	txn fdb.ReadTransaction,
	serverName string,
	roomID id.RoomID,
) (bool, error) {
	if value, err := txn.Get(s.KeyForMembership(serverName, roomID)).Get(); err != nil {
		return false, err
	} else {
		return value != nil, nil
	}
}

func (s *ServersDirectory) TxnMustIsServerInRoom(
	txn fdb.ReadTransaction,
	serverName string,
	roomID id.RoomID,
) bool {
	if ret, err := s.TxnIsServerInRoom(txn, serverName, roomID); err != nil {
		panic(err)
	} else {
		return ret
	}
}

func (s *ServersDirectory) TxnLookupServerMemberships(
	txn fdb.ReadTransaction,
	serverName string,
) (types.Memberships, error) {
	iter := txn.GetRange(
		s.RangeForMemberships(serverName),
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
		s.RangeForMembershipChanges(serverName, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	changes := make(types.MembershipChanges, 0)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		membershipTup := types.BytesToMembershipTup(kv.Value)
		version := s.KeyToMembershipChangeVersion(kv.Key)
		changes = append(changes, types.MembershipTupWithVersion{
			MembershipTup: membershipTup,
			Version:       version,
		})
	}

	return changes, nil
}
