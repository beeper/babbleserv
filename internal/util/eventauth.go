package util

import (
	"errors"
	"fmt"

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
// gomatrixserverlib.Allowed does not propagate. It adds the rule of state DAG rooms (MSC4242) that
// gomatrixserverlib lacks, 1.3: a create event citing prev state events is rejected.
func Authorize(ev *types.Event, lookup AuthEventLookup) (authErr, err error) {
	if ev.Type == event.StateCreate && ev.StateDAGs() && len(ev.PrevStateEventIDs) > 0 {
		return errors.New("create event has prev_state_events"), nil
	}
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

// PrevStateEventLookup returns an event an event cites in prev_state_events, nil when unknown here
type PrevStateEventLookup func(id.EventID) (*types.Event, error)

// An event is evaluated only once every event it cites is, so a missing one is not a rejection
var ErrPrevStateEventMissing = errors.New("prev state event missing")

// AuthorizePrevStateEvents applies section 2 of the auth rules of state DAG rooms (MSC4242): each
// event the event cites in prev_state_events must be a state event of its room that was not
// rejected, so rejection cascades along the state DAG. Events of other room versions pass.
func AuthorizePrevStateEvents(ev *types.Event, lookup PrevStateEventLookup) (authErr, err error) {
	if !ev.StateDAGs() {
		return nil, nil
	}
	for _, eventID := range ev.PrevStateEventIDs {
		prevStateEv, err := lookup(eventID)
		switch {
		case err != nil:
			return nil, err
		case prevStateEv == nil:
			return nil, fmt.Errorf("%w: %s", ErrPrevStateEventMissing, eventID)
		case prevStateEv.RoomID != ev.RoomID:
			return fmt.Errorf("prev state event %s is in another room", eventID), nil
		case prevStateEv.StateKey == nil:
			return fmt.Errorf("prev state event %s is not a state event", eventID), nil
		case prevStateEv.Rejected:
			return fmt.Errorf("prev state event %s was rejected", eventID), nil
		}
	}
	return nil, nil
}
