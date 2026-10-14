package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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

	util.ResponseJSON(w, r, http.StatusOK, userDevicesResponse(userID, snapshot))
}

func userDevicesResponse(userID id.UserID, snapshot *types.LocalDeviceSnapshot) types.RespUserDevices {
	devicesResp := make([]types.RespUserDevice, 0, len(snapshot.Devices))
	for _, device := range snapshot.Devices {
		// We only care about devices that have device keys
		if !device.Keys.IsZero() {
			devicesResp = append(devicesResp, types.RespUserDevice{
				ID:          device.Device.ID,
				DisplayName: device.Device.DisplayName,
				Keys:        device.Keys,
			})
		}
	}
	return types.RespUserDevices{
		Devices:        devicesResp,
		MasterKey:      snapshot.MasterKey,
		SelfSigningKey: snapshot.SelfSigningKey,
		StreamID:       snapshot.Version,
		UserID:         userID,
	}
}
