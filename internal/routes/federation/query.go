package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.10/server-server-api/#get_matrixfederationv1queryprofile
func (f *FederationRoutes) QueryProfile(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(r.URL.Query().Get("user_id"))

	profile, err := f.db.Rooms.GetUserProfile(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if profile == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var resp any = profile

	key := r.URL.Query().Get("field")
	if key == "displayname" {
		resp = map[string]string{
			"displayname": profile.DisplayName,
		}
	} else if key == "avatar_url" {
		resp = map[string]string{
			"avatar_url": profile.AvatarURL,
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
	return
}
