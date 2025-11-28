package types

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Note we smuggle device list and signing key updates within the user/server to-device versions. We
// use custom types to identify these just in case someone tries to send a to-device message with an
// event type m.device_list_update or m.signing_key_update.
var (
	// To device events of this type are popped and turned into m.device_list_update EDUs or sync
	// device_lists content.
	InternalDeviceListUpdate = event.Type{
		Type: "babbleserv.device_list_update",
	}
	// To device events of this type are popped and turned into m.signing_key_update EDUs
	InternalSigningKeyUpdate = event.Type{
		Type: "babbleserv.signing_key_update",
	}
)

type ToDevice struct {
	UserID   id.UserID
	DeviceID id.DeviceID
	Sender   id.UserID
	Type     event.Type
	Content  json.RawMessage
}

func (t *ToDevice) Bytes() []byte {
	return tuple.Tuple{
		t.UserID.String(),
		t.DeviceID.String(),
		t.Sender.String(),
		t.Type.String(),
		[]byte(t.Content),
	}.Pack()
}

func BytesToToDevice(b []byte) (*ToDevice, error) {
	tup, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	return &ToDevice{
		UserID:   id.UserID(tup[0].(string)),
		DeviceID: id.DeviceID(tup[1].(string)),
		Sender:   id.UserID(tup[2].(string)),
		Type:     event.NewEventType(tup[3].(string)),
		Content:  tup[4].([]byte),
	}, nil
}

func MustBytesToToDevice(b []byte) *ToDevice {
	t, err := BytesToToDevice(b)
	if err != nil {
		panic(err)
	}
	return t
}

func (t *ToDevice) ToEvent() *PartialEvent {
	var content map[string]any
	json.Unmarshal(t.Content, &content)
	return NewPartialEvent("", t.Type, nil, t.Sender, content)
}
