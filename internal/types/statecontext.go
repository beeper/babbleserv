package types

import (
	"encoding/hex"
	"encoding/json"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// StateHash identifies an immutable state context or state map page: the first 16 bytes of the
// SHA-256 of its encoding. The zero hash is the root of an empty map, and as a context ID means the
// context is unknown.
type StateHash [16]byte

func (h StateHash) IsZero() bool {
	return h == StateHash{}
}

// MarshalJSON renders the hash as hex. There is deliberately no MarshalText: msgpack prefers it and
// would store the hash as a hex string instead of 16 bytes.
func (h StateHash) MarshalJSON() ([]byte, error) {
	return json.Marshal(hex.EncodeToString(h[:]))
}

// StateChange describes a change bwtween two contexts for a given state tup (type, state_key) pair
type StateChange struct {
	StateTup
	OldEventID    id.EventID
	NewEventID    id.EventID
	OldMembership event.Membership
	NewMembership event.Membership
}

type StateEntry struct {
	EventID    id.EventID
	Membership event.Membership
}

type StateEntries map[StateTup]StateEntry

func (s StateEntries) EventIDs() StateMap {
	stateMap := make(StateMap, len(s))
	for tup, entry := range s {
		stateMap[tup] = entry.EventID
	}
	return stateMap
}
