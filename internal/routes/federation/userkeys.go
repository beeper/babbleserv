package federation

import (
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.16/server-server-api/#post_matrixfederationv1userkeysclaim
func (f *FederationRoutes) ClaimUserKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqClaimKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	resp, otherServerKeys, err := shared.ClaimUserKeys(r.Context(), f.config, f.db, req.OneTimeKeys)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(otherServerKeys) > 0 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Requested claims for nonlocal users")
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
	}

	resp, otherServerKeys, err := shared.GetUserKeys(r.Context(), f.config, f.db, req.DeviceKeys, "")
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(otherServerKeys) > 0 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Requested keys for nonlocal users")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}
