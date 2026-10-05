package events

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// TxnEventsProvider is a per-txn singleton that handles pulling event objects from FoundationDB.
// Designed to be passed into each other over multiple transactions sharing the same event cache,
// and many pagination methods take it to kick off event fetches before they are needed.
//
// NOTE: provider is *not* concurrency safe and maintains no internal locking mechanism. FDB txns
// should not be fed into goroutines which means this shouldn't ever be an issue.
type TxnEventsProvider struct {
	log  zerolog.Logger
	txn  fdb.ReadTransaction
	byID subspace.Subspace

	futures map[id.EventID]fdb.FutureByteSlice
	events  map[id.EventID]*types.Event
	// Events whose read found nothing in this transaction
	missing map[id.EventID]struct{}
}

func (e *EventsDirectory) NewTxnEventsProvider(ctx context.Context, txn fdb.ReadTransaction) *TxnEventsProvider {
	log := zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("provider", "TxnEventsProvider").
		Logger()

	provider := &TxnEventsProvider{
		log:     log,
		txn:     txn,
		byID:    e.idToEvent,
		futures: make(map[id.EventID]fdb.FutureByteSlice, 10),
		events:  make(map[id.EventID]*types.Event, 10),
		missing: make(map[id.EventID]struct{}),
	}

	runtime.SetFinalizer(provider, func(ep *TxnEventsProvider) {
		for eventID, fut := range ep.futures {
			if fut.IsReady() {
				ep.log.Warn().Stringer("event_id", eventID).Msg("Unused ready fut in finalized TxnEventsProvider")
			} else {
				ep.log.Warn().Stringer("event_id", eventID).Msg("Never ready fut in finalized TxnEventsProvider")
			}
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
		// Pull any fetched events
		for _, ev := range provider.events {
			ep.Add(ev)
		}
		// Now check for any unclaimed futures that are ready, and get those events too (if exist)
		for evID, evFut := range provider.futures {
			if evFut.IsReady() {
				ev, err := provider.Get(evID)
				if err != nil {
					ep.log.Err(err).Msg("Error getting ready event future")
					continue
				} else if ev != nil {
					ep.Add(ev)
				}
			}
		}
	}
	return ep
}

// Settle waits for every read started, so a provider for a later transaction takes the events read
// with WithProviderEvents once this provider's transaction is too old to read with. A read that
// failed is dropped, to start again there.
func (ep *TxnEventsProvider) Settle() {
	for eventID, fut := range ep.futures {
		if _, err := fut.Get(); err != nil {
			delete(ep.futures, eventID)
		}
	}
}

func (ep *TxnEventsProvider) keyForEventID(eventID id.EventID) fdb.Key {
	return ep.byID.Pack(tuple.Tuple{eventID.String()})
}

func (ep *TxnEventsProvider) WillGet(eventIDs ...id.EventID) {
	for _, eventID := range eventIDs {
		if _, found := ep.events[eventID]; found {
			continue
		}
		if _, found := ep.futures[eventID]; found {
			continue
		}
		if _, found := ep.missing[eventID]; found {
			continue
		}
		fut := ep.txn.Get(ep.keyForEventID(eventID))
		ep.futures[eventID] = fut
		ep.log.Trace().Str("event_id", eventID.String()).Msg("Will get event")
	}
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
	} else if _, found := ep.missing[eventID]; found {
		return nil, nil
	}

	// Fallback to any future we have pending
	fut, found := ep.futures[eventID]
	if !found {
		ep.log.Warn().
			Stringer("event_id", eventID).
			Msg("Fetching event with no existing future")
		fut = ep.txn.Get(ep.keyForEventID(eventID))
	} else if !fut.IsReady() {
		ep.log.Trace().
			Stringer("event_id", eventID).
			Msg("Fetching event using unready future")
	}

	b, err := fut.Get()
	if err != nil {
		ep.log.Err(err).Str("event_id", eventID.String()).Msg("Error fetching event")
		return nil, err
	} else if b == nil {
		ep.log.Warn().Str("event_id", eventID.String()).Msg("Event does not exist")
		if found {
			ep.missing[eventID] = struct{}{}
			delete(ep.futures, eventID)
		}
		return nil, nil
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

// GetRequired is Get for an event that must exist, failing with types.ErrEventNotFound otherwise.
func (ep *TxnEventsProvider) GetRequired(eventID id.EventID) (*types.Event, error) {
	ev, err := ep.Get(eventID)
	if err == nil && ev == nil {
		err = fmt.Errorf("%w: %s", types.ErrEventNotFound, eventID)
	}
	return ev, err
}

// GetAll is GetRequired for each event, fetching them all at once.
func (ep *TxnEventsProvider) GetAll(eventIDs []id.EventID) ([]*types.Event, error) {
	ep.WillGet(eventIDs...)
	evs := make([]*types.Event, len(eventIDs))
	for i, eventID := range eventIDs {
		ev, err := ep.GetRequired(eventID)
		if err != nil {
			return nil, err
		}
		evs[i] = ev
	}
	return evs, nil
}

// An event citing an auth event that is not known was not evaluated, unlike one that fails auth
var ErrAuthEventMissing = errors.New("auth event missing")

// CheckEventAuthEvents authorizes an event using only its declared auth events. The event cache
// is shared, but the auth state is fresh for each event so unrelated branches cannot affect it.
// Auth failures and lookup errors are returned separately so database errors can be retried.
func (ep *TxnEventsProvider) CheckEventAuthEvents(ctx context.Context, ev *types.Event) (authErr, err error) {
	// Auth rule 2 in every room version: no duplicate tuples, and none outside the selection. An
	// event citing more auth events than the selection holds breaks it without loading any.
	selected := authEventSelection(ev)
	if len(ev.AuthEventIDs) > len(selected) {
		return fmt.Errorf("event cites %d auth events, its selection allows %d", len(ev.AuthEventIDs), len(selected)), nil
	}
	authState := make(types.StateMap, len(ev.AuthEventIDs))
	for _, authID := range ev.AuthEventIDs {
		authEv, err := ep.Get(authID)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch auth event: %w", err)
		} else if authEv == nil {
			return fmt.Errorf("%w: %s", ErrAuthEventMissing, authID), nil
		} else if authEv.Rejected {
			return fmt.Errorf("auth event rejected: %s", authID), nil
		} else if authEv.StateKey == nil {
			return fmt.Errorf("auth event %s is not state", authID), nil
		} else if authEv.RoomID != ev.RoomID {
			return fmt.Errorf("auth event %s is for another room", authID), nil
		} else if _, duplicate := authState[authEv.StateTup()]; duplicate {
			return fmt.Errorf("duplicate auth tuple %v", authEv.StateTup()), nil
		} else if !slices.Contains(selected, authEv.StateTup()) {
			return fmt.Errorf("unexpected auth tuple %v", authEv.StateTup()), nil
		}
		authState[authEv.StateTup()] = authEv.ID
	}
	if ev.Type != event.StateCreate && util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) {
		createID := ev.ImplicitCreateEventID()
		create, err := ep.Get(createID)
		if err != nil {
			return nil, err
		}
		if create == nil {
			return fmt.Errorf("%w: implicit create %s", ErrAuthEventMissing, createID), nil
		}
		if create.Rejected || create.Type != event.StateCreate || create.StateKey == nil || *create.StateKey != "" || create.RoomID != ev.RoomID {
			return fmt.Errorf("invalid implicit create %s", createID), nil
		}
		authState[create.StateTup()] = createID
	}
	return NewTxnAuthEventsProvider(ctx, ep, authState).IsEventAllowed(ev)
}

func (ep *TxnEventsProvider) authEventIDs() AuthEventIDs {
	known := make(AuthEventIDs, len(ep.events))
	for eventID, ev := range ep.events {
		known[eventID] = ev.AuthEventIDs
	}
	return known
}

// TxnAuthEventsProvider authorizes events against a supplied state map. Authorization never
// changes that state.
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

func (ap *TxnAuthEventsProvider) lookup(tup types.StateTup) (*types.Event, error) {
	eventID, found := ap.stateMap[tup]
	if !found {
		ap.log.Trace().Stringer("type", tup.Type).Str("state_key", tup.StateKey).Msg("Missed auth state event")
		return nil, nil
	}
	return ap.eventsProvider.GetRequired(eventID)
}

// IsEventAllowed authorizes an event without changing the provider's state. Auth failures and
// failures to load that state are returned separately, so only the former reject the event.
func (ap *TxnAuthEventsProvider) IsEventAllowed(ev *types.Event) (authErr, err error) {
	authErr, err = util.Authorize(ev, ap.lookup)
	if err != nil {
		return nil, fmt.Errorf("failed to load auth state for %s: %w", ev.ID, err)
	}
	return authErr, nil
}

// AuthEventIDsFor selects an event's auth events from the state it is authorized against.
// https://spec.matrix.org/v1.11/server-server-api/#auth-events-selection
func AuthEventIDsFor(ev *types.Event, authState types.StateMap) []id.EventID {
	if ev.Type == event.StateCreate {
		return nil
	}
	authEventIDs := make([]id.EventID, 0, len(authState))
	for _, stateTup := range authEventSelection(ev) {
		if eventID, found := authState[stateTup]; found {
			authEventIDs = append(authEventIDs, eventID)
		}
	}
	slices.Sort(authEventIDs)
	return authEventIDs
}
