package types

import (
	"encoding/json"
	"fmt"

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
	Timestamp int64           `msgpack:"ots" json:"origin_server_ts,omitzero"`

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

	// Internal state contexts before and after the event, zero when unknown
	BeforeState StateHash `msgpack:"bst" json:"-"`
	AfterState  StateHash `msgpack:"ast" json:"-"`

	// Only here for backwards compat ???
	PrevState []id.EventID `msgpack:"pst" json:"prev_state,omitzero"`

	PrevEventIDs []id.EventID `msgpack:"pid" json:"prev_events"`
	AuthEventIDs []id.EventID `msgpack:"aid" json:"auth_events"`

	Hashes     map[string]string            `msgpack:"hsh" json:"hashes"`
	Signatures map[string]map[string]string `msgpack:"sig" json:"signatures"`

	// Internal, in-memory only flags used for the lifetime of a request/background job
	ClientTransactionID string `msgpack:"-" json:"-"`
	IsForClientAPI      bool   `msgpack:"-" json:"-"`
	IsDuplicate         bool   `msgpack:"-" json:"-"`
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

func eventIDsFromProtoEvent(input any) ([]id.EventID, error) {
	var out []id.EventID
	switch ids := input.(type) {
	case nil:
	case []string:
		for _, value := range ids {
			out = append(out, id.EventID(value))
		}
	case []any:
		for _, value := range ids {
			eventID, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("unsupported proto-event reference %T", value)
			}
			out = append(out, id.EventID(eventID))
		}
	default:
		return nil, fmt.Errorf("unsupported proto-event references %T", input)
	}
	return out, nil
}

func EventFromProtoEvent(protoEv gomatrixserverlib.ProtoEvent) (*Event, error) {
	authEventIDs, err := eventIDsFromProtoEvent(protoEv.AuthEvents)
	if err != nil {
		return nil, err
	}
	prevEventIDs, err := eventIDsFromProtoEvent(protoEv.PrevEvents)
	if err != nil {
		return nil, err
	}
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
		AuthEventIDs: authEventIDs,
		PrevEventIDs: prevEventIDs,
	}, nil
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
	roomSpec, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion())
	domainlessCreate := ev.Type == event.StateCreate && err == nil && roomSpec.DomainlessRoomIDs()
	if ev.IsForClientAPI {
		// Client API removes room_id, adds event_id
		b := exerrors.Must(json.Marshal(ev.PartialEvent))
		b = exerrors.Must(sjson.DeleteBytes(b, "room_id"))
		// Clients need the derived room ID even though v12 create PDUs omit it.
		if domainlessCreate {
			b = exerrors.Must(sjson.SetBytes(b, "room_id", ev.RoomID))
		}
		b = exerrors.Must(sjson.SetBytes(b, "event_id", ev.ID))
		// Only locally looked-up metadata may expose a client transaction ID.
		b = exerrors.Must(sjson.DeleteBytes(b, "unsigned.transaction_id"))
		if ev.ClientTransactionID != "" {
			b = exerrors.Must(sjson.SetBytes(b, "unsigned.transaction_id", ev.ClientTransactionID))
		}
		return b, nil
	}

	if ev.AuthEventIDs == nil {
		ev.AuthEventIDs = make([]id.EventID, 0)
	}
	if ev.PrevEventIDs == nil {
		ev.PrevEventIDs = make([]id.EventID, 0)
	}
	b := exerrors.Must(json.Marshal((marshalEvent)(ev)))
	if domainlessCreate {
		b = exerrors.Must(sjson.DeleteBytes(b, "room_id"))
	}
	// Strip unsigned from any S2S API calls
	b = exerrors.Must(sjson.DeleteBytes(b, "unsigned"))
	return b, nil
}

func (ev *Event) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, (*marshalEvent)(ev)); err != nil {
		return err
	}
	if ev.Type != event.StateCreate {
		return nil
	}
	roomVersion := ev.RoomVersion
	if roomVersion == "" {
		roomVersion = gjson.GetBytes(ev.Content, "room_version").String()
	}
	roomSpec, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	if err != nil || !roomSpec.DomainlessRoomIDs() {
		return nil
	}
	ev.RoomVersion = roomVersion
	if gjson.GetBytes(b, "room_id").Exists() {
		return fmt.Errorf("v12 create event must omit room_id")
	}
	if ev.StateKey == nil || *ev.StateKey != "" {
		return fmt.Errorf("create event must have an empty state key")
	}
	pdu, err := roomSpec.NewEventFromTrustedJSON(b, false)
	if err != nil {
		return err
	}
	ev.RoomID = id.RoomID(pdu.RoomID().String())
	return nil
}

func (ev *Event) EventTup() EventTup {
	return EventTup{
		EventID: ev.ID,
		RoomID:  ev.RoomID,
		Sender:  ev.Sender,
		Type:    ev.Type,
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

// StateEntry is what a state context holds for the state event
func (ev *Event) StateEntry() StateEntry {
	entry := StateEntry{EventID: ev.ID}
	if ev.Type == event.StateMember {
		entry.Membership = ev.Membership()
	}
	return entry
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
	return gjson.GetBytes(ev.Content, `babbleserv\.is_profile_update`).Bool()
}

func (ev *Event) Membership() event.Membership {
	return event.Membership(gjson.GetBytes(ev.Content, "membership").String())
}

func (ev *Event) Mentions() (m event.Mentions) {
	json.Unmarshal([]byte(gjson.GetBytes(ev.Content, "m\\.mentions").Raw), &m)
	return m
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
	redacted.RoomVersion, redacted.RoomID, redacted.ID = ev.RoomVersion, ev.RoomID, ev.ID
	redacted.Redacted = true

	return &redacted, err
}

// ImplicitCreateEventID is the create event committed to by a domainless room ID.
func (ev *Event) ImplicitCreateEventID() id.EventID {
	roomSpec, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion())
	if err != nil || !roomSpec.DomainlessRoomIDs() || len(ev.RoomID) < 2 || ev.RoomID[0] != '!' {
		return ""
	}
	return id.EventID("$" + ev.RoomID[1:])
}

// DomainlessRoomID is the room ID a create event commits to, the inverse of ImplicitCreateEventID.
func (ev *Event) DomainlessRoomID() id.RoomID {
	roomSpec, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion())
	if err != nil || !roomSpec.DomainlessRoomIDs() || ev.Type != event.StateCreate || len(ev.ID) < 2 || ev.ID[0] != '$' {
		return ""
	}
	return id.RoomID("!" + ev.ID[1:])
}

// AuthDependencyIDs are the event's auth events, preceded by the create event that it cites
// implicitly in a room with a domainless room ID.
func (ev *Event) AuthDependencyIDs() []id.EventID {
	if createID := ev.ImplicitCreateEventID(); createID != "" && ev.Type != event.StateCreate {
		return append([]id.EventID{createID}, ev.AuthEventIDs...)
	}
	return ev.AuthEventIDs
}

// HasStateIn reports whether the event, which may be nil, is in the room with known state contexts.
// BeforeState and AfterState are always set together, and an outlier never has them.
func (ev *Event) HasStateIn(roomID id.RoomID) bool {
	return ev != nil && !ev.Outlier && ev.RoomID == roomID && !ev.AfterState.IsZero()
}

// ResetOutcome clears what authorizing the event decided: its rejection, soft failure and state.
func (ev *Event) ResetOutcome() {
	ev.Rejected, ev.SoftFailed = false, false
	ev.BeforeState, ev.AfterState = StateHash{}, StateHash{}
}

// CopyOutcome gives the event what authorizing another copy of it decided.
func (ev *Event) CopyOutcome(from *Event) {
	ev.Rejected, ev.SoftFailed = from.Rejected, from.SoftFailed
	ev.BeforeState, ev.AfterState = from.BeforeState, from.AfterState
}
