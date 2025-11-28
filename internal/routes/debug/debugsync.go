package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugSyncUser(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))
	deviceID := id.DeviceID(chi.URLParam(r, "deviceID"))

	versions, err := util.VersionMapFromRequestQuery(r, "since")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	if sync, err := b.db.SyncForUser(r.Context(), userID, deviceID, types.SyncOptions{}, versions); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, sync)
		return
	}
}
