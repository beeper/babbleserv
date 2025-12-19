package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
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

type UserDeviceChange struct {
	UserDevice
	Version tuple.Versionstamp
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

func (d *Device) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(d); err != nil {
		panic(err)
	} else {
		return b
	}
}
