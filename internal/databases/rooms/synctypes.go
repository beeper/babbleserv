package rooms

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type roomSyncConfig struct {
	from, to        tuple.Versionstamp
	isInitial       bool
	includeReceipts bool
}

type roomIDWithVersion struct {
	id      id.RoomID
	version tuple.Versionstamp
}

func (rv roomIDWithVersion) GetVersion() tuple.Versionstamp {
	return rv.version
}

type roomSyncResult struct {
	*roomSyncConfig

	limited        bool
	eventTups      []types.EventTupWithVersion
	eventStateTups []types.EventStateTupWithVersion

	receipts []*types.ReceiptWithVersion
}
