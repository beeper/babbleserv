package types

import (
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type User struct {
	Username string `json:"username" msgpack:"un"`
}

func NewUserFromBytes(b []byte) (*User, error) {
	var u User
	if err := msgpack.Unmarshal(b, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func MustNewUserFromBytes(b []byte) *User {
	r, err := NewUserFromBytes(b)
	if err != nil {
		panic(err)
	}
	return r
}

func (r *User) MustToMsgpack() []byte {
	if bytes, err := msgpack.Marshal(r); err != nil {
		panic(err)
	} else {
		return bytes
	}
}

func (r *User) UserID(homeserver string) id.UserID {
	return id.UserID("@" + r.Username + ":" + homeserver)
}
