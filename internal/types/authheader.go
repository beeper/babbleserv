package types

import "maunium.net/go/mautrix/id"

// AuthHeader is graph metadata independent of an event's authorization outcome: the event's auth
// events and, once finalized, its chain position, zero while it is not.
type AuthHeader struct {
	AuthEventIDs []id.EventID
	Chain        uint32
	Sequence     uint32
}

func (h AuthHeader) Finalized() bool {
	return h.Chain != 0
}
