package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

type Device struct {
	ID          id.DeviceID `msgpack:"id" json:"device_id"`
	DisplayName string      `msgpack:"dn" json:"display_name"`
}

type UserDevice struct {
	UserID   id.UserID
	DeviceID id.DeviceID
}

type DeviceListStream struct {
	StreamID int64
	PrevID   int64
}

type UserDeviceChange struct {
	UserDevice
	Version tuple.Versionstamp
	// Set when the change allocated a local device-list version
	Stream *DeviceListStream
}

// One read of a local device for its device-list EDU; Device nil when deleted
type LocalDeviceListUpdate struct {
	Version int64
	Device  *Device
	Keys    *mautrix.DeviceKeys
}

type LocalSnapshotDevice struct {
	Device Device
	Keys   *mautrix.DeviceKeys
}

// A local user's device list as one read: what /user/devices and join announcements serve
type LocalDeviceSnapshot struct {
	Version        int64
	Devices        []LocalSnapshotDevice
	MasterKey      *mautrix.CrossSigningKeys
	SelfSigningKey *mautrix.CrossSigningKeys
}

func NewDevice(id id.DeviceID, displayName string) *Device {
	return &Device{
		ID:          id,
		DisplayName: displayName,
	}
}

func NewDeviceFromBytes(b []byte) (*Device, error) {
	var d Device
	if err := msgpack.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func MustNewDeviceFromBytes(b []byte) *Device {
	if d, err := NewDeviceFromBytes(b); err != nil {
		panic(err)
	} else {
		return d
	}
}

func (d *Device) ToBytes() []byte {
	if b, err := msgpack.Marshal(d); err != nil {
		panic(err)
	} else {
		return b
	}
}
