package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// EventsDirectory is where events and associated metadata are stored in addition to a number of
// indices to enable fast access to various bits of (room) event data.
type EventsDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// Full event bytes by ID (turned into `types.Event`), contians the full event content and extra
	// metadata like `auth_events` & `prev_events`.
	//
	// key: EventID
	// value: types.Event
	idToEvent subspace.Subspace

	// Auth event IDs by event ID, written with every stored event so auth chains are walked
	// without loading full events, with the event's chain position once finalized, 0 until then
	//
	// key: EventID
	// value: (1, Chain, Sequence, (AuthEventID, ...))
	idToAuthEventIDs subspace.Subspace

	// Chain cover positions, in every room version
	//
	// key: (RoomID, Chain, Sequence)
	// value: (EventID)
	authChainPositions subspace.Subspace
	// Chain cover links from a position to the position of an auth event of its event on another chain
	//
	// key: (RoomID, Chain, Sequence, TargetChain, TargetSequence)
	// value: empty
	authChainLinks subspace.Subspace
	// The room's chain ID allocator
	//
	// key: RoomID
	// value: last allocated chain ID
	authChainNext subspace.Subspace

	// Ordered EventTups by version, for paginating all events over all rooms. Used by internal
	// services for things like profile update propagation.
	//
	// key: tuple.Versionstamp
	// value: types.EventTup
	versionToTup subspace.Subspace

	// Event ID to `tuple.Versionstamp`, the version the event was stored at. Tells whether an event
	// is stored and places receipts and thread roots in the timeline. Every stored room event has
	// its own version, rejected and soft failed events included, outliers have none.
	//
	// key: EventID
	// value: Versionstamp
	idToVersion subspace.Subspace

	// Ordered `EventTup`s by room/version, for paginating all events in a room
	//
	// key: (RoomID, tuple.Versionstamp)
	// value: types.EventTup
	roomVersionToTup subspace.Subspace

	// Ordered `EventTup`s originating from this homeserver, for paginating events to send over
	// federation.
	//
	// key: (RoomID, tuple.Versionstamp)
	// value: types.EventTup
	localRoomVersionToTup subspace.Subspace

	// The room's current state context by room/version, one row per step moving it, at the version
	// of the event making the step. The latest row at or before a version is the room's current
	// state as of that version, for syncing state changes.
	//
	// key: (RoomID, tuple.Versionstamp)
	// value: (ContextID)
	roomVersionToState subspace.Subspace

	// Current room extremeties, keys only roomID/eventID. We range over them to pull the current
	// DAG extremeties before clearing when writing events (and setting the written event).
	//
	// - on local send take all event IDs as `prev_events`
	// - on store event clear any found in `prev_events`
	//
	// key: (RoomID, EventID)
	// value: empty
	roomExtremIDs subspace.Subspace

	// RoomID/related-event-ID/version to (eventID, relType)
	//
	// - paginate events relating to this event
	// - paginate events of a certain rel_type relating to this event
	//   - have to paginate through types that don't match
	//   - probably sufficient performance (rare)
	byRoomRelation subspace.Subspace

	// Room event reactions
	//
	// - de-dupe reactions to an event by user/key
	// - paginate reactions for a given event (`/relations/rel_to_ev_id/m.annotation`)
	byRoomReaction subspace.Subspace

	// root event ID by room/root-ev-version
	//
	// Note: only set for new thread roots, on the first replying (in-thread) event.
	//
	// - paginate thread roots in a room
	roomThreadVersionToID subspace.Subspace
}

type SomethingElse struct{}

func NewEventsDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *EventsDirectory {
	eventsDir, err := parentDir.CreateOrOpen(db, []string{"events"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "events").Logger()
	log.Debug().
		Bytes("prefix", eventsDir.Bytes()).
		Msg("Init rooms/events directory")

	return &EventsDirectory{
		log: log,
		db:  db,

		// Init data model subspaces, subspace prefixes are intentionally short
		// "When using the tuple layer to encode keys (as is recommended), select short strings or small integers for tuple elements."
		// https://apple.github.io/foundationdb/data-modeling.html#key-and-value-sizes
		idToEvent:             eventsDir.Sub("eid"),
		idToAuthEventIDs:      eventsDir.Sub("eah"),
		authChainPositions:    eventsDir.Sub("acp"),
		authChainLinks:        eventsDir.Sub("acl"),
		authChainNext:         eventsDir.Sub("acn"),
		versionToTup:          eventsDir.Sub("evr"),
		localRoomVersionToTup: eventsDir.Sub("elv"),
		idToVersion:           eventsDir.Sub("etv"),
		roomVersionToTup:      eventsDir.Sub("rmv"),
		roomVersionToState:    eventsDir.Sub("rch"),
		roomExtremIDs:         eventsDir.Sub("rex"),
		byRoomRelation:        eventsDir.Sub("rel"),
		byRoomReaction:        eventsDir.Sub("rea"),
		roomThreadVersionToID: eventsDir.Sub("rth"),
	}
}

func (e *EventsDirectory) KeyForEventID(eventID id.EventID) fdb.Key {
	return e.idToEvent.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) keyForAuthEventIDs(eventID id.EventID) fdb.Key {
	return e.idToAuthEventIDs.Pack(tuple.Tuple{eventID.String()})
}

// TxnStoreEventRows writes events stored for the first time and returns the version this transaction
// gives each: its body, an unfinalized auth header, its version unless it is an outlier, and with
// indexed its entry in the index of all events. A member event stored before as an outlier keeps its
// auth header, which an auth graph may have finalized as another event's auth event.
func (e *EventsDirectory) TxnStoreEventRows(txn fdb.Transaction, indexed bool, evs ...*types.Event) map[id.EventID]tuple.Versionstamp {
	storedHeaders := make(map[id.EventID]fdb.FutureByteSlice)
	for _, ev := range evs {
		if ev.Type == event.StateMember {
			storedHeaders[ev.ID] = txn.Get(e.keyForAuthEventIDs(ev.ID))
		}
	}
	versions := make(map[id.EventID]tuple.Versionstamp, len(evs))
	for i, ev := range evs {
		version := tuple.IncompleteVersionstamp(uint16(i))
		versions[ev.ID] = version
		txn.Set(e.KeyForEventID(ev.ID), ev.ToMsgpack())
		if header, found := storedHeaders[ev.ID]; !found || len(header.MustGet()) == 0 {
			txn.Set(e.keyForAuthEventIDs(ev.ID), packAuthHeader(types.AuthHeader{AuthEventIDs: ev.AuthEventIDs}))
		}
		if indexed {
			txn.SetVersionstampedKey(e.KeyForVersion(version), types.EventTupToBytes(ev.EventTup()))
		}
		if !ev.Outlier {
			txn.SetVersionstampedValue(e.KeyForIDToVersion(ev.ID), types.MustVersionstampToBytes(version))
		}
	}
	return versions
}

// TxnAdoptEvents rewrites stored events with the outcome a remote join gives them, keeping the auth
// header stored for each.
func (e *EventsDirectory) TxnAdoptEvents(txn fdb.Transaction, evs ...*types.Event) {
	for _, ev := range evs {
		txn.Set(e.KeyForEventID(ev.ID), ev.ToMsgpack())
	}
}

func (e *EventsDirectory) TxnGetEvent(txn fdb.ReadTransaction, id id.EventID) *types.Event {
	b := txn.Get(e.KeyForEventID(id)).MustGet()
	if b == nil {
		return nil
	}
	return types.MustNewEventFromBytes(b, id)
}

func (e *EventsDirectory) KeyForIDToVersion(eventID id.EventID) fdb.Key {
	return e.idToVersion.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) TxnLookupVersionForEventID(
	txn fdb.ReadTransaction,
	eventID id.EventID,
) tuple.Versionstamp {
	b := txn.Get(e.KeyForIDToVersion(eventID)).MustGet()
	if b == nil {
		return types.ZeroVersionstamp
	}
	return types.MustBytesToVersionstamp(b)
}

// Room version indices
//

func (e *EventsDirectory) KeyForVersion(version tuple.Versionstamp) fdb.Key {
	if key, err := e.versionToTup.PackWithVersionstamp(tuple.Tuple{version}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) KeyToVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := e.versionToTup.Unpack(key)
	return tup[0].(tuple.Versionstamp)
}

func (e *EventsDirectory) rangeForVersion(fromVersion, toVersion tuple.Versionstamp) fdb.Range {
	ret := types.GetVersionRange(e.versionToTup, fromVersion, toVersion)
	return ret
}

// Room version
func (e *EventsDirectory) KeyToRoomVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := e.roomVersionToTup.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (e *EventsDirectory) KeyForRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := e.roomVersionToTup.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) rangeForRoomVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(e.roomVersionToTup, fromVersion, toVersion, roomID.String())
}

// Local room version
func (e *EventsDirectory) KeyToLocalRoomVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := e.localRoomVersionToTup.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

func (e *EventsDirectory) KeyForLocalRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := e.localRoomVersionToTup.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) rangeForLocalRoomVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(e.localRoomVersionToTup, fromVersion, toVersion, roomID.String())
}

// Room extremeties (room_id, event_id) -> ''
//

func (e *EventsDirectory) keyForRoomExtrem(roomID id.RoomID, eventID id.EventID) fdb.Key {
	return e.roomExtremIDs.Pack(tuple.Tuple{roomID.String(), eventID.String()})
}

func (e *EventsDirectory) roomExtremKeyToEventID(key fdb.Key) id.EventID {
	tup, _ := e.roomExtremIDs.Unpack(key)
	return id.EventID(tup[1].(string))
}

func (e *EventsDirectory) rangeForRoomExtrems(roomID id.RoomID) fdb.ExactRange {
	return types.GetVersionRange(e.roomExtremIDs, types.ZeroVersionstamp, types.ZeroVersionstamp, roomID.String())
	// return e.roomExtremIDs
}

// Room relations/reactions/threads
//

func (e *EventsDirectory) KeyForRoomRelation(roomID id.RoomID, relEvID id.EventID, version tuple.Versionstamp) fdb.Key {
	if key, err := e.byRoomRelation.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), relEvID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) KeyForRoomReaction(roomID id.RoomID, relEvID id.EventID, userID id.UserID, key string) fdb.Key {
	return e.byRoomReaction.Pack(tuple.Tuple{roomID.String(), relEvID.String(), userID.String(), key})
}

func (e *EventsDirectory) KeyForRoomThread(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(e.roomThreadVersionToID, tuple.Tuple{roomID.String(), version})
}
