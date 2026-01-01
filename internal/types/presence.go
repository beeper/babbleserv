package types

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Presence struct {
	UserID     id.UserID
	Presence   event.Presence
	Message    string
	LastActive time.Time
}

type PresenceChange struct {
	Presence
	Version tuple.Versionstamp
}

func (p Presence) ToBytes() []byte {
	return tuple.Tuple{string(p.Presence), p.Message, p.LastActive.UnixMilli()}.Pack()
}

func MustNewPresenceFromBytes(b []byte, userID id.UserID) *Presence {
	tup, err := tuple.Unpack(b)
	if err != nil {
		panic(err)
	}
	lastActive := time.Unix(0, tup[2].(int64)*int64(time.Millisecond))
	return &Presence{
		UserID:     userID,
		Presence:   event.Presence(tup[0].(string)),
		Message:    tup[1].(string),
		LastActive: lastActive,
	}
}
