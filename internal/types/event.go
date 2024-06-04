package types

import (
	"encoding/json"

	"github.com/thanhpk/randstr"
	"github.com/tidwall/gjson"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Event struct {
	ID     id.EventID `json:"event_id" msgpack:"eid"`
	RoomID id.RoomID  `json:"room_id" msgpack:"rid"`
	Sender id.UserID  `json:"sender" msgpack:"sdr"`

	SoftFailed bool `json:"-" msgpack:"sfd"`

	Type    event.Type `json:"-" msgpack:"-"`
	TypeStr string     `json:"type" msgpack:"typ"`

	StateKey    *string `json:"state_key,omitempty" msgpack:"sky"` // stored in FDB
	StateKeyStr string  `json:"-" msgpack:"-"`                     // not stored in FDB

	Content json.RawMessage `json:"content" msgpack:"cnt"`

	PrevEventIDs []id.EventID `json:"prev_events" msgpack:"pids"`
	AuthEventIDs []id.EventID `json:"auth_events" msgpack:"aids"`

	Signatures json.RawMessage `json:"signatures" msgpack:"-"`
}

func (ev *Event) init() {
	ev.Type = event.NewEventType(ev.TypeStr)

	if ev.StateKey == nil {
		ev.StateKeyStr = ""
	} else {
		ev.StateKeyStr = *ev.StateKey
	}

	if ev.PrevEventIDs == nil {
		ev.PrevEventIDs = make([]id.EventID, 0)
	}
	if ev.AuthEventIDs == nil {
		ev.AuthEventIDs = make([]id.EventID, 0)
	}
}

func NewEventFromBytes(b []byte) (*Event, error) {
	var ev Event
	if err := msgpack.Unmarshal(b, &ev); err != nil {
		return nil, err
	}
	ev.init()
	return &ev, nil
}

func MustNewEventFromBytes(b []byte) *Event {
	ev, err := NewEventFromBytes(b)
	if err != nil {
		panic(err)
	}
	return ev
}

func NewEventFromMautrix(mev *event.Event) *Event {
	ev := &Event{
		ID:       mev.ID,
		RoomID:   mev.RoomID,
		TypeStr:  mev.Type.String(),
		Sender:   mev.Sender,
		Content:  mev.Content.VeryRaw,
		StateKey: mev.StateKey,
	}
	ev.init()
	return ev
}

func NewEventRawContent(roomID id.RoomID, evType event.Type, stateKey *string, sender id.UserID, content json.RawMessage) *Event {
	evID := randstr.Hex(32)

	ev := &Event{
		ID:       id.EventID("$" + evID),
		RoomID:   roomID,
		TypeStr:  evType.String(),
		StateKey: stateKey,
		Sender:   sender,
		Content:  content,
	}
	ev.init()
	return ev
}

func NewEvent(roomID id.RoomID, evType event.Type, stateKey *string, sender id.UserID, content map[string]any) *Event {
	contentBytes, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	return NewEventRawContent(roomID, evType, stateKey, sender, contentBytes)
}

func (e *Event) ToJSON() []byte {
	if bytes, err := json.Marshal(e); err != nil {
		panic(err)
	} else {
		return bytes
	}
}

func (e *Event) ToMsgpack() []byte {
	if bytes, err := msgpack.Marshal(e); err != nil {
		panic(err)
	} else {
		return bytes
	}
}

// Create EventPDU instance for this event, note pointer so PDU can write
func (e *Event) PDU() EventPDU {
	return EventPDU{e}
}

func (e *Event) Membership() event.Membership {
	return event.Membership(gjson.GetBytes(e.Content, "membership").String())
}
