package util

import (
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// AuthEventLookup returns the event for a tuple of the state an event is authorized against, or
// nil if that state has none.
type AuthEventLookup func(types.StateTup) (*types.Event, error)

var _ gomatrixserverlib.AuthEventProvider = AuthEventLookup(nil)

func (lookup AuthEventLookup) pdu(tup types.StateTup) (gomatrixserverlib.PDU, error) {
	ev, err := lookup(tup)
	if ev == nil || err != nil {
		return nil, err
	}
	return ev.PDU(), nil
}

func (lookup AuthEventLookup) Create() (gomatrixserverlib.PDU, error) {
	return lookup.pdu(types.StateTup{Type: event.StateCreate})
}

func (lookup AuthEventLookup) JoinRules() (gomatrixserverlib.PDU, error) {
	return lookup.pdu(types.StateTup{Type: event.StateJoinRules})
}

func (lookup AuthEventLookup) PowerLevels() (gomatrixserverlib.PDU, error) {
	return lookup.pdu(types.StateTup{Type: event.StatePowerLevels})
}

func (lookup AuthEventLookup) Member(stateKey spec.SenderID) (gomatrixserverlib.PDU, error) {
	return lookup.pdu(types.MemberStateTup(id.UserID(stateKey)))
}

func (lookup AuthEventLookup) ThirdPartyInvite(stateKey string) (gomatrixserverlib.PDU, error) {
	return lookup.pdu(types.StateTup{Type: event.StateThirdPartyInvite, StateKey: stateKey})
}

func (lookup AuthEventLookup) Valid() bool {
	return true
}

func userIDForSender(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return senderID.ToUserID(), nil
}

// Authorize keeps lookup errors separate from rejections, including provider errors that
// gomatrixserverlib.Allowed does not propagate.
func Authorize(ev *types.Event, lookup AuthEventLookup) (authErr, err error) {
	var lookupErr error
	provider := AuthEventLookup(func(tup types.StateTup) (*types.Event, error) {
		authEv, err := lookup(tup)
		if lookupErr == nil {
			lookupErr = err
		}
		return authEv, err
	})
	authErr = gomatrixserverlib.Allowed(ev.PDU(), provider, userIDForSender)
	if lookupErr != nil {
		return nil, lookupErr
	}
	return authErr, nil
}
