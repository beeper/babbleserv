package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

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
	StreamID       int64                     `json:"stream_id"`
	UserID         id.UserID                 `json:"user_id"`
}

// The stream ID and devices come from one read so a receiver's next device-list EDU, whose prev_id
// is this stream ID, applies on top of exactly this device list.
func (f *FederationRoutes) GetUserDevices(w http.ResponseWriter, r *http.Request) {
	userID := util.UserIDFromRequestURLParam(r, "userID")

	snapshot, err := f.db.Accounts.GetLocalUserDevicesSnapshot(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if snapshot == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	devicesResp := make([]userDeviceResp, 0, len(snapshot.Devices))
	for _, device := range snapshot.Devices {
		// We only care about devices that have device keys
		if device.Keys != nil {
			devicesResp = append(devicesResp, userDeviceResp{
				ID:          device.Device.ID,
				DisplayName: device.Device.DisplayName,
				Keys:        *device.Keys,
			})
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, userDevicesResp{
		Devices:        devicesResp,
		MasterKey:      snapshot.MasterKey,
		SelfSigningKey: snapshot.SelfSigningKey,
		StreamID:       snapshot.Version,
		UserID:         userID,
	})
}
