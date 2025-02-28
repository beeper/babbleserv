package types

import (
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type Room struct {
	// Populated at fetch time from the FDB key
	ID id.RoomID `json:"room_id" msgpack:"-"`

	// Matrix room version
	Version string `json:"version" msgpack:"ver"`

	CurrentDepth int64 `msgpack:"dpt"`

	Name      string `json:"name" msgpack:"nme"`
	Type      string `json:"type"`
	Topic     string `json:"topic" msgpack:"tpc"`
	AvatarURL string `json:"avatar_url" msgpack:"aul"`

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
