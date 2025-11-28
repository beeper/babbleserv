package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type ReceiptTup struct {
	UserID   id.UserID         `json:"user_id"`
	RoomID   id.RoomID         `json:"room_id"`
	Type     event.ReceiptType `json:"type"`
	ThreadID string            `json:"thread_id,omitempty"`
}

type Receipt struct {
	ReceiptTup   `json:",inline"`
	EventID      id.EventID `json:"event_id"`
	EventVersion Version    `json:"event_version"`
	Timestamp    int64      `json:"ts"`
}

type ReceiptWithVersion struct {
	Receipt
	Version tuple.Versionstamp
}

func (r ReceiptWithVersion) GetVersion() tuple.Versionstamp {
	return r.Version
}

func (rc *Receipt) ToBytes() []byte {
	return tuple.Tuple{
		rc.RoomID.String(),
		string(rc.Type),
		rc.ThreadID,
		rc.UserID.String(),
		rc.EventID.String(),
		tuple.Versionstamp(rc.EventVersion),
		rc.Timestamp,
	}.Pack()
}

func BytesToReceipt(b []byte) (*Receipt, error) {
	tup, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	return &Receipt{
		ReceiptTup: ReceiptTup{
			RoomID:   id.RoomID(tup[0].(string)),
			Type:     event.ReceiptType(tup[1].(string)),
			ThreadID: tup[2].(string),
			UserID:   id.UserID(tup[3].(string)),
		},
		EventID:      id.EventID(tup[4].(string)),
		EventVersion: Version(tup[5].(tuple.Versionstamp)),
		Timestamp:    tup[6].(int64),
	}, nil
}

func MustBytesToReceipt(b []byte) *Receipt {
	r, err := BytesToReceipt(b)
	if err != nil {
		panic(err)
	}
	return r
}
