package federation

import (
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/util"
)

// Federation key requests may only name our users. Checked up front so a nonlocal user never reaches
// the remote device cache and a rejected claim consumes no keys.
func hasNonlocalUser[V any](serverName string, users map[id.UserID]V) bool {
	for userID := range users {
		if userID.Homeserver() != serverName {
			return true
		}
	}
	return false
}

// https://spec.matrix.org/v1.16/server-server-api/#post_matrixfederationv1userkeysclaim
func (f *FederationRoutes) ClaimUserKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqClaimKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	} else if hasNonlocalUser(f.config.ServerName, req.OneTimeKeys) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Requested claims for nonlocal users")
		return
	}

	resp, _, err := shared.ClaimUserKeys(r.Context(), f.config, f.db, req.OneTimeKeys)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.16/server-server-api/#post_matrixfederationv1userkeysquery
func (f *FederationRoutes) QueryUserKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqQueryKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	} else if hasNonlocalUser(f.config.ServerName, req.DeviceKeys) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Requested keys for nonlocal users")
		return
	}

	resp, err := shared.GetUserKeys(r.Context(), f.config, f.db, req.DeviceKeys)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}
