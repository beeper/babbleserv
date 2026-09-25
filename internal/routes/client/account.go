package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

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
		strings.ToLower(username),
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

type registrationAuthData struct {
	Type    mautrix.AuthType `json:"type"`
	Session string           `json:"session,omitempty"`
}

type reqRegister struct {
	Username                 string      `json:"username,omitempty"`
	Password                 string      `json:"password,omitempty"`
	DeviceID                 id.DeviceID `json:"device_id,omitempty"`
	InitialDeviceDisplayName string      `json:"initial_device_display_name,omitempty"`
	InhibitLogin             bool        `json:"inhibit_login,omitempty"`
	RefreshToken             bool        `json:"refresh_token,omitempty"`
}

type storedRegisterRequest struct {
	Username                 string      `json:"username"`
	UsernameProvided         bool        `json:"username_provided"`
	PasswordHash             []byte      `json:"password_hash,omitempty"`
	DeviceID                 id.DeviceID `json:"device_id,omitempty"`
	InitialDeviceDisplayName string      `json:"initial_device_display_name,omitempty"`
	InhibitLogin             bool        `json:"inhibit_login,omitempty"`
	RefreshToken             bool        `json:"refresh_token,omitempty"`
}

func (c *ClientRoutes) registrationAllowed(r *http.Request) bool {
	if public := c.config.Accounts.PublicRegistration; public != nil {
		if *public {
			return true
		}
	} else if c.config.Accounts.RegisterSecretHeaderValue == "" {
		return true
	}
	return c.registrationSecretAuthorized(r)
}

func (c *ClientRoutes) registrationSecretAuthorized(r *http.Request) bool {
	secret := c.config.Accounts.RegisterSecretHeaderValue
	return secret != "" && r.Header.Get("X-Babbleserv-Register-Secret") == secret
}

func normalizeRegistrationUsername(username, serverName string) (string, error) {
	username = strings.ToLower(username)
	if err := id.ValidateUserLocalpart(username); err != nil {
		return "", err
	}
	if len(id.NewUserID(username, serverName)) > id.UserIDMaxLength {
		return "", id.ErrUserIDTooLong
	}
	return username, nil
}

func prepareRegistrationRequest(req reqRegister, serverName string) (storedRegisterRequest, error) {
	stored := storedRegisterRequest{
		UsernameProvided:         req.Username != "",
		DeviceID:                 req.DeviceID,
		InitialDeviceDisplayName: req.InitialDeviceDisplayName,
		InhibitLogin:             req.InhibitLogin,
		RefreshToken:             req.RefreshToken,
	}
	var err error
	if req.Username == "" {
		stored.Username = strings.ToLower(util.GenerateRandomStringBase32Hex(16))
	} else if stored.Username, err = normalizeRegistrationUsername(req.Username, serverName); err != nil {
		return stored, err
	}
	if req.Password != "" {
		stored.PasswordHash, err = bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	}
	return stored, err
}

func registrationRequestMatches(req reqRegister, stored storedRegisterRequest) bool {
	username := strings.ToLower(req.Username)
	if (req.Username != "") != stored.UsernameProvided || stored.UsernameProvided && username != stored.Username {
		return false
	}
	if (req.Password == "") != (len(stored.PasswordHash) == 0) ||
		req.Password != "" && bcrypt.CompareHashAndPassword(stored.PasswordHash, []byte(req.Password)) != nil {
		return false
	}
	return req.DeviceID == stored.DeviceID &&
		req.InitialDeviceDisplayName == stored.InitialDeviceDisplayName &&
		req.InhibitLogin == stored.InhibitLogin && req.RefreshToken == stored.RefreshToken
}

func respondRegistrationUIA(w http.ResponseWriter, r *http.Request, session, errCode, message string) {
	util.ResponseJSON(w, r, http.StatusUnauthorized, uiaResponse{
		Flows:   []mautrix.UIAFlow{{Stages: []mautrix.AuthType{mautrix.AuthTypeDummy}}},
		Params:  map[string]any{},
		Session: session,
		ErrCode: errCode,
		Error:   message,
	})
}

func (c *ClientRoutes) validateRegistrationAvailability(
	w http.ResponseWriter,
	r *http.Request,
	username string,
) bool {
	available, err := c.db.Accounts.IsLocalUsernameAvailable(r.Context(), username)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return false
	} else if !available {
		util.ResponseErrorMessageJSON(w, r, mautrix.MUserInUse, "Username is already taken")
		return false
	}
	return true
}

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3registeravailable
func (c *ClientRoutes) GetRegisterAvailable(w http.ResponseWriter, r *http.Request) {
	if !c.registrationAllowed(r) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Registration is disabled")
		return
	}
	username, err := normalizeRegistrationUsername(r.URL.Query().Get("username"), c.config.ServerName)
	if err != nil || username == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidUsername, "Invalid username")
		return
	}
	if !c.validateRegistrationAvailability(w, r, username) {
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespRegisterAvailable{Available: true})
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3register
func (c *ClientRoutes) Register(w http.ResponseWriter, r *http.Request) {
	if !c.registrationAllowed(r) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Registration is disabled")
		return
	}
	if kind := r.URL.Query().Get("kind"); kind != "" && kind != "user" {
		if kind == "guest" {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Guest registration is not supported")
		} else {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid registration kind")
		}
		return
	}

	params, rawAuth, respErr := parseUIARequestBody(r, false)
	if respErr != nil {
		if respErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, respErr)
		} else {
			util.ResponseErrorJSON(w, r, *respErr)
		}
		return
	}
	var req reqRegister
	if err := json.Unmarshal(params, &req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	var auth registrationAuthData
	if len(rawAuth) > 0 && string(rawAuth) != "null" && string(rawAuth) != "{}" {
		if err := json.Unmarshal(rawAuth, &auth); err != nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
	}

	if len(req.Password) > 72 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Password must be at most 72 bytes")
		return
	}

	var stored storedRegisterRequest
	if auth.Session != "" {
		saved, err := c.db.Accounts.GetUIASessionRequest(
			r.Context(), auth.Session, types.UserDevice{}, r.Method, r.URL.Path,
		)
		if errors.Is(err, types.ErrUIASessionNotFound) || errors.Is(err, types.ErrUIASessionExpired) ||
			errors.Is(err, types.ErrUIASessionMismatch) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Unknown or mismatched UIA session")
			return
		} else if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if err := json.Unmarshal(saved, &stored); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		if string(params) != "{}" && !registrationRequestMatches(req, stored) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "Request parameters changed during UIA")
			return
		}
	} else {
		if req.Username != "" {
			if _, err := normalizeRegistrationUsername(req.Username, c.config.ServerName); err != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidUsername, "Invalid username")
				return
			}
		}
		var err error
		stored, err = prepareRegistrationRequest(req, c.config.ServerName)
		if err != nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid password")
			return
		}
	}

	if !c.validateRegistrationAvailability(w, r, stored.Username) {
		return
	}
	if auth.Type != mautrix.AuthTypeDummy && !c.registrationSecretAuthorized(r) {
		if auth.Session == "" {
			// Incomplete requests discover flows without binding an unusable session.
			if len(stored.PasswordHash) == 0 {
				respondRegistrationUIA(w, r, "", "", "")
				return
			}
			storedParams, err := json.Marshal(stored)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
			session, err := c.db.Accounts.CreateUIASession(
				r.Context(), types.UserDevice{}, r.Method, r.URL.Path, storedParams,
			)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
			respondRegistrationUIA(w, r, session, "", "")
		} else {
			respondRegistrationUIA(w, r, auth.Session, mautrix.MForbidden.ErrCode, "Invalid authentication data")
		}
		return
	}
	if len(stored.PasswordHash) == 0 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing or empty password")
		return
	}

	resp, err := c.db.Accounts.RegisterWithPasswordHash(
		r.Context(), stored.Username, stored.PasswordHash, stored.RefreshToken, stored.DeviceID,
		stored.InitialDeviceDisplayName, stored.InhibitLogin, auth.Session, r.Method, r.URL.Path,
	)
	if errors.Is(err, types.ErrUserAlreadyExists) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MUserInUse, "Username is already taken")
	} else if errors.Is(err, types.ErrUIASessionNotFound) || errors.Is(err, types.ErrUIASessionExpired) ||
		errors.Is(err, types.ErrUIASessionMismatch) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, "UIA session is no longer valid")
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
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

type reqChangePassword struct {
	NewPassword   string `json:"new_password"`
	LogoutDevices *bool  `json:"logout_devices,omitempty"`
}

type storedChangePasswordRequest struct {
	NewPasswordHash []byte `json:"new_password_hash"`
	LogoutDevices   bool   `json:"logout_devices"`
}

func prepareStoredUIAParams(path string, params json.RawMessage) (json.RawMessage, error) {
	if path != "/_matrix/client/v3/account/password" {
		return params, nil
	}
	var req reqChangePassword
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), 12)
	if err != nil {
		return nil, err
	}
	return json.Marshal(storedChangePasswordRequest{
		NewPasswordHash: hash,
		LogoutDevices:   req.LogoutDevices == nil || *req.LogoutDevices,
	})
}

func storedUIAParamsMatch(path string, submitted, stored json.RawMessage) bool {
	if path != "/_matrix/client/v3/account/password" {
		return bytes.Equal(submitted, stored)
	}
	var req reqChangePassword
	var saved storedChangePasswordRequest
	if json.Unmarshal(submitted, &req) != nil || json.Unmarshal(stored, &saved) != nil {
		return false
	}
	return (req.LogoutDevices == nil || *req.LogoutDevices) == saved.LogoutDevices &&
		bcrypt.CompareHashAndPassword(saved.NewPasswordHash, []byte(req.NewPassword)) == nil
}

type uiaResponse struct {
	Flows   []mautrix.UIAFlow `json:"flows"`
	Params  map[string]any    `json:"params"`
	Session string            `json:"session,omitempty"`
	ErrCode string            `json:"errcode,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func respondPasswordUIA(w http.ResponseWriter, r *http.Request, session, errCode, message string) {
	util.ResponseJSON(w, r, http.StatusUnauthorized, uiaResponse{
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
	storedParams, err := prepareStoredUIAParams(r.URL.Path, params)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	session, err := c.db.Accounts.CreateUIASession(
		r.Context(), userDevice, r.Method, r.URL.Path, storedParams,
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
		if string(params) != "{}" && !storedUIAParamsMatch(r.URL.Path, params, stored) {
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

func (c *ClientRoutes) ChangePassword(w http.ResponseWriter, r *http.Request) {
	params, rawAuth, respErr := parseUIARequestBody(r, false)
	if respErr != nil {
		if respErr.ErrCode == mautrix.MTooLarge.ErrCode {
			util.ResponseJSON(w, r, http.StatusRequestEntityTooLarge, respErr)
			return
		}
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	var req reqChangePassword
	if err := json.Unmarshal(params, &req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	var sessionAuth passwordAuthData
	if len(rawAuth) > 0 {
		if err := json.Unmarshal(rawAuth, &sessionAuth); err != nil {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
	}
	if req.NewPassword == "" && (string(params) != "{}" || sessionAuth.Session == "") {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing or empty new_password")
		return
	}
	if len(req.NewPassword) > 72 {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Password exceeds 72 bytes")
		return
	}
	userDevice := *middleware.GetRequestUserDevice(r)
	auth, prepared, ok := c.parsePasswordUIA(w, r, params, rawAuth, userDevice)
	if !ok {
		return
	}
	if auth.Session == "" {
		var err error
		prepared, err = prepareStoredUIAParams(r.URL.Path, params)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}
	var stored storedChangePasswordRequest
	if err := json.Unmarshal(prepared, &stored); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	err := c.db.Accounts.ChangePassword(
		r.Context(), userDevice, auth.Password, stored.NewPasswordHash, stored.LogoutDevices,
		auth.Session, r.Method, r.URL.Path,
	)
	if c.respondPasswordError(w, r, userDevice, params, auth.Session, err) {
		return
	}
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}
