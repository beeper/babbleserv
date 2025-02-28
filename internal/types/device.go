package types

import (
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type Device struct {
	ID id.DeviceID

	DisplayName string
}

func NewDevice(id id.DeviceID, displayName string) *Device {
	return &Device{
		ID:          id,
		DisplayName: displayName,
	}
}

func NewDeviceFromBytes(b []byte, id id.DeviceID) (*Device, error) {
	var d Device
	if err := msgpack.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.ID = id
	return &d, nil
}

func MustNewDeviceFromBytes(b []byte, id id.DeviceID) *Device {
	if d, err := NewDeviceFromBytes(b, id); err != nil {
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
