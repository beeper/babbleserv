package events

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type EventsDirectory struct {
	log zerolog.Logger
	db  fdb.Database
	ByID,
	ByVersion,
	IDToVersion,
	ByRoomVersion,
	ByRoomVersionState,
	ByRoomCurrentState,
	ByRoomMembers,
	ByRoomLast subspace.Subspace
}

func NewEventsDirectory(logger zerolog.Logger, db fdb.Database) *EventsDirectory {
	eventsDir, err := directory.CreateOrOpen(db, []string{"evs"}, nil)
	if err != nil {
		panic(err)
	}

	return &EventsDirectory{
		log: logger.With().Str("database", "events").Logger(),
		db:  db,

		// Init data model subspaces, subspace prefixes are intentionally short
		// "When using the tuple layer to encode keys (as is recommended), select short strings or small integers for tuple elements."
		// https://apple.github.io/foundationdb/data-modeling.html#key-and-value-sizes
		ByID:               eventsDir.Sub("id"),
		ByVersion:          eventsDir.Sub("ver"),
		IDToVersion:        eventsDir.Sub("itv"),
		ByRoomVersion:      eventsDir.Sub("rmv"),
		ByRoomVersionState: eventsDir.Sub("rvst"),
		ByRoomCurrentState: eventsDir.Sub("rcst"),
		ByRoomMembers:      eventsDir.Sub("rmem"),
		ByRoomLast:         eventsDir.Sub("rlst"),
	}
}

func (e *EventsDirectory) KeyForEvent(eventID id.EventID) fdb.Key {
	return e.ByID.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) KeyForVersion(version tuple.Versionstamp) fdb.Key {
	key, err := e.ByVersion.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (e *EventsDirectory) KeyForIDToVersion(eventID id.EventID) fdb.Key {
	return e.IDToVersion.Pack(tuple.Tuple{eventID.String()})
}

func (e *EventsDirectory) KeyForRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	key, err := e.ByRoomVersion.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	})
	if err != nil {
		panic(err)
	}
	return key
}

func (e *EventsDirectory) KeyForRoomMember(roomID id.RoomID, userID id.UserID) fdb.Key {
	return e.ByRoomMembers.Pack(tuple.Tuple{roomID.String(), userID.String()})
}

func (e *EventsDirectory) KeyForRoomLast(roomID id.RoomID, eventID id.EventID) fdb.Key {
	return e.ByRoomLast.Pack(tuple.Tuple{roomID.String(), eventID.String()})
}

func (e *EventsDirectory) RangeForRoomLasts(roomID id.RoomID) fdb.Range {
	return e.ByRoomLast.Sub(roomID.String())
}

func (e *EventsDirectory) KeyForRoomCurrentState(roomID id.RoomID, evType event.Type, stateKey *string) fdb.Key {
	return e.ByRoomCurrentState.Pack(tuple.Tuple{
		roomID.String(), evType.String(), *stateKey,
	})
}

func (e *EventsDirectory) RangeForCurrentRoomState(roomID id.RoomID) fdb.Range {
	return e.ByRoomCurrentState.Sub(roomID.String())
}

func (e *EventsDirectory) RangeForRoomVersionState(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Range {
	start := e.ByRoomVersionState.Pack(tuple.Tuple{
		roomID.String(), evType.String(), stateKey,
	})
	end := e.ByRoomVersionState.Pack(tuple.Tuple{
		roomID.String(), evType.String(), stateKey, version,
	})
	return fdb.KeyRange{
		Begin: start,
		End:   end,
	}
}

func (e *EventsDirectory) KeyForRoomVersionState(roomID id.RoomID, evType event.Type, stateKey string, version tuple.Versionstamp) fdb.Key {
	key, err := e.ByRoomVersionState.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), evType.String(), stateKey, version,
	})
	if err != nil {
		panic(err)
	}
	return key
}
