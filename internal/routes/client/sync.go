package client

import (
	"maps"
	"net/http"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) Sync(w http.ResponseWriter, r *http.Request) {
	versions, err := util.VersionMapFromRequestQuery(r, "since")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	limit, err := util.IntFromRequestQuery(r, "limit", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	userID := middleware.GetRequestUserID(r)

	var sync *types.Sync

	if len(versions) == 0 {
		if sync, err = c.db.InitForUser(r.Context(), userID); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	} else {
		// TODO: cache these?
		rooms, err := c.db.Rooms.GetUserMemberships(r.Context(), userID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		changeCh := make(chan any, 1)
		c.notifiers.Subscribe(changeCh, notifier.Subscription{
			UserIDs: []id.UserID{userID},
			RoomIDs: slices.Collect(maps.Keys(rooms)),
		})
		defer c.notifiers.Unsubscribe(changeCh)

		sync, err = c.db.SyncForUser(r.Context(), userID, versions, databases.SyncOptions{
			Limit: limit,
		})
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		// If sync is empty, wait on notifier, re-sync
		if sync.IsEmpty() {
			<-changeCh
			sync, err = c.db.SyncForUser(r.Context(), userID, versions, databases.SyncOptions{
				Limit: limit,
			})
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, sync)
	return
}
