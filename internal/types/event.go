package types

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vmihailenco/msgpack/v5"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var _ json.Marshaler = (*Event)(nil)
var _ json.Unmarshaler = (*Event)(nil)
var _ msgpack.Marshaler = (*Event)(nil)

// A partial event before hashing, signatures and prev/auth events are added
type PartialEvent struct {
	RoomID    id.RoomID       `msgpack:"rid" json:"room_id,omitempty"`
	Sender    id.UserID       `msgpack:"sdr" json:"sender,omitempty"`
	StateKey  *string         `msgpack:"sky" json:"state_key,omitempty"`
	Content   json.RawMessage `msgpack:"cnt" json:"content"`
	Redacts   id.EventID      `msgpack:"rds" json:"redacts,omitempty"` // room <v10
	Timestamp int64           `msgpack:"ots" json:"origin_server_ts"`

	// event.Type doesn't implement msgpack marshalling, so we use TypeStr
	TypeStr string     `msgpack:"typ" json:"-"`
	Type    event.Type `msgpack:"-" json:"type"`

	// We store some unsigned info (invite state), marshalled as JSON bytes
	UnsignedRaw json.RawMessage `msgpack:"ussn" json:"-"`
	Unsigned    map[string]any  `msgpack:"-" json:"unsigned,omitempty"`
}

// https://spec.matrix.org/v1.11/rooms/v11/#event-format-1
type Event struct {
	PartialEvent `msgpack:",inline" json:",inline"`

	// Populated at fetch from key (not stored)
	ID id.EventID `msgpack:"-" json:"-"`

	// Internal flag to indicate whether this event was generated locally
	Local bool `msgpack:"loc" json:"-"`
	// Internal copy of the room version so we don't need to look it up
	RoomVersion string `msgpack:"rmv" json:"-"`
	// Internal indicators of whether an event is soft failed or an outlier, if so it should not
	// appear in any indices or user facing responses.
	SoftFailed bool `msgpack:"sfd" json:"-"`
	Outlier    bool `msgpack:"out" json:"-"`
	Rejected   bool `msgpack:"rej" json:"-"`
	// Internal indicator of whether the event has been redacted - needed? (content hash)
	Redacted bool `msgpack:"red" json:"-"`

	// Spec unclear, sometimes exists others does not - must be here for signature validation, not
	// actually used anywhere.
	Origin string `msgpack:"ori" json:"origin,omitempty"`

	Depth int64 `msgpack:"dpt" json:"depth"`

	// Only here for backwards compat ???
	PrevState []id.EventID `msgpack:"pst" json:"prev_state,omitzero"`

	PrevEventIDs []id.EventID `msgpack:"pid" json:"prev_events"`
	AuthEventIDs []id.EventID `msgpack:"aid" json:"auth_events"`

	Hashes     map[string]string            `msgpack:"hsh" json:"hashes"`
	Signatures map[string]map[string]string `msgpack:"sig" json:"signatures"`

	// Internal, in-memory only flags used for the lifetime of a request/background job
	IsForClientAPI    bool               `msgpack:"-" json:"-"`
	IsDuplicate       bool               `msgpack:"-" json:"-"`
	IncompleteVersion tuple.Versionstamp `msgpack:"-" json:"-"`
}

func NewEventFromBytes(b []byte, id id.EventID) (*Event, error) {
	var ev Event
	if err := msgpack.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	ev.ID = id
	return &ev, nil
}

func MustNewEventFromBytes(b []byte, id id.EventID) *Event {
	if ev, err := NewEventFromBytes(b, id); err != nil {
		panic(err)
	} else {
		return ev
	}
}

func NewPartialEvent(
	roomID id.RoomID,
	evType event.Type,
	stateKey *string,
	sender id.UserID,
	content map[string]any,
) *PartialEvent {
	contentBytes, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	ev := &PartialEvent{
		RoomID:   roomID,
		Type:     evType,
		StateKey: stateKey,
		Sender:   sender,
		Content:  contentBytes,
	}
	return ev
}

func NewEventFromPartialEvent(pev *PartialEvent) *Event {
	return &Event{
		PartialEvent: *pev,
	}
}

func (ev *PartialEvent) SetUnsigned(key string, value any) {
	if ev.Unsigned == nil {
		ev.Unsigned = make(map[string]any, 1)
	}
	ev.Unsigned[key] = value
}

func eventIDsFromProtoEvent(input any) []id.EventID {
	ids := input.([]any)
	evIDs := make([]id.EventID, 0, len(ids))
	for _, ev := range ids {
		evIDs = append(evIDs, id.EventID(ev.(string)))
	}
	return evIDs
}

func EventFromProtoEvent(protoEv gomatrixserverlib.ProtoEvent) *Event {
	return &Event{
		PartialEvent: PartialEvent{
			RoomID:   id.RoomID(protoEv.RoomID),
			Sender:   id.UserID(protoEv.SenderID),
			TypeStr:  protoEv.Type,
			Type:     event.NewEventType(protoEv.Type),
			StateKey: protoEv.StateKey,
			Content:  []byte(protoEv.Content),
			Redacts:  id.EventID(protoEv.Redacts),
		},
		Depth:        protoEv.Depth,
		AuthEventIDs: eventIDsFromProtoEvent(protoEv.AuthEvents),
		PrevEventIDs: eventIDsFromProtoEvent(protoEv.PrevEvents),
	}
}

type marshalEvent Event

// Wrapper around msgpack marshalling to set .TypeStr + .UnsignedRaw
func (ev Event) MarshalMsgpack() ([]byte, error) {
	ev.TypeStr = ev.Type.Type

	if b, err := json.Marshal(ev.Unsigned); err != nil {
		return nil, err
	} else {
		ev.UnsignedRaw = b
	}

	return msgpack.Marshal((marshalEvent)(ev))
}

// Wrapper around msgpack unmarshal to set .Type + .Unsigned
func (ev *Event) UnmarshalMsgpack(b []byte) error {
	if err := msgpack.Unmarshal(b, (*marshalEvent)(ev)); err != nil {
		return err
	}

	ev.Type = event.NewEventType(ev.TypeStr)

	if len(ev.UnsignedRaw) > 0 {
		if err := json.Unmarshal(ev.UnsignedRaw, &ev.Unsigned); err != nil {
			return err
		}
	}
	return nil
}

func (ev *Event) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(ev); err != nil {
		panic(err)
	} else {
		return b
	}
}

func (ev Event) MarshalJSON() ([]byte, error) {
	if ev.IsForClientAPI {
		// Client API removes room_id, adds event_id
		b := exerrors.Must(json.Marshal(ev.PartialEvent))
		b = exerrors.Must(sjson.DeleteBytes(b, "room_id"))
		b = exerrors.Must(sjson.SetBytes(b, "event_id", ev.ID))
		return b, nil
	}

	if ev.AuthEventIDs == nil {
		ev.AuthEventIDs = make([]id.EventID, 0)
	}
	if ev.PrevEventIDs == nil {
		ev.PrevEventIDs = make([]id.EventID, 0)
	}
	b := exerrors.Must(json.Marshal((marshalEvent)(ev)))
	// Strip unsigned from any S2S API calls
	b = exerrors.Must(sjson.DeleteBytes(b, "unsigned"))
	return b, nil
}

func (ev *Event) UnmarshalJSON(b []byte) error {
	return json.Unmarshal(b, (*marshalEvent)(ev))
}

func (ev *Event) EventTup() EventTup {
	return EventTup{
		EventID: ev.ID,
		RoomID:  ev.RoomID,
		Sender:  ev.Sender,
		Type:    ev.Type,
	}
}

func (ev *Event) EventStateTup() EventStateTup {
	if ev.StateKey == nil {
		panic("not a state event")
	}
	return EventStateTup{
		EventID: ev.ID,
		StateTup: StateTup{
			Type:     ev.Type,
			StateKey: *ev.StateKey,
		},
	}
}

func (ev *Event) StateTup() StateTup {
	if ev.StateKey == nil {
		panic("not a state event")
	}
	return StateTup{
		Type:     ev.Type,
		StateKey: *ev.StateKey,
	}
}

func (ev *Event) MembershipTup() MembershipTup {
	if ev.Type != event.StateMember {
		panic("not a member event")
	}
	return MembershipTup{
		EventID:    ev.ID,
		RoomID:     ev.RoomID,
		Membership: ev.Membership(),
	}
}

func (ev *Event) GetRoomVersion() gomatrixserverlib.RoomVersion {
	return gomatrixserverlib.RoomVersion(ev.RoomVersion)
}

// Create EventPDU instance for this event, note pointer so PDU can write
func (ev *Event) PDU() EventPDU {
	return EventPDU{ev, ev.GetRoomVersion()}
}

func (ev *Event) MustGetRoomSpec() gomatrixserverlib.IRoomVersion {
	roomSpec, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion())
	if err != nil {
		panic(err)
	}
	return roomSpec
}

func (ev *Event) IsProfileUpdate() bool {
	return gjson.GetBytes(ev.Content, "babbleserv.is_profile_update").Bool()
}

func (ev *Event) Membership() event.Membership {
	return event.Membership(gjson.GetBytes(ev.Content, "membership").String())
}

func (ev *Event) RelatesTo() (id.EventID, event.RelationType) {
	relatesTo := gjson.GetBytes(ev.Content, "m\\.relates_to")
	if !relatesTo.Exists() {
		return "", ""
	}

	rel := struct {
		Type    event.RelationType `json:"rel_type"`
		EventID id.EventID         `json:"event_id"`
	}{}

	json.Unmarshal([]byte(relatesTo.Raw), &rel)

	return rel.EventID, rel.Type
}

func (ev *Event) ReactionKey() string {
	if ev.Type != event.EventReaction {
		return ""
	}
	return gjson.GetBytes(ev.Content, "m\\.relates_to.key").String()
}

func (ev *Event) GetRedactedEvent() (*Event, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}

	if b, err = ev.MustGetRoomSpec().RedactEventJSON(b); err != nil {
		return nil, err
	}

	var redacted Event
	if err := json.Unmarshal(b, &redacted); err != nil {
		return nil, err
	}
	redacted.Redacted = true

	return &redacted, err
}
