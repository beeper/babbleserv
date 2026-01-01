package types

import (
	"encoding/json"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type EDUType string

var (
	EDUTypeToDevice         EDUType = "m.direct_to_device"
	EDUTypeReceipt          EDUType = "m.receipt"
	EDUTypeDeviceListUpdate EDUType = "m.device_list_update"
	EDUTypeSigningKeyUpdate EDUType = "m.signing_key_update"
	EDUTypePresence         EDUType = "m.presence"
)

type EDU struct {
	Type    EDUType         `json:"edu_type"`
	Content json.RawMessage `json:"content"`
}

type ReceiptEDUData struct {
	ThreadID string `json:"thread_id"`
	TS       int64  `json:"ts"`
}

type ReceiptEDUContent map[id.RoomID]map[event.ReceiptType]map[id.UserID]struct {
	EventIDs []id.EventID   `json:"event_ids"`
	Data     ReceiptEDUData `json:"data"`
}

// UserID -> DeviceID -> content
type ToDeviceEDUMessages map[id.UserID]map[id.DeviceID]map[string]any

type ToDeviceEDUContent struct {
	MessageID string              `json:"message_id"`
	Messages  ToDeviceEDUMessages `json:"messages"`
	Sender    id.UserID           `json:"sender"`
	Type      event.Type          `json:"type"`
}

// TODO: move to mautrix
type SigningKeyUpdateEDUContent struct {
	UserID      id.UserID                `json:"user_id"`
	MasterKey   mautrix.CrossSigningKeys `json:"master_key"`
	SelfSigning mautrix.CrossSigningKeys `json:"self_signing_key"`
}

// TODO: move to mautrix
type DeviceListUpdateEDUContent struct {
	DeviceID id.DeviceID `json:"device_id"`
	UserID   id.UserID   `json:"user_id"`
	StreamID int64       `json:"stream_id"`

	PrevID            []int64             `json:"prev_id,omitzero"`
	DeviceKeys        *mautrix.DeviceKeys `json:"keys,omitempty"`
	Deleted           bool                `json:"deleted,omitzero"`
	DeviceDisplayName string              `json:"device_display_name,omitzero"`
}

type PresenceEDUItem struct {
	UserID          id.UserID      `json:"user_id"`
	Presence        event.Presence `json:"presence"`
	StatusMsg       string         `json:"status_msg,omitempty"`
	LastActiveAgo   int64          `json:"last_active_ago,omitempty"`
	CurrentlyActive bool           `json:"currently_active,omitempty"`
}

type PresenceEDUContent struct {
	Push []PresenceEDUItem `json:"push"`
}
