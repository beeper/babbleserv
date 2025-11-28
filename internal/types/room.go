package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type Room struct {
	// Populated at fetch time from the FDB key
	ID id.RoomID `json:"room_id" msgpack:"-"`

	// Matrix room version
	Version string `json:"version" msgpack:"ver"`

	Name      string `msgpack:"nme" json:"name" `
	Type      string `msgpack:"typ" json:"type"`
	Topic     string `msgpack:"tpc" json:"topic"`
	AvatarURL string `msgpack:"aul" json:"avatar_url"`

	CanonicalAlias string `json:"canonical_alias" msgpack:"cas"`

	MemberCount int `json:"members" msgpack:"mem"`

	Public    bool `json:"is_public" msgpack:"pub"`
	Federated bool `json:"is_federated" msgpack:"fed"`
}

func NewRoomFromBytes(b []byte, id id.RoomID) (*Room, error) {
	var rm Room
	if err := msgpack.Unmarshal(b, &rm); err != nil {
		return nil, err
	}
	rm.ID = id
	return &rm, nil
}

func MustNewRoomFromBytes(b []byte, id id.RoomID) *Room {
	if r, err := NewRoomFromBytes(b, id); err != nil {
		panic(err)
	} else {
		return r
	}
}

func (r *Room) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(r); err != nil {
		panic(err)
	} else {
		return b
	}
}

func RoomDepthToBytes(depth int64) []byte {
	return tuple.Tuple{depth}.Pack()
}

func BytesToRoomDepth(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	tup, _ := tuple.Unpack(b)
	return tup[0].(int64)
}
