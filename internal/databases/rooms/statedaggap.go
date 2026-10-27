package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// A PDU of a state DAG room (MSC4242) this server is in that cites state events without state here
// is filled by the room's ingest job: phase A pulls the state DAG events missing here and stages them
// (StageStateDAGEvents) until WalkStateDAGGap finds none missing, phase B replays them
// (ReplayStateDAG, keeping them unpublished), and phase C publishes them ahead of the PDUs as
// federated events, whose preparation finds their state in place.

// StateDAGGap is what events a state DAG room's job publishes reach along prev_state_events of the
// room's state DAG not published here, see WalkStateDAGGap
type StateDAGGap struct {
	// The events stored without state, or replayed and not published yet, each with the events it
	// cites, the implicit create among them
	Cites  map[id.EventID][]id.EventID
	Events map[id.EventID]*types.Event
	// Of those, the ones replayed and not published yet
	Unpublished map[id.EventID]struct{}
	// The events not stored, or stored only as outliers
	Missing map[id.EventID]struct{}
}

// WalkStateDAGGap walks back along prev_state_events from events of a state DAG room that its job
// publishes, unstored, through the events stored without state or replayed and not published yet,
// to the published events. The events given are never walked into: those citing one publish after
// it.
func (r *RoomsDatabase) WalkStateDAGGap(ctx context.Context, roomID id.RoomID, evs []*types.Event) (*StateDAGGap, error) {
	read, err := r.renewableEventReads(ctx, roomID)
	if err != nil {
		return nil, err
	}
	var fromIDs []id.EventID
	for _, ev := range evs {
		fromIDs = append(fromIDs, ev.AuthDependencyIDs()...)
	}
	walk, err := walkUnreplayed(roomID, fromIDs, util.EventsToIDs(evs), true, read)
	if err != nil {
		return nil, err
	}
	return &StateDAGGap{Cites: walk.cites, Events: walk.evs, Unpublished: walk.unpublished, Missing: walk.missing}, nil
}

// GetRoomStateExtremEventIDs returns a state DAG room's state extremities in the order they were
// published
func (r *RoomsDatabase) GetRoomStateExtremEventIDs(ctx context.Context, roomID id.RoomID) ([]id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.EventID, error) {
		return r.events.TxnLookupCurrentRoomStateExtremEventIDs(txn, roomID), nil
	})
}
