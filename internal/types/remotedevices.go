package types

import (
	"encoding/json"
	"time"

	"maunium.net/go/mautrix/id"
)

const (
	MaxRemoteDevicesPerUser         = 1000
	MaxRemoteDeviceSnapshotBytes    = 4 << 20
	MaxRemoteKeyObjectBytes         = 64 << 10
	MaxRemoteDeviceDisplayNameBytes = 256
	MaxRemoteDeviceIDBytes          = 512
	MaxRemoteDevicePrevIDs          = 10
	MaxRemoteDeviceEDUsPerIngest    = 100
	MaxRemoteDeviceStreamHistory    = 200

	RemoteDeviceStreamHistoryRetention = 30 * time.Minute
	RemoteDeviceCacheRetention         = 30 * 24 * time.Hour
)

// RemoteDevice is a remote user's device as the cache stores it. Keys is canonical JSON without
// unsigned and with only the owner's signatures, nil for a device without E2E keys.
type RemoteDevice struct {
	DisplayName string
	Keys        json.RawMessage
}

type RemoteDeviceListUpdate struct {
	DeviceID id.DeviceID
	StreamID int64
	PrevIDs  []int64
	Deleted  bool
	Device   RemoteDevice
	// The payload failed validation: its stream ID is never accepted
	Invalid bool
}

// Nil keys were omitted, or dropped as invalid with Invalid set
type RemoteSigningKeyUpdate struct {
	MasterKey      json.RawMessage
	SelfSigningKey json.RawMessage
	Invalid        bool
}

// RemoteDeviceEDU is one of a user's device-list or signing-key EDUs, in received order
type RemoteDeviceEDU struct {
	DeviceList  *RemoteDeviceListUpdate
	SigningKeys *RemoteSigningKeyUpdate
}

type RemoteDeviceSnapshot struct {
	StreamID       int64
	Devices        map[id.DeviceID]RemoteDevice
	MasterKey      json.RawMessage
	SelfSigningKey json.RawMessage
}

// RemoteDeviceCache is a cached user's valid snapshot with every accepted delta applied
type RemoteDeviceCache struct {
	UserID id.UserID
	RemoteDeviceSnapshot
}

// RemoteDeviceJob is a due snapshot fetch and the generation its snapshot must be published against
type RemoteDeviceJob struct {
	UserID     id.UserID
	DueAt      time.Time
	Attempts   int
	Generation int64
}
