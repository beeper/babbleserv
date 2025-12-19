package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) SendToDevice(w http.ResponseWriter, r *http.Request) {
	txnID := chi.URLParam(r, "txnID")
	eventType := event.NewEventType(chi.URLParam(r, "eventType"))

	req, respErr := util.ParseRequestJSON[mautrix.ReqSendToDevice](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	userDevice := middleware.GetRequestUserDevice(r)

	tds := make([]*types.ToDevice, 0, len(req.Messages)*3)

	for targetUserID, devices := range req.Messages {
		targetUserHS := targetUserID.Homeserver()
		if targetUserHS == c.config.ServerName {
			user, err := c.db.Accounts.GetLocalUser(r.Context(), targetUserID)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			} else if user == nil {
				hlog.FromRequest(r).Warn().Msg("Ignoring to-device event for unknown local user")
				continue
			}
		}
		for targetDeviceID, content := range devices {
			deviceIDs := make([]id.DeviceID, 0, 1)

			if targetDeviceID == "*" && targetUserHS == c.config.ServerName {
				// If sending to a *local* user with "*" as the device ID, fetch all the users
				// devices and create a to device for each.
				userDevices, err := c.db.Accounts.GetUserDevices(r.Context(), targetUserID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				}
				for _, d := range userDevices {
					deviceIDs = append(deviceIDs, d.ID)
				}
			} else {
				deviceIDs = append(deviceIDs, targetDeviceID)
			}

			for _, did := range deviceIDs {
				tds = append(tds, &types.ToDevice{
					UserID:   targetUserID,
					DeviceID: did,
					Sender:   userDevice.UserID,
					Type:     eventType,
					Content:  content.VeryRaw,
				})
			}
		}
	}

	if len(tds) == 0 {
		hlog.FromRequest(r).Warn().Msg("Got empty to-device request")
		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
		return
	}

	_, err := c.db.Transient.SendToDeviceEvents(r.Context(), tds, transient.SendToDeviceOptions{
		TransactionID: txnID,
		DeviceID:      userDevice.DeviceID,
	})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
