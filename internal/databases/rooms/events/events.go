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

	// Ordered EventTups by version, for paginating all events over all rooms. Used by internal
	// services for things like profile update propagation.
	//
	// key: tuple.Versionstamp
	// value: types.EventTup
	versionToTup subspace.Subspace

	// Event ID to `tuple.Versionstamp`. Allows us to get the version of an event so we can lookup state
	// at that point in time.
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

	// Ordered `EventStateTup`s by room/version, for pagination of room state changes.
	//
	// - pull everything <= by versionstamp, latest (type, state_key) pairs win
	// - means iterating over duplicates, but alternative is to snapshot state at each change which
	//   is extremely space wasteful.
	// - "get state changes between X and Y events in this room"
	//   (S2S API needs MSC, see [this synapse issue](https://github.com/matrix-org/synapse/issues/13618))
	//
	// key: (RoomID, tuple.Versionstamp)
	// value: types.EventStateTup
	roomStateVersionToTup subspace.Subspace

	// Current room extremeties, keys only roomID/eventID. We range over them to pull the current
	// DAG extremeties before clearing when writing events (and setting the written event).
	//
	// - on local send take all event IDs as `prev_events`
	// - on store event clear any found in `prev_events`
	//
	// key: (RoomID, EventID)
	// value: empty
	roomExtremIDs subspace.Subspace

	// Room ID to tuple of information from the last time state resolution was performed on the room
	// ie (versionstamp, eventID1, eventID2, ...). We combine this with the list of current extrem
	// IDs to identify whether we need to re-resolve the room again (if multiple forks have written
	// state changes since last resolve).
	//
	// key: RoomID
	// value: []id.EventID
	idToLastResolvedData subspace.Subspace

	// State events by room/type/version, for paginating the history of a single state event. This
	// allows us to fetch state for a given room/type at a point in time, for authorizing events.
	//
	// - "get m.room.power_levels at version X"
	//
	// key: (RoomID, EventType, StateKey, Versionstamp)
	// value: EventID
	roomVersionToIDStateTup subspace.Subspace

	// Current state event by room/type for quick lookups and event auth. Also allows fetching the
	// entire room state quickly (without members) as all the keys are stored together.
	//
	// key: (RoomID, EventType, StateKey)
	// value: EventID
	currentRoomStateToID subspace.Subspace

	// Current memberships by room/user, quick lookups of specific user memberships in a room and
	// quick pagination of all members in a room by keeping the keys together.
	//
	// key: (RoomID, EventType, StateKey)
	// value: types.MembershipTup
	currentRoomMemberships subspace.Subspace

	// Current servers by room, used to ensure we're running federation senders for each one when
	// the room receives updates.
	//
	// key: (RoomID, EventType, StateKey)
	// value: types.MembershipTup
	currentRoomServers subspace.Subspace

	// Per server history of membership changes for the room. Allows us to determine a servers
	// membership at a given version.
	//
	// key: (id.RoomID, ServerName, tuple.Versionstamp)
	// value: types.MembershipTup
	serverMembershipChanges subspace.Subspace

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
		idToEvent:               eventsDir.Sub("eid"),
		versionToTup:            eventsDir.Sub("evr"),
		localRoomVersionToTup:   eventsDir.Sub("elv"),
		idToVersion:             eventsDir.Sub("etv"),
		roomVersionToTup:        eventsDir.Sub("rmv"),
		roomStateVersionToTup:   eventsDir.Sub("rsv"),
		roomExtremIDs:           eventsDir.Sub("rex"),
		idToLastResolvedData:    eventsDir.Sub("lre"),
		roomVersionToIDStateTup: eventsDir.Sub("rvs"),
		currentRoomStateToID:    eventsDir.Sub("rcs"),
		currentRoomMemberships:  eventsDir.Sub("rmb"),
		currentRoomServers:      eventsDir.Sub("rsr"),
		serverMembershipChanges: eventsDir.Sub("smc"),
		byRoomRelation:          eventsDir.Sub("rel"),
		byRoomReaction:          eventsDir.Sub("rea"),
		roomThreadVersionToID:   eventsDir.Sub("rth"),
	}
}

func (e *EventsDirectory) keyForEventID(eventID id.EventID) fdb.Key {
	return e.idToEvent.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) TxnStoreEvent(txn fdb.Transaction, ev *types.Event) {
	txn.Set(e.keyForEventID(ev.ID), ev.ToMsgpack())
}

func (e *EventsDirectory) TxnGetEvent(txn fdb.ReadTransaction, id id.EventID) *types.Event {
	b := txn.Get(e.keyForEventID(id)).MustGet()
	if b == nil {
		return nil
	}
	return types.MustNewEventFromBytes(b, id)
}

func (e *EventsDirectory) KeyForIDToVersion(eventID id.EventID) fdb.Key {
	return e.idToVersion.Pack(tuple.Tuple{eventID.String()})
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

// Room state versions (room_id, versionstamp) -> (event_id, type, state_key)
//

func (e *EventsDirectory) KeyToRoomStateVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := e.roomStateVersionToTup.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

func (e *EventsDirectory) KeyForRoomStateVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(e.roomStateVersionToTup, tuple.Tuple{roomID.String(), version})
}

func (e *EventsDirectory) RangeForRoomStateVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(e.roomStateVersionToTup, fromVersion, toVersion, roomID.String())
}

// Room member (room_id, user_id) -> (event_id, membership)
//

func (e *EventsDirectory) KeyForCurrentRoomMember(roomID id.RoomID, userID id.UserID) fdb.Key {
	return e.currentRoomMemberships.Pack(tuple.Tuple{roomID.String(), userID.String()})
}

func (e *EventsDirectory) CurrentRoomMemberKeyValueToTups(kv fdb.KeyValue) (types.EventStateTup, types.MembershipTup) {
	tup, _ := e.currentRoomMemberships.Unpack(kv.Key)
	membershipTup := types.BytesToMembershipTup(kv.Value)

	return types.EventStateTup{
		EventID: membershipTup.EventID,
		StateTup: types.StateTup{
			Type:     event.StateMember,
			StateKey: tup[1].(string),
		},
	}, membershipTup
}

func (e *EventsDirectory) RangeForCurrentRoomMembers(roomID id.RoomID) fdb.Range {
	return e.currentRoomMemberships.Sub(roomID.String())
}

// Room server (room_id, server_name) -> MembershipTup
//

func (e *EventsDirectory) KeyForCurrentRoomServer(roomID id.RoomID, serverName string) fdb.Key {
	return e.currentRoomServers.Pack(tuple.Tuple{roomID.String(), serverName})
}

func (e *EventsDirectory) currentRoomServerKeyToServer(key fdb.Key) string {
	tup, _ := e.currentRoomServers.Unpack(key)
	return tup[1].(string)
}

func (e *EventsDirectory) rangeForCurrentRoomServers(roomID id.RoomID) fdb.Range {
	return e.currentRoomServers.Sub(roomID.String())
}

func (e *EventsDirectory) TxnStoreServerMembership(
	txn fdb.Transaction,
	roomID id.RoomID,
	serverName string,
	mtup types.MembershipTup,
	version tuple.Versionstamp,
) {
	mtupBytes := types.MembershipTupToBytes(mtup)

	key := e.KeyForCurrentRoomServer(roomID, serverName)
	if mtup.Membership == event.MembershipJoin {
		txn.Set(key, mtupBytes)
	} else {
		txn.Clear(key)
	}

	key, err := e.serverMembershipChanges.PackWithVersionstamp(tuple.Tuple{roomID.String(), serverName, version})
	if err != nil {
		panic(err)
	}
	txn.Set(key, mtupBytes)
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

// Room last resolution event IDs
func (e *EventsDirectory) KeyForRoomLastResolvedExtrems(roomID id.RoomID) fdb.Key {
	return e.idToLastResolvedData.Pack(tuple.Tuple{roomID.String(), "ids"})
}

// Room last resolution version
func (e *EventsDirectory) KeyForRoomLastResolvedVersion(roomID id.RoomID) fdb.Key {
	return e.idToLastResolvedData.Pack(tuple.Tuple{roomID.String(), "version"})
}

// Room current state tups (room_id, type, state_key) -> event_id
//

func (e *EventsDirectory) KeyForRoomCurrentStateTup(roomID id.RoomID, evType event.Type, stateKey string) fdb.Key {
	return e.currentRoomStateToID.Pack(tuple.Tuple{
		roomID.String(), evType.String(), stateKey,
	})
}

func (e *EventsDirectory) CurrentRoomStateKeyValueToStateTup(kv fdb.KeyValue) types.EventStateTup {
	tup, _ := e.currentRoomStateToID.Unpack(kv.Key)
	return types.EventStateTup{
		EventID: id.EventID(kv.Value),
		StateTup: types.StateTup{
			Type:     event.NewEventType(tup[1].(string)),
			StateKey: tup[2].(string),
		},
	}
}

func (e *EventsDirectory) RangeForRoomCurrentState(roomID id.RoomID) fdb.Range {
	return e.currentRoomStateToID.Sub(roomID.String())
}

// Room version state tups
//

func (e *EventsDirectory) rangeForRoomVersionStateTup(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Range {
	return fdb.KeyRange{
		Begin: e.roomVersionToIDStateTup.Pack(tuple.Tuple{
			roomID.String(), evType.String(), stateKey,
		}),
		End: e.roomVersionToIDStateTup.Pack(tuple.Tuple{
			roomID.String(), evType.String(), stateKey, version,
		}),
	}
}

func (e *EventsDirectory) KeyForRoomStateTupVersion(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(e.roomVersionToIDStateTup, tuple.Tuple{roomID.String(), evType.String(), stateKey, version})
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
	return e.roomThreadVersionToID.Pack(tuple.Tuple{roomID.String(), version})
}
