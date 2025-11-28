package types

import (
	"encoding/json"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type EDUType string

var (
	EDUTypeToDevice EDUType = "m.direct_to_device"
	EDUTypeReceipt  EDUType = "m.receipt"
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
