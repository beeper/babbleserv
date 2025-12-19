package rooms

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
)

type roomSyncConfig struct {
	from, to        tuple.Versionstamp
	isInitial       bool
	includeReceipts bool
}

type roomSyncResult struct {
	*roomSyncConfig

	limited        bool
	eventTups      []types.EventTupWithVersion
	eventStateTups []types.EventStateTupWithVersion

	receipts []*types.ReceiptWithVersion
}
