package debug

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (d *DebugRoutes) DebugGetUser(w http.ResponseWriter, r *http.Request) {
	userID := util.UserIDFromRequestURLParam(r, "userID")
	user, err := d.db.Accounts.GetLocalUser(r.Context(), userID)
	if errors.Is(err, types.ErrUserNotFound) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	tokens, err := d.db.Accounts.GetUserDeviceTokenPrefixes(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	profile, err := d.db.Accounts.GetUserProfile(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	memberships, err := d.db.Rooms.GetUserMemberships(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	crossSigningKeys, err := d.db.Accounts.GetUserCrossSigningKeys(r.Context(), userID, userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	devices, err := d.db.Accounts.GetUserDevices(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	deviceKeys := make(map[id.DeviceID]mautrix.DeviceKeys, len(devices))
	deviceOTKCounts := make(map[id.DeviceID]mautrix.OTKCount, len(devices))
	deviceFallbackKeys := make(map[id.DeviceID]map[id.KeyAlgorithm]types.FallbackKey, len(devices))
	for _, device := range devices {
		keys, err := d.db.Accounts.GetDeviceKeys(r.Context(), userID, device.ID, userID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if keys != nil {
			deviceKeys[device.ID] = *keys
		}
		counts, err := d.db.Accounts.CountOneTimeKeys(r.Context(), userID, device.ID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		deviceOTKCounts[device.ID] = counts
		fbkeys, err := d.db.Accounts.GetFallbackKeys(r.Context(), userID, device.ID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		deviceFallbackKeys[device.ID] = fbkeys
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		User *types.User `json:"accounts_user"`

		Devices                []*types.Device                                       `json:"devices"`
		DeviceKeys             map[id.DeviceID]mautrix.DeviceKeys                    `json:"device_keys"`
		DeviceOneTimeKeyCounts map[id.DeviceID]mautrix.OTKCount                      `json:"device_otk_counts"`
		DeviceFallbackKeys     map[id.DeviceID]map[id.KeyAlgorithm]types.FallbackKey `json:"device_fallback_keys"`

		CrossSigningKeys *types.UserCrossSigningKeys `json:"cross_signing_keys"`

		AuthTokens    map[id.DeviceID][]string `json:"accounts_device_auth_tokens"`
		RefreshTokens map[id.DeviceID][]string `json:"accounts_device_refresh_tokens"`

		Profile     *types.UserProfile `json:"rooms_profile"`
		Memberships types.Memberships  `json:"rooms_memberships"`
	}{user, devices, deviceKeys, deviceOTKCounts, deviceFallbackKeys, crossSigningKeys, tokens.AuthTokens, tokens.RefreshTokens, profile, memberships})
}

func (d *DebugRoutes) DebugGetServer(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")

	memberships, err := d.db.Rooms.GetServerMemberships(r.Context(), serverName)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Memberships types.Memberships `json:"memberships"`
	}{memberships})
}
