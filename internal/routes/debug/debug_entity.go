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

	user, err := b.db.Accounts.GetLocalUserForUsername(r.Context(), userID.Localpart())
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	tokens, err := b.db.Accounts.GetUserDeviceTokenPrefixes(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

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
		User          *types.User              `json:"accounts_user"`
		AuthTokens    map[id.DeviceID][]string `json:"accounts_device_auth_tokens"`
		RefreshTokens map[id.DeviceID][]string `json:"accounts_device_refresh_tokens"`

		Profile            *types.UserProfile `json:"rooms_profile"`
		Memberships        types.Memberships  `json:"rooms_memberships"`
		OutlierMemberships types.Memberships  `json:"rooms_outlier_memberships"`
	}{user, tokens.AuthTokens, tokens.RefreshTokens, profile, memberships, outlierMemberships})
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
