package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (s *ServersDirectory) TxnIsServerInRoom(
	txn fdb.ReadTransaction,
	serverName string,
	roomID id.RoomID,
) (bool, error) {
	if value, err := txn.Get(s.KeyForServerMembership(serverName, roomID)).Get(); err != nil {
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
		s.RangeForServerMemberships(serverName),
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

func (s *ServersDirectory) TxnLookupServerMembershipChanges(
	txn fdb.ReadTransaction,
	serverName string,
	fromVersion, toVersion tuple.Versionstamp,
) (types.MembershipChanges, error) {
	return nil, nil
}
