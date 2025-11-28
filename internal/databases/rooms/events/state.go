package events

import (
	"maunium.net/go/mautrix/event"

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

var authStateTups = []types.StateTup{
	{Type: event.StateCreate, StateKey: ""},
	{Type: event.StateJoinRules, StateKey: ""},
	{Type: event.StatePowerLevels, StateKey: ""},
}

// Types of state event used for stripped state on invites
// https://spec.matrix.org/v1.11/client-server-api/#stripped-state
var strippedStateTups = []types.StateTup{
	{Type: event.StateCreate, StateKey: ""},
	{Type: event.StateRoomName, StateKey: ""},
	{Type: event.StateRoomAvatar, StateKey: ""},
	{Type: event.StateTopic, StateKey: ""},
	{Type: event.StateJoinRules, StateKey: ""},
	{Type: event.StateCanonicalAlias, StateKey: ""},
	{Type: event.StateEncryption, StateKey: ""},
}
