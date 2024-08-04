package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugGetUser(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	profile, err := b.db.Rooms.GetUserProfile(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	memberships, err := b.db.Rooms.GetUserMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	outlierMemberships, err := b.db.Rooms.GetUserOutlierMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Profile            *types.UserProfile `json:"profile"`
		Memberships        types.Memberships  `json:"memberships"`
		OutlierMemberships types.Memberships  `json:"outlier_memberships"`
	}{profile, memberships, outlierMemberships})
}

func (b *DebugRoutes) DebugGetServer(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")

	memberships, err := b.db.Rooms.GetServerMemberships(r.Context(), serverName)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Memberships types.Memberships `json:"memberships"`
	}{memberships})
}
