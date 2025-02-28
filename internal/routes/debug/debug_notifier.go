package debug

import (
	"net/http"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugSendNotifierChange(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	change := notifier.Change{}

	if query.Has("event_id") {
		change.EventIDs = []id.EventID{id.EventID(query.Get("event_id"))}
	}
	if query.Has("room_id") {
		change.RoomIDs = []id.RoomID{id.RoomID(query.Get("room_id"))}
	}
	if query.Has("user_id") {
		change.UserIDs = []id.UserID{id.UserID(query.Get("user_id"))}
	}
	if query.Has("server") {
		change.Servers = []string{query.Get("server")}
	}

	if b.notifiers.Rooms != nil {
		b.notifiers.Rooms.SendChange(change)
	}
	if b.notifiers.Accounts != nil {
		b.notifiers.Accounts.SendChange(change)
	}
	if b.notifiers.Transient != nil {
		b.notifiers.Transient.SendChange(change)
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
