package events

import (
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Types of state event used for authorization - excluding members
// https://spec.matrix.org/v1.11/server-server-api/#auth-events-selection
var authStateTypes = []event.Type{
	event.StateCreate,
	event.StateJoinRules,
	event.StatePowerLevels,
	// Third party?
}

// Types of state event used for stripped state on invites
// https://spec.matrix.org/v1.11/client-server-api/#stripped-state
var inviteStateTypes = []event.Type{
	event.StateCreate,
	event.StateRoomName,
	event.StateRoomAvatar,
	event.StateTopic,
	event.StateJoinRules,
	event.StateCanonicalAlias,
	event.StateEncryption,
}

func filterStateMap(
	state types.StateMap,
	desiredTypes []event.Type,
	eventsProvider *TxnEventsProvider,
) types.StateMap {
	authState := make(types.StateMap, len(state))
	for _, evType := range desiredTypes {
		stateTup := types.StateTup{Type: evType}
		if evID, found := state[stateTup]; found {
			authState[stateTup] = evID
			if eventsProvider != nil {
				eventsProvider.WillGet(id.EventID(evID))
			}
		}
	}
	return authState
}
