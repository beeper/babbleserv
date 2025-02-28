package types

import (
	"encoding/json"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var _ json.Marshaler = (*Event)(nil)
var _ json.Unmarshaler = (*Event)(nil)
var _ msgpack.Marshaler = (*Event)(nil)

type PartialEvent struct {
	RoomID   id.RoomID       `msgpack:"rid" json:"room_id"`
	Sender   id.UserID       `msgpack:"sdr" json:"sender,omitempty"`
	StateKey *string         `msgpack:"sky" json:"state_key,omitempty"`
	Content  json.RawMessage `msgpack:"cnt" json:"content"`
	Redacts  id.EventID      `msgpack:"rds" json:"redacts,omitempty"` // room <v10

	// event.Type doesn't implement msgpack marshalling, so we use TypeStr
	TypeStr string     `msgpack:"typ" json:"-"`
	Type    event.Type `msgpack:"-" json:"type"`
}

type Event struct {
	PartialEvent `msgpack:",inline" json:",inline"`

	// Event ID is not part of the (S2S) event JSON, populated at fetch (from key)
	ID id.EventID `msgpack:"-" json:"-"`

	// Internal copy of the room version so we don't need to look it up
	RoomVersion string `msgpack:"rmv" json:"-"`
	// Internal indicators of whether an event is soft failed or an outlier,
	// if so it should not appear in any indices or user facing responses.
	SoftFailed bool `msgpack:"sfd" json:"-"`
	Outlier    bool `msgpack:"out" json:"-"`
	// Internal indicator of whether the event has been redacted - note the
	// actual content will not be redacted in the DB.
	Redacted bool `msgpack:"red" json:"-"`

	Origin    string `msgpack:"ori" json:"origin"`
	Timestamp int64  `msgpack:"ots" json:"origin_server_ts"`

	Depth int64 `msgpack:"dpt" json:"depth"`

	// Only here for backwards compat
	PrevState []id.EventID `msgpack:"pst" json:"prev_state,omitempty"`

	PrevEventIDs []id.EventID `msgpack:"pid" json:"prev_events"`
	AuthEventIDs []id.EventID `msgpack:"aid" json:"auth_events"`

	Hashes     map[string]string            `msgpack:"hsh" json:"hashes"`
	Signatures map[string]map[string]string `msgpack:"sig" json:"signatures"`

	Unsigned map[string]any `msgpack:"uns" json:"unsigned,omitempty"`
}

func NewEventFromBytes(b []byte, id id.EventID) (*Event, error) {
	var ev Event
	if err := msgpack.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	ev.ID = id
	ev.Type = event.NewEventType(ev.TypeStr)
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

// Wrapper around msgpack marshalling to set TypeStr
func (ev Event) MarshalMsgpack() ([]byte, error) {
	ev.TypeStr = ev.Type.Type
	return msgpack.Marshal((marshalEvent)(ev))
}

func (ev *Event) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(ev); err != nil {
		panic(err)
	} else {
		return b
	}
}

func (ev Event) MarshalJSON() ([]byte, error) {
	if ev.AuthEventIDs == nil {
		ev.AuthEventIDs = make([]id.EventID, 0)
	}
	if ev.PrevEventIDs == nil {
		ev.PrevEventIDs = make([]id.EventID, 0)
	}
	b, err := json.Marshal((marshalEvent)(ev))
	if ev.PrevState != nil {
		// Work around no omitnil in Go's JSON marshaller
		b, err = sjson.SetBytes(b, "prev_state", []string{})
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (ev *Event) UnmarshalJSON(b []byte) error {
	err := json.Unmarshal(b, (*marshalEvent)(ev))
	if err != nil {
		return err
	}
	if gjson.GetBytes(b, "prev_state").Exists() {
		// Work around no omitnil in Go's JSON unmarshaller
		ev.PrevState = []id.EventID{}
	} else {
		ev.PrevState = nil
	}
	return nil
}

// Wrapper around event for the CS API where event_id is part of the JSON
var _ json.Marshaler = (*ClientEvent)(nil)
var _ json.Unmarshaler = (*ClientEvent)(nil)

type ClientEvent struct {
	*Event `json:",inline"`
}

func (ev *Event) ClientEvent() ClientEvent {
	return ClientEvent{ev}
}

type marshalClientEvent ClientEvent

func (ev ClientEvent) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal((marshalClientEvent)(ev))
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(b, "event_id", ev.ID)
}

func (ev *Event) EventIDTup() EventIDTup {
	return EventIDTup{
		EventID: ev.ID,
		RoomID:  ev.RoomID,
	}
}

func (ev *Event) StateTupWithID() StateTupWithID {
	if ev.StateKey == nil {
		panic("not a state event")
	}
	return StateTupWithID{
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
