// Package stateres resolves room state across several inputs with the Matrix state resolution
// algorithm, version 2 or 2.1 as the room version gives it:
// https://spec.matrix.org/v1.14/rooms/v2/#state-resolution
package stateres

import (
	"errors"
	"fmt"

	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var ErrUnsupportedAlgorithm = errors.New("unsupported state resolution algorithm")

// Version identifies the results of this implementation, which are stored. Bump it with any change
// that can change a result, so results stored before it are not used.
const Version = 2

// CheckAlgorithm fails with ErrUnsupportedAlgorithm for a room version resolving state with an
// algorithm before state resolution v2.
func CheckAlgorithm(roomVersion string) error {
	ver, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	if err != nil {
		return err
	}
	if ver.StateResAlgorithm() != gomatrixserverlib.StateResV2 && ver.StateResAlgorithm() != gomatrixserverlib.StateResV2_1 {
		return fmt.Errorf("%w: room version %s", ErrUnsupportedAlgorithm, roomVersion)
	}
	return nil
}

// GetEventFunc must return nil, nil for unknown events.
type GetEventFunc func(id.EventID) (*types.Event, error)

// checkRoomVersion keeps an event authorized under another room version from being counted as
// rejected, which is how gomatrixserverlib.Allowed would report it.
func (r *resolver) checkRoomVersion(ev *types.Event) error {
	if ev.RoomVersion != r.roomVersion {
		return fmt.Errorf("event %s has room version %q, resolving for %q", ev.ID, ev.RoomVersion, r.roomVersion)
	}
	return nil
}

type eventCache struct {
	getEvent GetEventFunc
	events   map[id.EventID]*types.Event
}

func (c *eventCache) get(eventID id.EventID) (*types.Event, error) {
	if ev, ok := c.events[eventID]; ok {
		return ev, nil
	}
	ev, err := c.getEvent(eventID)
	if err != nil {
		return nil, fmt.Errorf("failed to get event %s: %w", eventID, err)
	}
	c.events[eventID] = ev
	return ev, nil
}
