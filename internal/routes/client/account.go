package client

import (
	"encoding/json"
	"fmt"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3login
func (c *ClientRoutes) GetLogin(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespLoginFlows{
		Flows: []mautrix.LoginFlow{
			{Type: "m.login.password"},
		},
	})
}

// https://github.com/mautrix/go/pull/278
type reqLogin struct {
	mautrix.ReqLogin `json:",inline"`
	RefreshToken     bool `json:"refresh_token"`
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3login
func (c *ClientRoutes) Login(w http.ResponseWriter, r *http.Request) {
	var req reqLogin
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	if req.Type != mautrix.AuthTypePassword {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid auth type")
		return
	} else if req.Identifier.Type != mautrix.IdentifierTypeUser {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid identifier type")
		return
	}

	if req.Password == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid password")
		return
	}

	resp, err := c.db.Accounts.LoginWithPassword(
		r.Context(),
		req.Identifier.User,
		req.Password,
		req.RefreshToken,
		req.DeviceID,
		req.InitialDeviceDisplayName,
	)
	if err == types.ErrUserNotFound || err == types.ErrInvalidPassword {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Invalid username or password")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3register
func (c *ClientRoutes) Register(w http.ResponseWriter, r *http.Request) {
	var req mautrix.ReqRegister
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	if err := id.ValidateUserLocalpart(req.Username); err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Sprintf("Invalid username; %s", err.Error()))
		return
	}

	if req.Password == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing or empty password")
		return
	}

	if resp, err := c.db.Accounts.RegisterWithPassword(
		r.Context(),
		req.Username,
		[]byte(req.Password),
		req.RefreshToken,
		req.DeviceID,
		req.InitialDeviceDisplayName,
	); err == types.ErrUserAlreadyExists {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Username already taken")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, resp)
	}
}
