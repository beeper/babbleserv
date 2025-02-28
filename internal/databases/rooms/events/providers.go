package events

import (
	"context"
	"runtime"
	"slices"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
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
	txn  fdb.ReadTransaction
	byID subspace.Subspace

	futures map[id.EventID]fdb.FutureByteSlice
	events  map[id.EventID]*types.Event
}

func (e *EventsDirectory) NewTxnEventsProvider(ctx context.Context, txn fdb.ReadTransaction) *TxnEventsProvider {
	log := zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("provider", "TxnEventsProvider").
		Logger()

	provider := &TxnEventsProvider{
		log:     log,
		txn:     txn,
		byID:    e.byID,
		futures: make(map[id.EventID]fdb.FutureByteSlice, 10),
		events:  make(map[id.EventID]*types.Event, 10),
	}

	runtime.SetFinalizer(provider, func(ep *TxnEventsProvider) {
		for eventID := range ep.futures {
			ep.log.Warn().Stringer("event_id", eventID).Msg("Unused fut in finalized TxnEventsProvider")
		}
	})

	return provider
}

func (ep *TxnEventsProvider) WithEvents(evs ...*types.Event) *TxnEventsProvider {
	for _, ev := range evs {
		ep.Add(ev)
	}
	return ep
}

func (ep *TxnEventsProvider) WithProviderEvents(providers ...*TxnEventsProvider) *TxnEventsProvider {
	for _, provider := range providers {
		for _, ev := range provider.events {
			ep.Add(ev)
		}
	}
	return ep
}

func (ep *TxnEventsProvider) keyForEventOD(eventID id.EventID) fdb.Key {
	return ep.byID.Pack(tuple.Tuple{eventID.String()})
}

func (ep *TxnEventsProvider) WillGet(eventID id.EventID) fdb.FutureByteSlice {
	if fut, found := ep.futures[eventID]; found {
		return fut
	}
	fut := ep.txn.Get(ep.keyForEventOD(eventID))
	ep.futures[eventID] = fut
	ep.log.Trace().Str("event_id", eventID.String()).Msg("Will get event")
	return fut
}

func (ep *TxnEventsProvider) Add(ev *types.Event) {
	if fut, found := ep.futures[ev.ID]; found {
		fut.Cancel()
		delete(ep.futures, ev.ID)
	}
	ep.events[ev.ID] = ev
}

func (ep *TxnEventsProvider) Get(eventID id.EventID) (*types.Event, error) {
	if ev, found := ep.events[eventID]; found {
		return ev, nil
	}

	// Fallback to any future we have pending
	fut, found := ep.futures[eventID]
	if !found {
		ep.log.Warn().
			Stringer("event_id", eventID).
			Msg("Fetching event with no existing future")
		fut = ep.txn.Get(ep.keyForEventOD(eventID))
	} else if !fut.IsReady() {
		ep.log.Warn().
			Stringer("event_id", eventID).
			Msg("Fetching event using unready future")
	}

	b, err := fut.Get()
	if err != nil {
		ep.log.Err(err).Str("event_id", eventID.String()).Msg("Error fetching event")
		return nil, err
	} else if b == nil {
		ep.log.Warn().Str("event_id", eventID.String()).Msg("Event does not exist")
		return nil, types.ErrEventNotFound
	}

	ep.log.Trace().Str("event_id", eventID.String()).Msg("Load event")
	ev, err := types.NewEventFromBytes(b, eventID)
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
	stateMap       types.StateMap
}

func NewTxnAuthEventsProvider(
	ctx context.Context,
	eventsProvider *TxnEventsProvider,
	stateMap types.StateMap,
) *TxnAuthEventsProvider {
	log := zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("provider", "TxnAuthEventsProvider").
		Logger()

	return &TxnAuthEventsProvider{
		log:            log,
		eventsProvider: eventsProvider,
		stateMap:       stateMap,
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
	eventID, found := ap.stateMap[types.StateTup{Type: evType}]
	if !found {
		ap.log.Trace().Str("type", evType.String()).Msg("Missed auth state event")
		return nil, nil
	}
	return ap.get(eventID)
}

func (ap *TxnAuthEventsProvider) IsEventAllowed(ev *types.Event) error {
	if err := gomatrixserverlib.Allowed(
		ev.PDU(),
		ap,
		func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return senderID.ToUserID(), nil
		},
	); err != nil {
		return err
	}
	// If we authorized this event and it's a state event type, overwrite any
	// in our stateEventIDs/memberEventIDs. This means subsequent auth checks
	// will use this event if it has altered the auth state.
	switch ev.Type {
	case event.StateCreate:
		ap.stateMap[types.StateTup{Type: event.StateCreate}] = ev.ID
	case event.StateJoinRules:
		ap.stateMap[types.StateTup{Type: event.StateJoinRules}] = ev.ID
	case event.StatePowerLevels:
		ap.stateMap[types.StateTup{Type: event.StatePowerLevels}] = ev.ID
	case event.StateMember:
		ap.stateMap[types.StateTup{
			Type:     event.StateMember,
			StateKey: *ev.StateKey,
		}] = ev.ID
	}
	return nil
}

// https://spec.matrix.org/v1.11/server-server-api/#auth-events-selection
func (ap *TxnAuthEventsProvider) GetAuthEventIDsForEvent(ev *types.Event) []id.EventID {
	if ev.Type == event.StateCreate {
		return nil
	}
	authEventIDs := make([]id.EventID, 0, len(ap.stateMap))
	for stateTup, eventID := range ap.stateMap {
		var include bool
		if stateTup.Type == event.StateCreate {
			// The m.room.create event.
			include = true
		} else if stateTup.Type == event.StatePowerLevels {
			// The current m.room.power_levels event, if any.
			include = true
		} else if stateTup.Type == event.StateMember && stateTup.StateKey == ev.Sender.String() {
			// The sender’s current m.room.member event, if any.
			include = true
		} else if ev.Type == event.StateMember {
			// If type is m.room.member:
			if stateTup.Type == event.StateMember && stateTup.StateKey == *ev.StateKey {
				// The target’s current m.room.member event, if any.
				include = true
			}
			// TODO: If membership is invite and content contains a third_party_invite property, the current m.room.third_party_invite event with state_key matching content.third_party_invite.signed.token, if any.
			// TODO: If content.join_authorised_via_users_server is present, and the room version supports restricted rooms, then the m.room.member event with state_key matching content.join_authorised_via_users_server.
		}
		if include {
			authEventIDs = append(authEventIDs, eventID)
		}
	}
	slices.SortFunc(authEventIDs, func(a, b id.EventID) int {
		return strings.Compare(a.String(), b.String())
	})
	return authEventIDs
}

// Interface methods for gomatrixserverlib.AuthEventProvider

func (ap *TxnAuthEventsProvider) Member(senderID spec.SenderID) (gomatrixserverlib.PDU, error) {
	eventID, found := ap.stateMap[types.StateTup{
		Type:     event.StateMember,
		StateKey: string(senderID),
	}]
	if !found {
		ap.log.Trace().Str("sender", string(senderID)).Msg("Missed member state event")
		return nil, nil
	}
	return ap.get(eventID)
}

func (ap *TxnAuthEventsProvider) Create() (gomatrixserverlib.PDU, error) {
	return ap.getByType(event.StateCreate)
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
