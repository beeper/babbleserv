package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3devices
func (c *ClientRoutes) GetDevices(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)

	devices, err := c.db.Accounts.GetUserDevices(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Devices []*types.Device `json:"devices"`
	}{devices})
}

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3devicesdeviceid
func (c *ClientRoutes) GetDevice(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	device, err := c.db.Accounts.GetUserDevice(r.Context(), userID, id.DeviceID(deviceID))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, device)
}

// https://spec.matrix.org/v1.16/client-server-api/#put_matrixclientv3devicesdeviceid
func (c *ClientRoutes) PutDevice(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqPutDevice](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	userID := middleware.GetRequestUserID(r)
	deviceID := chi.URLParam(r, "deviceID")

	if err := c.db.Accounts.UpdateUserDevice(r.Context(), userID, id.DeviceID(deviceID), req.DisplayName); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#delete_matrixclientv3devicesdeviceid
func (c *ClientRoutes) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3delete_devices
func (c *ClientRoutes) DeleteDevices(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
