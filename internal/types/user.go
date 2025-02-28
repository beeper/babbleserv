package types

import (
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type UserDevice struct {
	UserID   id.UserID
	DeviceID id.DeviceID
}

type User struct {
	// Populated at fetch time
	Username   string `msgpack:"-"`
	ServerName string `msgpack:"-"`

	Email string `msgpack:"em"`

	CreatedAt time.Time `msgpack:"ct"`
}

func NewUserFromBytes(b []byte, username, serverName string) (*User, error) {
	var u User
	if err := msgpack.Unmarshal(b, &u); err != nil {
		return nil, err
	}
	u.Username = username
	u.ServerName = serverName
	return &u, nil
}

func MustNewUserFromBytes(b []byte, username, serverName string) *User {
	if u, err := NewUserFromBytes(b, username, serverName); err != nil {
		panic(err)
	} else {
		return u
	}
}

func (u *User) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(u); err != nil {
		panic(err)
	} else {
		return b
	}
}

func (r *User) UserID() id.UserID {
	return id.UserID("@" + r.Username + ":" + r.ServerName)
}
