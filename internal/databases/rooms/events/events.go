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

type EventsDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	byID,
	byVersion,
	idToVersion,
	byRoomVersion,
	byRoomStateVersion,
	byRoomExtrem,
	byRoomVersionStateTup,
	byRoomCurrentStateTup,
	byRoomCurrentMembers,
	byRoomCurrentServers,
	byRoomRelation,
	byRoomReaction,
	byRoomThread subspace.Subspace
}

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
		byID:        eventsDir.Sub("id"),  // event by ID
		byVersion:   eventsDir.Sub("ver"), // event by version
		idToVersion: eventsDir.Sub("itv"), // event ID to version

		byRoomVersion:      eventsDir.Sub("rmv"), // event by room/version
		byRoomStateVersion: eventsDir.Sub("rsv"), // state event by room/verstion
		byRoomExtrem:       eventsDir.Sub("rex"), // current room extremeties

		byRoomVersionStateTup: eventsDir.Sub("rvs"), // state event by room/type/version

		byRoomCurrentStateTup: eventsDir.Sub("rcs"), // current state event by room/type
		byRoomCurrentMembers:  eventsDir.Sub("rmb"), // current members by room
		byRoomCurrentServers:  eventsDir.Sub("rsr"), // current servers by room

		byRoomRelation: eventsDir.Sub("rel"), // event by room/rel-to-ev/version
		byRoomReaction: eventsDir.Sub("rea"), // event by room/rel-to-ev/uid/key
		byRoomThread:   eventsDir.Sub("rth"), // root event by room/root-ev-version
	}
}

func (e *EventsDirectory) KeyForEvent(eventID id.EventID) fdb.Key {
	return e.byID.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) KeyForIDToVersion(eventID id.EventID) fdb.Key {
	return e.idToVersion.Pack(tuple.Tuple{eventID.String()})
}

// Room version indices
//

func (e *EventsDirectory) KeyForVersion(version tuple.Versionstamp) fdb.Key {
	if key, err := e.byVersion.PackWithVersionstamp(tuple.Tuple{version}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) KeyToVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := e.byVersion.Unpack(key)
	return tup[0].(tuple.Versionstamp)
}

func (e *EventsDirectory) RangeForVersion(fromVersion, toVersion tuple.Versionstamp) fdb.Range {
	ret := types.GetVersionRange(e.byVersion, fromVersion, toVersion)
	return ret
}

func (e *EventsDirectory) KeyForRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := e.byRoomVersion.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) KeyToRoomVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := e.byRoomVersion.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

func (e *EventsDirectory) RangeForRoomVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(e.byRoomVersion, fromVersion, toVersion, roomID.String())
}

// Room state versions (room_id, versionstamp) -> (event_id, type, state_key)
//

func (e *EventsDirectory) KeyForRoomStateVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := e.byRoomStateVersion.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (e *EventsDirectory) RangeForRoomStateVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Range {
	return fdb.KeyRange{
		Begin: e.byRoomStateVersion.Pack(tuple.Tuple{roomID.String()}),
		End:   e.byRoomStateVersion.Pack(tuple.Tuple{roomID.String(), version}),
	}
}

// Room member (room_id, user_id) -> (event_id, membership)
//

func (e *EventsDirectory) KeyForCurrentRoomMember(roomID id.RoomID, userID id.UserID) fdb.Key {
	return e.byRoomCurrentMembers.Pack(tuple.Tuple{roomID.String(), userID.String()})
}

func (e *EventsDirectory) CurrentRoomMemberKeyValueToTups(kv fdb.KeyValue) (types.StateTupWithID, types.MembershipTup) {
	tup, _ := e.byRoomCurrentMembers.Unpack(kv.Key)
	membershipTup := types.ValueToMembershipTup(kv.Value)

	return types.StateTupWithID{
		EventID: membershipTup.EventID,
		StateTup: types.StateTup{
			Type:     event.StateMember,
			StateKey: tup[1].(string),
		},
	}, membershipTup
}

func (e *EventsDirectory) RangeForCurrentRoomMembers(roomID id.RoomID) fdb.Range {
	return e.byRoomCurrentMembers.Sub(roomID.String())
}

// Room server (room_id, server_name) -> MembershipTup
//

func (e *EventsDirectory) KeyForCurrentRoomServer(roomID id.RoomID, serverName string) fdb.Key {
	return e.byRoomCurrentServers.Pack(tuple.Tuple{roomID.String(), serverName})
}

func (e *EventsDirectory) CurrentRoomServerKeyToServer(key fdb.Key) string {
	tup, _ := e.byRoomCurrentServers.Unpack(key)
	return tup[1].(string)
}

func (e *EventsDirectory) RangeForCurrentRoomServers(roomID id.RoomID) fdb.Range {
	return e.byRoomCurrentServers.Sub(roomID.String())
}

// Room extremeties (room_id, event_id) -> ''
//

func (e *EventsDirectory) KeyForRoomExtrem(roomID id.RoomID, eventID id.EventID) fdb.Key {
	return e.byRoomExtrem.Pack(tuple.Tuple{roomID.String(), eventID.String()})
}

func (e *EventsDirectory) RoomExtremKeyToEventID(key fdb.Key) id.EventID {
	tup, _ := e.byRoomExtrem.Unpack(key)
	return id.EventID(tup[1].(string))
}

func (e *EventsDirectory) RangeForRoomExtrems(roomID id.RoomID) fdb.Range {
	return e.byRoomExtrem.Sub(roomID.String())
}

// Room current state tups (room_id, type, state_key) -> event_id
//

func (e *EventsDirectory) KeyForRoomCurrentStateTup(roomID id.RoomID, evType event.Type, stateKey *string) fdb.Key {
	return e.byRoomCurrentStateTup.Pack(tuple.Tuple{
		roomID.String(), evType.String(), *stateKey,
	})
}

func (e *EventsDirectory) CurrentRoomStateKeyValueToStateTup(kv fdb.KeyValue) types.StateTupWithID {
	tup, _ := e.byRoomCurrentStateTup.Unpack(kv.Key)
	return types.StateTupWithID{
		EventID: id.EventID(kv.Value),
		StateTup: types.StateTup{
			Type:     event.NewEventType(tup[1].(string)),
			StateKey: tup[2].(string),
		},
	}
}

func (e *EventsDirectory) RangeForRoomCurrentState(roomID id.RoomID) fdb.Range {
	return e.byRoomCurrentStateTup.Sub(roomID.String())
}

// Room version state tups
//

func (e *EventsDirectory) RangeForRoomVersionStateTup(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Range {
	return fdb.KeyRange{
		Begin: e.byRoomVersionStateTup.Pack(tuple.Tuple{
			roomID.String(), evType.String(), stateKey,
		}),
		End: e.byRoomVersionStateTup.Pack(tuple.Tuple{
			roomID.String(), evType.String(), stateKey, version,
		}),
	}
}

func (e *EventsDirectory) KeyForRoomVersionStateTup(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Key {
	if key, err := e.byRoomVersionStateTup.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), evType.String(), stateKey, version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
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
	if key, err := e.byRoomThread.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}
