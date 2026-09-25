package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
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

// https://spec.matrix.org/v1.14/client-server-api/#get_matrixclientv3accountwhoami
func (c *ClientRoutes) GetWhoami(w http.ResponseWriter, r *http.Request) {
	userDevice := middleware.GetRequestUserDevice(r)
	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespWhoami{
		UserID:   userDevice.UserID,
		DeviceID: userDevice.DeviceID,
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
	// TODO: other auth methods?

	username := req.Identifier.User

	// Handle userIDs -> extract localpart
	if userID := id.UserID(req.Identifier.User); userID.Homeserver() != "" {
		if userID.Homeserver() != c.config.ServerName {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid userid")
			return
		}
		username = userID.Localpart()
	}

	resp, err := c.db.Accounts.LoginWithPassword(
		r.Context(),
		username,
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

	if r.Header.Get("X-Babbleserv-Register-Secret") != c.config.Accounts.RegisterSecretHeaderValue {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Invalid secret header")
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

type passwordAuthData struct {
	Type       mautrix.AuthType       `json:"type"`
	Session    string                 `json:"session,omitempty"`
	Identifier mautrix.UserIdentifier `json:"identifier"`
	Password   string                 `json:"password"`
}

type passwordUIAResponse struct {
	Flows   []mautrix.UIAFlow `json:"flows"`
	Params  map[string]any    `json:"params"`
	Session string            `json:"session"`
	ErrCode string            `json:"errcode,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func respondPasswordUIA(w http.ResponseWriter, r *http.Request, session, errCode, message string) {
	util.ResponseJSON(w, r, http.StatusUnauthorized, passwordUIAResponse{
		Flows:   []mautrix.UIAFlow{{Stages: []mautrix.AuthType{mautrix.AuthTypePassword}}},
		Params:  map[string]any{},
		Session: session,
		ErrCode: errCode,
		Error:   message,
	})
}

func parseUIARequestBody(r *http.Request, allowEmpty bool) (json.RawMessage, json.RawMessage, *mautrix.RespError) {
	var encoded json.RawMessage
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	if err != nil {
		return nil, nil, &mautrix.MNotJSON
	}
	if len(bodyBytes) > 64<<10 {
		return nil, nil, &mautrix.MTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	if err := decoder.Decode(&encoded); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			encoded = json.RawMessage("{}")
		} else {
			return nil, nil, &mautrix.MNotJSON
		}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, &mautrix.MNotJSON
	}
	var body map[string]any
	objectDecoder := json.NewDecoder(bytes.NewReader(encoded))
	objectDecoder.UseNumber()
	if err := objectDecoder.Decode(&body); err != nil || body == nil {
		return nil, nil, &mautrix.MBadJSON
	}
	var auth json.RawMessage
	if value, ok := body["auth"]; ok {
		auth, _ = json.Marshal(value)
	}
	delete(body, "auth")
	params, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	if len(params) > 64<<10 {
		return nil, nil, &mautrix.MTooLarge
	}
	return params, auth, nil
}

func (c *ClientRoutes) createPasswordUIASession(
	w http.ResponseWriter,
	r *http.Request,
	userDevice types.UserDevice,
	params json.RawMessage,
	errCode, message string,
) {
	session, err := c.db.Accounts.CreateUIASession(
		r.Context(), userDevice, r.Method, r.URL.Path, params,
	)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	respondPasswordUIA(w, r, session, errCode, message)
}

func (c *ClientRoutes) parsePasswordUIA(
	w http.ResponseWriter,
	r *http.Request,
	params, raw json.RawMessage,
	userDevice types.UserDevice,
) (*passwordAuthData, json.RawMessage, bool) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		c.createPasswordUIASession(w, r, userDevice, params, "", "")
		return nil, nil, false
	}
	var auth passwordAuthData
	if err := json.Unmarshal(raw, &auth); err != nil {
		c.createPasswordUIASession(
			w, r, userDevice, params, mautrix.MForbidden.ErrCode, "Invalid authentication data",
		)
		return nil, nil, false
	}
	if auth.Session != "" {
		stored, err := c.db.Accounts.GetUIASessionRequest(
			r.Context(), auth.Session, userDevice, r.Method, r.URL.Path,
		)
		if errors.Is(err, types.ErrUIASessionMismatch) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "UIA session does not belong to this request")
			return nil, nil, false
		} else if errors.Is(err, types.ErrUIASessionNotFound) || errors.Is(err, types.ErrUIASessionExpired) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Unknown or expired UIA session")
			return nil, nil, false
		} else if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return nil, nil, false
		}
		if string(params) != "{}" && !bytes.Equal(params, stored) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Request parameters changed during UIA")
			return nil, nil, false
		}
		params = stored
	}
	if auth.Type != mautrix.AuthTypePassword || auth.Identifier.Type != mautrix.IdentifierTypeUser || auth.Password == "" {
		if auth.Session == "" {
			c.createPasswordUIASession(
				w, r, userDevice, params, mautrix.MForbidden.ErrCode, "Invalid password authentication",
			)
		} else {
			respondPasswordUIA(w, r, auth.Session, mautrix.MForbidden.ErrCode, "Invalid password authentication")
		}
		return nil, nil, false
	}

	authUser := auth.Identifier.User
	if parsed := id.UserID(authUser); parsed.Homeserver() == "" {
		authUser = "@" + authUser + ":" + userDevice.UserID.Homeserver()
	}
	if id.UserID(authUser) != userDevice.UserID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Authentication user does not match access token owner")
		return nil, nil, false
	}
	return &auth, params, true
}

func (c *ClientRoutes) respondPasswordError(
	w http.ResponseWriter,
	r *http.Request,
	userDevice types.UserDevice,
	params json.RawMessage,
	session string,
	err error,
) bool {
	if errors.Is(err, types.ErrInvalidPassword) || errors.Is(err, types.ErrUserNotFound) {
		if session == "" {
			c.createPasswordUIASession(
				w, r, userDevice, params, mautrix.MForbidden.ErrCode, "Invalid password",
			)
		} else {
			respondPasswordUIA(w, r, session, mautrix.MForbidden.ErrCode, "Invalid password")
		}
		return true
	} else if errors.Is(err, types.ErrUIASessionNotFound) || errors.Is(err, types.ErrUIASessionExpired) ||
		errors.Is(err, types.ErrUIASessionMismatch) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "UIA session is no longer valid")
		return true
	}
	return false
}

func (c *ClientRoutes) Logout(w http.ResponseWriter, r *http.Request) {
	c.logout(w, r, false)
}

func (c *ClientRoutes) LogoutAll(w http.ResponseWriter, r *http.Request) {
	c.logout(w, r, true)
}

func (c *ClientRoutes) logout(w http.ResponseWriter, r *http.Request, all bool) {
	err := c.db.Accounts.Logout(r.Context(), *middleware.GetRequestUserDevice(r), all)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

func (c *ClientRoutes) Refresh(w http.ResponseWriter, r *http.Request) {
	body, _, parseErr := parseUIARequestBody(r, false)
	if parseErr != nil {
		if parseErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, parseErr)
			return
		}
		util.ResponseErrorJSON(w, r, *parseErr)
		return
	}
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	} else if req.RefreshToken == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing refresh token")
		return
	}
	resp, err := c.db.Accounts.RefreshAccessToken(r.Context(), req.RefreshToken)
	if errors.Is(err, types.ErrUserNotFound) {
		util.ResponseErrorJSON(w, r, mautrix.MUnknownToken)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, resp)
}
