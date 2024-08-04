package types

import (
	"maunium.net/go/mautrix/id"
)

type User struct {
	Username   string
	ServerName string
}

func (r *User) UserID() id.UserID {
	return id.UserID("@" + r.Username + ":" + r.ServerName)
}
