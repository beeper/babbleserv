package types

import (
	"github.com/vmihailenco/msgpack/v5"
)

// Notifications represents notification and highlight counts for a single event.
// Stored as deltas per event version, summed for total counts.
type Notifications struct {
	Count     int    `msgpack:"n"` // notification count delta (+1 for notifying event)
	Highlight int    `msgpack:"h"` // highlight count delta (+1 if highlighted)
	ThreadID  string `msgpack:"t"`
}

func (n Notifications) IsEmpty() bool {
	return n.Count == 0 && n.Highlight == 0
}

func NotificationsToBytes(n Notifications) []byte {
	b, err := msgpack.Marshal(n)
	if err != nil {
		panic(err)
	}
	return b
}

func BytesToNotifications(b []byte) Notifications {
	var n Notifications
	if err := msgpack.Unmarshal(b, &n); err != nil {
		panic(err)
	}
	return n
}
