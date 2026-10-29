package rooms

import (
	"context"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// The room's ingest job fills the state DAG gap (MSC4242) of a PDU citing state events without state
// here: phase A stages the missing events until WalkStateDAGGap finds none, phase B replays them
// unpublished, and phase C publishes them ahead of the PDU.

type StateDAGGap struct {
	// The events stored without state or unpublished, with the events each cites
	Cites       map[id.EventID][]id.EventID
	Events      map[id.EventID]*types.Event
	Unpublished map[id.EventID]struct{}
	// Not stored, or stored only as outliers
	Missing map[id.EventID]struct{}
	// Including the events given
	Pending map[id.EventID]struct{}
}

// WalkStateDAGGap walks back from unstored events to the published state DAG, not into the events
// given or pending, as those citing one publish after it
func (r *RoomsDatabase) WalkStateDAGGap(
	ctx context.Context,
	roomID id.RoomID,
	evs []*types.Event,
	pendingIDs []id.EventID,
	keepEvents bool,
) (*StateDAGGap, error) {
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return nil, err
	}
	var fromIDs []id.EventID
	for _, ev := range evs {
		fromIDs = append(fromIDs, ev.AuthDependencyIDs()...)
	}
	walk, err := walkUnreplayed(roomID, fromIDs, slices.Concat(util.EventsToIDs(evs), pendingIDs), keepEvents, read)
	if err != nil {
		return nil, err
	}
	return &StateDAGGap{Cites: walk.cites, Events: walk.evs, Unpublished: walk.unpublished, Missing: walk.missing, Pending: walk.pending}, nil
}

// GetRoomStateExtremEventIDs returns the state extremities in the order they were published
func (r *RoomsDatabase) GetRoomStateExtremEventIDs(ctx context.Context, roomID id.RoomID) ([]id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.EventID, error) {
		return r.events.TxnLookupCurrentRoomStateExtremEventIDs(txn, roomID), nil
	})
}
