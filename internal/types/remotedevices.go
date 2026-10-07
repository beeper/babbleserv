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

// RemoteDeviceCache is a tracked user's valid snapshot with every accepted delta applied
type RemoteDeviceCache struct {
	UserID id.UserID
	RemoteDeviceSnapshot
}

type RemoteDeviceIngestResult struct {
	// The cache was cleared and a refetch scheduled
	Deleted bool
	// A change record was written, local clients will be told
	Notified bool
}

type RemoteDeviceTrackingChange struct {
	UserID  id.UserID
	RoomID  id.RoomID
	Tracked bool
}

type RemoteDeviceTrackingResult struct {
	Started []id.UserID
	Stopped []id.UserID
}

type RemoteDeviceJob struct {
	UserID   id.UserID
	Identity int64
	DueAt    time.Time
	Attempts int
}

// RemoteDeviceFetch is a claimed job and the generation read with the claim. Job.DueAt is the lease.
type RemoteDeviceFetch struct {
	Job        RemoteDeviceJob
	Generation int64
}

type RemoteDevicePublishOutcome string

const (
	RemoteDevicePublished         RemoteDevicePublishOutcome = "published"
	RemoteDevicePublishedFollowUp RemoteDevicePublishOutcome = "published_follow_up"
	RemoteDevicePublishCompleted  RemoteDevicePublishOutcome = "completed_live_cache"
	RemoteDevicePublishRetry      RemoteDevicePublishOutcome = "retry"
	RemoteDevicePublishSuperseded RemoteDevicePublishOutcome = "superseded"
)
