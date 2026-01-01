package types

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Custom internal to-device events
//

// We smuggle device list and signing key updates within the user/server to-device versions. We use
// custom types to identify these just in case someone tries to send a to-device message with an
// event type m.device_list_update or m.signing_key_update.
var (
	// These are for local user device_lists.changed sync field
	BabbleservLocalDeviceChange = event.Type{
		Type: "babbleserv.local_device_change",
	}
	// These are for local user device_lists.left sync field
	BabbleservLocalDeviceLeft = event.Type{
		Type: "babbleserv.local_device_left",
	}
	// These are for local user presence updates in sync
	BabbleservLocalPresenceChange = event.Type{
		Type: "babbleserv.local_presence_change",
	}
	// These are turned into federation m.device_list_update EDUs
	BabbleservRemoteDeviceListUpdate = event.Type{
		Type: "babbleserv.remote_device_list_update",
	}
	// These are turned into federation m.signing_key_update EDUs
	BabbleservRemoteSigningKeyUpdate = event.Type{
		Type: "babbleserv.remote_signing_key_update",
	}
	// These are turned into federation m.presence EDUs
	BabbleservRemotePresenceChange = event.Type{
		Type: "babbleserv.remote_presence_change",
	}
	// These are turned into federation PDUs where the other HS is not in the room to workaround the
	// federation sender ignoring rooms as soon as the HS leaves (ie to rescind invites).
	BabbleservRemoteOutlierEvent = event.Type{
		Type: "babbleserv_remote_outlier_event",
	}
)

type ToDevice struct {
	UserID   id.UserID
	DeviceID id.DeviceID
	Sender   id.UserID
	Type     event.Type
	Content  json.RawMessage
}
type ToDeviceWithVersion struct {
	ToDevice
	Version tuple.Versionstamp
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

func (t *ToDevice) ToPartialEvent() *PartialEvent {
	var content map[string]any
	json.Unmarshal(t.Content, &content)
	return NewPartialEvent("", t.Type, nil, t.Sender, content)
}
