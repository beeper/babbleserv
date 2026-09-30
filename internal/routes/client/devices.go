package client

import (
	"encoding/json"
	"errors"
	"net/http"

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
	deviceID := decodedURLParam(r, "deviceID")

	device, err := c.db.Accounts.GetUserDevice(r.Context(), userID, id.DeviceID(deviceID))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	if device == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
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
	deviceID := decodedURLParam(r, "deviceID")

	if err := c.db.Accounts.UpdateUserDevice(r.Context(), userID, id.DeviceID(deviceID), req.DisplayName); errors.Is(err, types.ErrUserDeviceNotFound) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#delete_matrixclientv3devicesdeviceid
func (c *ClientRoutes) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := decodedURLParam(r, "deviceID")
	params, rawAuth, respErr := parseUIARequestBody(r, true)
	if respErr != nil {
		if respErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, respErr)
			return
		}
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	userDevice := *middleware.GetRequestUserDevice(r)
	auth, params, ok := c.parsePasswordUIA(w, r, params, rawAuth, userDevice)
	if !ok {
		return
	}
	if err := c.db.Accounts.DeleteUserDevicesWithPassword(
		r.Context(), userDevice, auth.Password,
		[]id.DeviceID{id.DeviceID(deviceID)},
		auth.Session, r.Method, r.URL.Path,
	); c.respondPasswordError(w, r, userDevice, params, auth.Session, err) {
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3delete_devices
func (c *ClientRoutes) DeleteDevices(w http.ResponseWriter, r *http.Request) {
	params, rawAuth, respErr := parseUIARequestBody(r, false)
	if respErr != nil {
		if respErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, respErr)
			return
		}
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	var initial struct {
		Devices []id.DeviceID `json:"devices"`
	}
	var continuation struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rawAuth, &continuation)
	if string(params) != "{}" || continuation.Session == "" {
		if err := json.Unmarshal(params, &initial); err != nil || initial.Devices == nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
	}
	userDevice := *middleware.GetRequestUserDevice(r)
	auth, params, ok := c.parsePasswordUIA(w, r, params, rawAuth, userDevice)
	if !ok {
		return
	}
	var req struct {
		Devices []id.DeviceID `json:"devices"`
	}
	if err := json.Unmarshal(params, &req); err != nil || req.Devices == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	if err := c.db.Accounts.DeleteUserDevicesWithPassword(
		r.Context(), userDevice, auth.Password, req.Devices,
		auth.Session, r.Method, r.URL.Path,
	); c.respondPasswordError(w, r, userDevice, params, auth.Session, err) {
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
