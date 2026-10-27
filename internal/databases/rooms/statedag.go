package rooms

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Until the federated pipeline and joins handle state DAG rooms (MSC4242), only their local events
// are sent
var ErrStateDAGFederation = errors.New("federation in state DAG rooms is not supported yet")

// refuseStateDAGFederation answers a federated send to a state DAG room, by the version of the room,
// nil when not stored, or the version the events were verified with: a send with given states fails,
// and while this server is joined every event is dropped. A room this server is not in keeps its
// outlier memberships as other versions do. Returns nil for any other room.
func (r *RoomsDatabase) refuseStateDAGFederation(
	ctx context.Context,
	room *types.Room,
	roomID id.RoomID,
	evs []*types.Event,
	given *GivenStates,
	options SendFederatedEventsOptions,
) (*SendEventsResult, error) {
	storedStateDAGs := room != nil && util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs)
	if !storedStateDAGs && !slices.ContainsFunc(evs, (*types.Event).StateDAGs) {
		return nil, nil
	} else if given != nil {
		return nil, fmt.Errorf("%w: %s", ErrStateDAGFederation, roomID)
	} else if !isServerJoined(room) {
		return r.sendOutlierMemberships(ctx, roomID, evs, options)
	}
	res := &SendEventsResult{}
	for _, ev := range evs {
		res.Rejected = append(res.Rejected, RejectedEvent{ev, fmt.Errorf("%w: %w", ErrEventDropped, ErrStateDAGFederation)})
	}
	return res, nil
}

// entersStateDAG reports whether a published event of a state DAG room becomes a state extremity:
// an accepted state event does, in place of the prev state events it cites
func entersStateDAG(ev *types.Event) bool {
	return ev.StateKey != nil && !ev.Rejected && !ev.SoftFailed
}

// advanceStateExtremities returns a state DAG room's state extremities once an event publishes,
// the change its publish writes to events/srx
func advanceStateExtremities(extremities []id.EventID, ev *types.Event) []id.EventID {
	if !entersStateDAG(ev) {
		return extremities
	}
	return advanceExtremities(extremities, ev.PrevStateEventIDs, ev.ID)
}

// citedExtremities returns the extremities a local event of a state DAG room cites, of extremities
// in the order they were stored: all of them up to types.MaxStateDAGPrevEvents, otherwise that many
// stored last. Depth would let a sender choose.
func citedExtremities(extremities []id.EventID) []id.EventID {
	return append([]id.EventID{}, extremities[max(len(extremities)-types.MaxStateDAGPrevEvents, 0):]...)
}
