package events

import (
	"context"
	"errors"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// TxnEventsProvider is a per-txn singleton that handles all reading of full
// event objects from FoundationDB.
// NOTE: provider is *not* concurrency safe and maintains no internal locking
// mechanism. FDB transactions should not be fed into goroutines which means
// this shouldn't ever be an issue.
type TxnEventsProvider struct {
	log  zerolog.Logger
	txn  fdb.Transaction
	byID subspace.Subspace

	futures map[id.EventID]fdb.FutureByteSlice
	events  map[id.EventID]*types.Event
	rejects map[id.EventID]struct{}
}

func (e *EventsDirectory) NewTxnEventsProvider(ctx context.Context, txn fdb.Transaction, initialEvs []*types.Event) *TxnEventsProvider {
	log := log.Ctx(ctx).With().
		Str("component", "database").
		Str("provider", "TxnEventsProvider").
		Logger()

	provider := &TxnEventsProvider{
		log:     log,
		txn:     txn,
		byID:    e.ByID,
		futures: make(map[id.EventID]fdb.FutureByteSlice, 10),
		events:  make(map[id.EventID]*types.Event, len(initialEvs)),
		rejects: make(map[id.EventID]struct{}),
	}
	for _, ev := range initialEvs {
		provider.Add(ev)
	}
	return provider
}

func (ap *TxnEventsProvider) keyForEventOD(eventID id.EventID) fdb.Key {
	return ap.byID.Pack(tuple.Tuple{eventID.String()})
}

func (ep *TxnEventsProvider) WillGet(eventID id.EventID) {
	if _, found := ep.futures[eventID]; found {
		return
	}
	ep.futures[eventID] = ep.txn.Get(ep.keyForEventOD(eventID))
	ep.log.Trace().Str("event_id", eventID.String()).Msg("Will get event")
}

func (ep *TxnEventsProvider) Add(ev *types.Event) {
	if fut, found := ep.futures[ev.ID]; found {
		fut.Cancel()
		delete(ep.futures, ev.ID)
	}
	ep.events[ev.ID] = ev
}

func (ep *TxnEventsProvider) Reject(eventID id.EventID) {
	ep.rejects[eventID] = struct{}{}
	ep.log.Info().Str("event_id", eventID.String()).Msg("Reject event for transaction")
}

func (ep *TxnEventsProvider) Get(eventID id.EventID) (*types.Event, error) {
	if _, rejected := ep.rejects[eventID]; rejected {
		return nil, nil
	}

	if ev, found := ep.events[eventID]; found {
		return ev, nil
	}

	// Fallback to any future we have pending
	fut, found := ep.futures[eventID]
	if !found {
		ep.log.Warn().
			Stringer("event_id", eventID).
			Msg("Fetching event on-demand, no existing future")
		fut = ep.txn.Get(ep.keyForEventOD(eventID))
	}

	b, err := fut.Get()
	if err != nil {
		ep.log.Err(err).Str("event_id", eventID.String()).Msg("Error fetching event")
		return nil, err
	} else if b == nil {
		ep.log.Warn().Str("event_id", eventID.String()).Msg("Event does not exist")
		return nil, nil
	}

	ep.log.Trace().Str("event_id", eventID.String()).Msg("Load event")
	ev, err := types.NewEventFromBytes(b)
	if err != nil {
		ep.log.Err(err).Str("event_id", eventID.String()).Msg("Error unmarshalling event")
		return nil, err
	}

	// Cache event, drop any future
	ep.events[eventID] = ev
	delete(ep.futures, eventID)

	return ev, nil
}

func (ep *TxnEventsProvider) MustGet(eventID id.EventID) *types.Event {
	ev, err := ep.Get(eventID)
	if err != nil {
		panic(err)
	} else if ev == nil {
		panic(errors.New("event not found"))
	}
	return ev
}

// TxnAuthEventsProvider implements the gomatrixserverlib.AuthEventProvider
// interface in a way that allows us to overwrite event IDs with ones in the
// current persist batch, so batches are authed against themselves if they
// contain auth state changes.
type TxnAuthEventsProvider struct {
	log            zerolog.Logger
	eventsProvider *TxnEventsProvider
	memberEventIDs map[id.UserID]id.EventID
	stateEventIDs  map[event.Type]id.EventID
}

func NewTxnAuthEventsProvider(
	ctx context.Context,
	eventsProvider *TxnEventsProvider,
	memberEventIDs map[id.UserID]id.EventID,
	stateEventIDs map[event.Type]id.EventID,
) *TxnAuthEventsProvider {
	log := log.Ctx(ctx).With().
		Str("component", "database").
		Str("provider", "TxnAuthEventsProvider").
		Logger()

	if memberEventIDs == nil {
		memberEventIDs = make(map[id.UserID]id.EventID)
	}
	if stateEventIDs == nil {
		stateEventIDs = make(map[event.Type]id.EventID)
	}
	return &TxnAuthEventsProvider{
		log:            log,
		eventsProvider: eventsProvider,
		memberEventIDs: memberEventIDs,
		stateEventIDs:  stateEventIDs,
	}
}

func (ap *TxnAuthEventsProvider) get(eventID id.EventID) (gomatrixserverlib.PDU, error) {
	ev, err := ap.eventsProvider.Get(eventID)
	if err != nil {
		return nil, err
	}
	return ev.PDU(), nil
}

func (ap *TxnAuthEventsProvider) getByType(evType event.Type) (gomatrixserverlib.PDU, error) {
	eventID, found := ap.stateEventIDs[evType]
	if !found {
		ap.log.Trace().Str("type", evType.String()).Msg("Missed auth state event")
		return nil, nil
	}
	return ap.get(eventID)
}

func (ap *TxnAuthEventsProvider) IsEventAllowed(ev *types.Event) error {
	err := gomatrixserverlib.Allowed(ev.PDU(), ap, func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return senderID.ToUserID(), nil
	})
	if err == nil {
		// If we authorized this event and it's a state event type, overwrite any
		// in our stateEventIDs/memberEventIDs. This means subsequent auth checks
		// will use this event if it has altered the auth state.
		switch ev.Type {
		case event.StateCreate:
			ap.stateEventIDs[event.StateCreate] = ev.ID
		case event.StateJoinRules:
			ap.stateEventIDs[event.StateJoinRules] = ev.ID
		case event.StatePowerLevels:
			ap.stateEventIDs[event.StatePowerLevels] = ev.ID
		case event.StateMember:
			ap.memberEventIDs[id.UserID(*ev.StateKey)] = ev.ID
		}
	}
	return err
}

func (ap *TxnAuthEventsProvider) GetAuthEventIDsForEvent(ev *types.Event) []id.EventID {
	if ev.Type == event.StateCreate {
		return nil
	}
	authEventIDs := make([]id.EventID, 0, len(ap.stateEventIDs))
	for _, eventID := range ap.stateEventIDs {
		authEventIDs = append(authEventIDs, eventID)
	}
	if eventID, found := ap.memberEventIDs[ev.Sender]; found {
		authEventIDs = append(authEventIDs, eventID)
	}
	if ev.Type == event.StateMember {
		if eventID, found := ap.memberEventIDs[id.UserID(*ev.StateKey)]; found {
			// If we're a member event, grab any current state for the target user
			authEventIDs = append(authEventIDs, eventID)
		}
	}
	return authEventIDs
}

// Interface methods for gomatrixserverlib.AuthEventProvider

func (ap *TxnAuthEventsProvider) Member(senderID spec.SenderID) (gomatrixserverlib.PDU, error) {
	eventID, found := ap.memberEventIDs[id.UserID(senderID)]
	if !found {
		ap.log.Trace().Str("sender", string(senderID)).Msg("Missed member state event")
		return nil, nil
	}
	return ap.get(eventID)
}

func (ap *TxnAuthEventsProvider) Create() (gomatrixserverlib.PDU, error) {
	pdu, err := ap.getByType(event.StateCreate)
	return pdu, err
}

func (ap *TxnAuthEventsProvider) JoinRules() (gomatrixserverlib.PDU, error) {
	return ap.getByType(event.StateJoinRules)
}

func (ap *TxnAuthEventsProvider) PowerLevels() (gomatrixserverlib.PDU, error) {
	return ap.getByType(event.StatePowerLevels)
}

func (ap *TxnAuthEventsProvider) ThirdPartyInvite(s string) (gomatrixserverlib.PDU, error) {
	// TODO: implement
	return nil, nil
}

func (ap *TxnAuthEventsProvider) Valid() bool {
	// TODO: implement
	return true
}
