package servers

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

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

// TxnStoreServerMembership sets a server's membership of the room on a join, clears it otherwise
func (s *ServersDirectory) TxnStoreServerMembership(txn fdb.Transaction, roomID id.RoomID, serverName string, mtup types.MembershipTup) {
	membershipKey := s.KeyForMembership(serverName, roomID)
	if mtup.Membership == event.MembershipJoin {
		txn.Set(membershipKey, types.MembershipTupToBytes(mtup))
	} else {
		txn.Clear(membershipKey)
	}
}

func (s *ServersDirectory) TxnStoreServerMembershipChange(
	txn fdb.Transaction,
	serverName string,
	version tuple.Versionstamp,
	mtup types.MembershipTup,
) {
	txn.SetVersionstampedKey(s.KeyForMembershipChange(serverName, version), types.MembershipTupToBytes(mtup))
}

// TxnReadJoinedCounts starts reading each server's joined member count of the room
func (s *ServersDirectory) TxnReadJoinedCounts(txn fdb.ReadTransaction, roomID id.RoomID, serverNames []string) map[string]fdb.FutureByteSlice {
	futures := make(map[string]fdb.FutureByteSlice, len(serverNames))
	for _, serverName := range serverNames {
		futures[serverName] = txn.Get(s.KeyForJoinedCount(roomID, serverName))
	}
	return futures
}

func JoinedCountOf(b []byte) int {
	if b == nil {
		return 0
	}
	tup, err := tuple.Unpack(b)
	if err != nil {
		panic(err)
	}
	return int(tup[0].(int64))
}

// TxnSetJoinedCount writes a server's joined member count of the room, clearing it at zero
func (s *ServersDirectory) TxnSetJoinedCount(txn fdb.Transaction, roomID id.RoomID, serverName string, count int) {
	key := s.KeyForJoinedCount(roomID, serverName)
	if count > 0 {
		txn.Set(key, tuple.Tuple{int64(count)}.Pack())
	} else {
		txn.Clear(key)
	}
}

// TxnLookupRoomServers returns the servers with joined members in the room
func (s *ServersDirectory) TxnLookupRoomServers(txn fdb.ReadTransaction, roomID id.RoomID) ([]string, error) {
	kvs, err := txn.GetRange(s.rangeForJoinedCounts(roomID), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	serverNames := make([]string, len(kvs))
	for i, kv := range kvs {
		tup, err := s.joinedCounts.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		serverNames[i] = tup[1].(string)
	}
	return serverNames, nil
}
