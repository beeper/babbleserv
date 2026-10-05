package types

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type EventTup struct {
	EventID id.EventID
	RoomID  id.RoomID
	Sender  id.UserID
	Type    event.Type
}

type EventTupWithVersion struct {
	EventTup
	Version tuple.Versionstamp
}

func (t EventTupWithVersion) GetVersion() tuple.Versionstamp {
	return t.Version
}

func EventTupToBytes(tup EventTup) []byte {
	// TODO: switch back to only evid/roomid to test migration
	// return tuple.Tuple{
	// 	tup.EventID.String(),
	// 	tup.RoomID.String(),
	// }.Pack()
	return tuple.Tuple{
		tup.EventID.String(),
		tup.RoomID.String(),
		tup.Sender.String(),
		tup.Type.String(),
	}.Pack()
}

func BytesToEventTup(b []byte) EventTup {
	tup, err := tuple.Unpack(b)
	if err != nil {
		panic(err)
	}
	if len(tup) == 2 {
		// TODO: remove once migration completes
		zerolog.Ctx(context.TODO()).Warn().Any("tup", tup).Msg("Handle legacy EventTup")
		return EventTup{
			EventID: id.EventID(tup[0].(string)),
			RoomID:  id.RoomID(tup[1].(string)),
		}
	}
	return EventTup{
		EventID: id.EventID(tup[0].(string)),
		RoomID:  id.RoomID(tup[1].(string)),
		Sender:  id.UserID(tup[2].(string)),
		Type:    event.NewEventType(tup[3].(string)),
	}
}

func SortEventTups(versions []EventTupWithVersion) {
	slices.SortFunc(versions, func(a, b EventTupWithVersion) int {
		return bytes.Compare(a.Version.Bytes(), b.Version.Bytes())
	})
}

// State tuples defined as (type, stateKey)
type StateTup struct {
	Type     event.Type `json:"type"`
	StateKey string     `json:"state_key"`
}

func MemberStateTup(userID id.UserID) StateTup {
	return StateTup{Type: event.StateMember, StateKey: userID.String()}
}

// Compare orders state tuples by type, then state key
func (tup StateTup) Compare(other StateTup) int {
	return cmp.Or(strings.Compare(tup.Type.Type, other.Type.Type), strings.Compare(tup.StateKey, other.StateKey))
}

func (tup StateTup) MarshalText() ([]byte, error) {
	if tup.StateKey == "" {
		return []byte(tup.Type.String()), nil
	}
	return []byte(tup.Type.String() + "/" + tup.StateKey), nil
}

// TupleElements are the type and state key as FoundationDB tuple elements, read back with
// StateTupFromElements.
func (tup StateTup) TupleElements() tuple.Tuple {
	return tuple.Tuple{tup.Type.String(), tup.StateKey}
}

func StateTupFromElements(elements tuple.Tuple) (StateTup, bool) {
	if len(elements) != 2 {
		return StateTup{}, false
	}
	evType, typeOK := elements[0].(string)
	stateKey, stateKeyOK := elements[1].(string)
	if !typeOK || !stateKeyOK {
		return StateTup{}, false
	}
	return StateTup{Type: event.NewEventType(evType), StateKey: stateKey}, true
}

type EventStateTup struct {
	StateTup `json:",inline"`
	EventID  id.EventID
}
type EventStateTupWithVersion struct {
	EventStateTup
	Version tuple.Versionstamp
}

func (tup EventStateTup) MarshalText() ([]byte, error) {
	if tup.StateKey == "" {
		return []byte(tup.Type.String() + "/" + tup.EventID.String()), nil
	}
	return []byte(tup.Type.String() + "/" + tup.StateKey + "/" + tup.EventID.String()), nil
}

type StateMap map[StateTup]id.EventID

func (s StateMap) ToTups() []EventStateTup {
	tups := make([]EventStateTup, 0, len(s))
	for tup, eventID := range s {
		tups = append(tups, EventStateTup{
			StateTup: tup,
			EventID:  eventID,
		})
	}
	return tups
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
type RoomMemberships map[id.UserID]MembershipTup

func MembershipTupToBytes(tup MembershipTup) []byte {
	return tuple.Tuple{tup.EventID.String(), tup.RoomID.String(), string(tup.Membership)}.Pack()
}

func BytesToMembershipTup(b []byte) MembershipTup {
	tup, _ := tuple.Unpack(b)
	return membershipTupOf(tup)
}

func membershipTupOf(tup tuple.Tuple) MembershipTup {
	return MembershipTup{
		EventID:    id.EventID(tup[0].(string)),
		RoomID:     id.RoomID(tup[1].(string)),
		Membership: event.Membership(tup[2].(string)),
	}
}

// MembershipRow is a local user's membership row of a room, marking an outlier membership received
// while this server is out of the room.
type MembershipRow struct {
	MembershipTup
	Outlier bool
}

// MembershipRowToBytes packs (event_id, room_id, membership), and true after them for an outlier
// membership
func MembershipRowToBytes(row MembershipRow) []byte {
	tup := tuple.Tuple{row.EventID.String(), row.RoomID.String(), string(row.Membership)}
	if row.Outlier {
		tup = append(tup, true)
	}
	return tup.Pack()
}

func BytesToMembershipRow(b []byte) MembershipRow {
	tup, _ := tuple.Unpack(b)
	return MembershipRow{MembershipTup: membershipTupOf(tup), Outlier: len(tup) > 3 && tup[3] == true}
}
