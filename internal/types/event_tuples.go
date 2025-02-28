package types

import (
	"bytes"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type EventIDTup struct {
	EventID id.EventID
	RoomID  id.RoomID
}

type EventIDTupWithVersion struct {
	EventIDTup
	Version tuple.Versionstamp
}

func EventIDTupToValue(tup EventIDTup) []byte {
	return tuple.Tuple{tup.EventID.String(), tup.RoomID.String()}.Pack()
}

func ValueToEventIDTup(value []byte) EventIDTup {
	tup, _ := tuple.Unpack(value)
	return EventIDTup{
		EventID: id.EventID(tup[0].(string)),
		RoomID:  id.RoomID(tup[1].(string)),
	}
}

func SortEventIDTupWithVersions(versions []EventIDTupWithVersion) {
	slices.SortFunc(versions, func(a, b EventIDTupWithVersion) int {
		return bytes.Compare(a.Version.Bytes(), b.Version.Bytes())
	})
}

// State tuples defined as (type, stateKey)
type StateTup struct {
	Type     event.Type `json:"type"`
	StateKey string     `json:"state_key"`
}

func (tup StateTup) MarshalText() ([]byte, error) {
	if tup.StateKey == "" {
		return []byte(tup.Type.String()), nil
	}
	return []byte(tup.Type.String() + "/" + tup.StateKey), nil
}

type StateTupWithID struct {
	StateTup `json:",inline"`
	EventID  id.EventID
}

type StateMap map[StateTup]id.EventID

func StateTupWithIDToValue(tup StateTupWithID) []byte {
	return tuple.Tuple{tup.EventID.String(), tup.Type.String(), tup.StateKey}.Pack()
}

func ValueToStateTupWithID(value []byte) StateTupWithID {
	tup, _ := tuple.Unpack(value)
	return StateTupWithID{
		EventID: id.EventID(tup[0].(string)),
		StateTup: StateTup{
			Type:     event.NewEventType(tup[1].(string)),
			StateKey: tup[2].(string),
		},
	}
}

// Membership tuples defined as (eventID, roomID, membership)
type MembershipTup struct {
	EventID    id.EventID       `json:"event_id"`
	RoomID     id.RoomID        `json:"room_id"`
	Membership event.Membership `json:"membership"`
}
type MembershipTupWithVersion struct {
	MembershipTup
	Version tuple.Versionstamp
}

func (tup MembershipTup) MarshalText() ([]byte, error) {
	return []byte(string(tup.Membership) + "/" + tup.RoomID.String() + "/" + tup.EventID.String()), nil
}

type Memberships map[id.RoomID]MembershipTup
type MembershipChanges []MembershipTupWithVersion

func MembershipTupToValue(tup MembershipTup) []byte {
	return tuple.Tuple{tup.EventID.String(), tup.RoomID.String(), string(tup.Membership)}.Pack()
}

func ValueToMembershipTup(value []byte) MembershipTup {
	tup, _ := tuple.Unpack(value)
	return MembershipTup{
		EventID:    id.EventID(tup[0].(string)),
		RoomID:     id.RoomID(tup[1].(string)),
		Membership: event.Membership(tup[2].(string)),
	}
}
