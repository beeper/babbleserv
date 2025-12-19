package client

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) SyncLegacy(w http.ResponseWriter, r *http.Request) {
	c.doSyncWithMode(w, r, types.SyncModeLegacy)
}

func (c *ClientRoutes) SyncStreaming(w http.ResponseWriter, r *http.Request) {
	c.doSyncWithMode(w, r, types.SyncModeStreaming)
}

func (c *ClientRoutes) SyncSliding(w http.ResponseWriter, r *http.Request) {
	c.doSyncWithMode(w, r, types.SyncModeSliding)
}

func (c *ClientRoutes) doSyncWithMode(w http.ResponseWriter, r *http.Request, mode types.SyncMode) {
	versions, err := util.VersionMapFromRequestQuery(r, "since")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	timeout, err := util.IntFromRequestQuery(r, "timeout", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	userDevice := middleware.GetRequestUserDevice(r)
	userID, deviceID := userDevice.UserID, userDevice.DeviceID

	// Get filter from JSON in query or ID
	var filter *mautrix.Filter
	filterIDOrString := r.URL.Query().Get("filter")
	if filterIDOrString != "" {
		if strings.HasPrefix(filterIDOrString, "{") {
			var fil mautrix.Filter
			if err := json.Unmarshal([]byte(filterIDOrString), &fil); err != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MNotJSON, "Filter JSON is invalid: "+err.Error())
				return
			}
			filter = &fil
		} else {
			b, err := util.Base64DecodeURLSafe(filterIDOrString)
			if err != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Filter does not exist")
				return
			}
			filter, err = c.db.Accounts.GetFilter(r.Context(), userID, b)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
		}
	}

	options := types.SyncOptions{
		Filter: filter,
		Mode:   mode,
		UserID: userID,
	}

	// TODO: cache these, don't need 100% accuracy (only to wake up sync)
	rooms, err := c.db.Rooms.GetUserMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	changeCh := c.notifiers.Subscribe(notifier.Subscription{
		UserIDs: []id.UserID{userID},
		RoomIDs: slices.Collect(maps.Keys(rooms)),
	})
	defer c.notifiers.Unsubscribe(changeCh)

	sync, err := c.db.SyncForUser(r.Context(), userID, deviceID, options, versions)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// If sync is empty, wait on notifier, re-sync
	if sync.IsEmpty() {
		hlog.FromRequest(r).Trace().Msg("Sync empty, waiting for change")
		select {
		case <-changeCh:
		case <-time.After(time.Millisecond * time.Duration(timeout)):
		}
		sync, err = c.db.SyncForUser(r.Context(), userID, deviceID, options, versions)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, sync)
}
