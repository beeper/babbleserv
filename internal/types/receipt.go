package types

import (
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Receipt struct {
	RoomID   id.RoomID
	Type     event.ReceiptType
	ThreadID string
	UserID   id.UserID

	EventID id.EventID
	Data    []byte
}
