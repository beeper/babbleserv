package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/util"
)

type userDeviceResp struct {
	Keys        mautrix.DeviceKeys `json:"keys"`
	ID          id.DeviceID        `json:"device_id"`
	DisplayName string             `json:"device_display_name,omitzero"`
}

type userDevicesResp struct {
	Devices        []userDeviceResp          `json:"devices"`
	MasterKey      *mautrix.CrossSigningKeys `json:"master_key,omitzero"`
	SelfSigningKey *mautrix.CrossSigningKeys `json:"self_signing_key,omitzero"`
	StreamID       int                       `json:"stream_id"`
	UserID         id.UserID                 `json:"user_id"`
}

func (f *FederationRoutes) GetUserDevices(w http.ResponseWriter, r *http.Request) {
	userID := util.UserIDFromRequestURLParam(r, "userID")

	user, err := f.db.Accounts.GetLocalUser(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if user == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	devices, err := f.db.Accounts.GetUserDevices(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	keysReq := mautrix.DeviceKeysRequest{userID: mautrix.DeviceIDList{}}
	keysResp, _, err := shared.GetUserKeys(r.Context(), f.config, f.db, keysReq, "")
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	devicesResp := make([]userDeviceResp, 0, len(devices))
	for _, device := range devices {
		// We only care about devices that have device keys
		if keys, ok := keysResp.DeviceKeys[userID][device.ID]; ok {
			devicesResp = append(devicesResp, userDeviceResp{
				ID:          device.ID,
				DisplayName: device.DisplayName,
				Keys:        keys,
			})
		}
	}

	resp := userDevicesResp{
		StreamID: int(user.DeviceListVersion),
		Devices:  devicesResp,
		UserID:   userID,
	}

	if keys, ok := keysResp.MasterKeys[userID]; ok {
		resp.MasterKey = &keys
	}
	if keys, ok := keysResp.SelfSigningKeys[userID]; ok {
		resp.SelfSigningKey = &keys
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}
