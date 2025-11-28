package types

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type AccountDataTup struct {
	UserID id.UserID
	RoomID id.RoomID
	Type   event.Type
}

type AccountData struct {
	AccountDataTup
	Content json.RawMessage
}

func (ad *AccountData) ToBytes() []byte {
	return tuple.Tuple{
		ad.UserID.String(),
		ad.RoomID.String(),
		ad.Type.String(),
		[]byte(ad.Content),
	}.Pack()
}

func BytesToAccountData(b []byte) (*AccountData, error) {
	tup, err := tuple.Unpack(b)
	if err != nil {
		return nil, err
	}
	return &AccountData{
		AccountDataTup: AccountDataTup{
			UserID: id.UserID(tup[0].(string)),
			RoomID: id.RoomID(tup[1].(string)),
			Type:   event.NewEventType(tup[2].(string)),
		},
		Content: tup[3].([]byte),
	}, nil
}

func MustBytesToAccountData(b []byte) *AccountData {
	a, err := BytesToAccountData(b)
	if err != nil {
		panic(err)
	}
	return a
}
