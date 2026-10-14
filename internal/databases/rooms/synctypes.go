package rooms

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
)

type roomSyncConfig struct {
	from, to        tuple.Versionstamp
	isInitial       bool
	includeReceipts bool
	// The version of the membership change the room is synced for, zero for its current membership
	changedAt tuple.Versionstamp
	// Of a server's sync, what decides the room's range but the range itself
	server serverRoomSync
}

// serverRoomSync is what a server's sync of a room is decided by
type serverRoomSync struct {
	since, to tuple.Versionstamp
	// The server's join the room is synced for within the sync, zero for none
	serverJoined tuple.Versionstamp
	// Each server's membership changes of the room from since through latestVersion, and whether
	// it was joined at latestVersion, all based on the initial membership snapshot.
	serverChanges   types.MembershipChanges
	serverJoinedNow bool
	localChanges    types.MembershipChanges
	localJoinedNow  bool
}

type roomSyncResult struct {
	*roomSyncConfig

	limited        bool
	eventTups      []types.EventTupWithVersion
	eventStateTups []types.EventStateTupWithVersion
	// The last event a streaming page read when it may have stopped short of to
	pageEnd tuple.Versionstamp
	// The end of a server's joined interval when later intervals have not been read
	intervalEnd tuple.Versionstamp

	receipts []*types.ReceiptWithVersion
	// Left out of a server's sync, see applyServerSyncRange
	denied bool
}
