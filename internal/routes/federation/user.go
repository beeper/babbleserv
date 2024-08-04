package federation

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/util"
)

func (f *FederationRoutes) GetUserDevices(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Devices  []any     `json:"devices"`
		StreamID int       `json:"stream_id"`
		UserID   id.UserID `json:"user_id"`
	}{[]any{}, 1, userID})
}
