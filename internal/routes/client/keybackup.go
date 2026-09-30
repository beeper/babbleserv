package client

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3room_keysversion
func (c *ClientRoutes) PostKeyBackupVersion(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)

	req, respErr := util.ParseRequestJSON[types.ReqCreateKeyBackupVersion](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if req.Algorithm == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing algorithm")
		return
	}

	version, err := c.db.Accounts.CreateKeyBackupVersion(r.Context(), userID, &types.KeyBackupVersion{
		Algorithm: req.Algorithm,
		AuthData:  req.AuthData,
	})
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, types.RespCreateKeyBackupVersion{
		Version: version,
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3room_keysversion
func (c *ClientRoutes) GetKeyBackupVersionCurrent(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)

	version, err := c.db.Accounts.GetLatestKeyBackupVersion(r.Context(), userID)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if version == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "No current backup version")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, version)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3room_keysversionversion
func (c *ClientRoutes) GetKeyBackupVersion(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := chi.URLParam(r, "version")

	version, err := c.db.Accounts.GetKeyBackupVersion(r.Context(), userID, versionString)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if version == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Unknown backup version")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, version)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3room_keysversionversion
func (c *ClientRoutes) PutKeyBackupVersion(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := chi.URLParam(r, "version")

	req, respErr := util.ParseRequestJSON[types.ReqUpdateKeyBackupVersion](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if req.Algorithm == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing algorithm")
		return
	}

	err := c.db.Accounts.UpdateKeyBackupVersion(r.Context(), userID, versionString, &types.KeyBackupVersion{
		Algorithm: req.Algorithm,
		AuthData:  req.AuthData,
	})
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#delete_matrixclientv3room_keysversionversion
func (c *ClientRoutes) DeleteKeyBackupVersion(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := chi.URLParam(r, "version")

	err := c.db.Accounts.DeleteKeyBackupVersion(r.Context(), userID, versionString)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3room_keyskeys
func (c *ClientRoutes) PutRoomKeys(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	req, respErr := util.ParseRequestJSON[types.ReqStoreRoomKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if req.Rooms == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	// Convert to the format expected by the database
	rooms := make(map[string]map[string]*types.KeyBackupData)
	for roomID, roomBackup := range req.Rooms {
		if !roomBackup.Valid() {
			util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
			return
		}
		rooms[roomID] = roomBackup.Sessions
	}

	resp, err := c.db.Accounts.StoreKeyBackupKeys(r.Context(), userID, versionString, rooms)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if resp == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Unknown backup version")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3room_keyskeys
func (c *ClientRoutes) GetRoomKeys(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	rooms, err := c.db.Accounts.GetAllKeyBackupKeys(r.Context(), userID, versionString)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	// Convert to response format
	resp := types.RespGetRoomKeys{
		Rooms: make(map[string]*types.RoomKeyBackup),
	}
	for roomID, sessions := range rooms {
		resp.Rooms[roomID] = &types.RoomKeyBackup{Sessions: sessions}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#delete_matrixclientv3room_keyskeys
func (c *ClientRoutes) DeleteRoomKeys(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	resp, err := c.db.Accounts.DeleteAllKeyBackupKeys(r.Context(), userID, versionString)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3room_keyskeysroomid
func (c *ClientRoutes) PutRoomKeysByRoomID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	req, respErr := util.ParseRequestJSON[types.RoomKeyBackup](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	if !req.Valid() {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	rooms := map[string]map[string]*types.KeyBackupData{
		roomID: req.Sessions,
	}

	resp, err := c.db.Accounts.StoreKeyBackupKeys(r.Context(), userID, versionString, rooms)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if resp == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Unknown backup version")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3room_keyskeysroomid
func (c *ClientRoutes) GetRoomKeysByRoomID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	sessions, err := c.db.Accounts.GetKeyBackupKeysForRoom(r.Context(), userID, versionString, roomID)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, types.RespGetRoomKeysByRoom{
		Sessions: sessions,
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#delete_matrixclientv3room_keyskeysroomid
func (c *ClientRoutes) DeleteRoomKeysByRoomID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	resp, err := c.db.Accounts.DeleteKeyBackupKeysForRoom(r.Context(), userID, versionString, roomID)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3room_keyskeysroomidsessionid
func (c *ClientRoutes) PutRoomKeyBySessionID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	sessionID := chi.URLParam(r, "sessionId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	req, respErr := util.ParseRequestJSON[types.KeyBackupData](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	rooms := map[string]map[string]*types.KeyBackupData{
		roomID: {
			sessionID: &req,
		},
	}

	resp, err := c.db.Accounts.StoreKeyBackupKeys(r.Context(), userID, versionString, rooms)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if resp == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Unknown backup version")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3room_keyskeysroomidsessionid
func (c *ClientRoutes) GetRoomKeyBySessionID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	sessionID := chi.URLParam(r, "sessionId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	data, err := c.db.Accounts.GetKeyBackupKey(r.Context(), userID, versionString, roomID, sessionID)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	if data == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "Key not found")
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, data)
}

// https://spec.matrix.org/v1.11/client-server-api/#delete_matrixclientv3room_keyskeysroomidsessionid
func (c *ClientRoutes) DeleteRoomKeyBySessionID(w http.ResponseWriter, r *http.Request) {
	userID := middleware.GetRequestUserID(r)
	roomID := chi.URLParam(r, "roomId")
	sessionID := chi.URLParam(r, "sessionId")
	versionString := r.URL.Query().Get("version")
	if versionString == "" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing version query parameter")
		return
	}

	resp, err := c.db.Accounts.DeleteKeyBackupKey(r.Context(), userID, versionString, roomID, sessionID)
	if err != nil {
		respondKeyBackupError(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

func respondKeyBackupError(w http.ResponseWriter, r *http.Request, err error) {
	var wrongVersion *types.WrongKeyBackupVersionError
	switch {
	case errors.Is(err, types.ErrKeyBackupNotFound):
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, err.Error())
	case errors.Is(err, types.ErrKeyBackupAlgorithmMismatch):
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
	case errors.As(err, &wrongVersion):
		util.ResponseJSON(w, r, http.StatusForbidden, struct {
			ErrCode        string `json:"errcode"`
			Error          string `json:"error"`
			CurrentVersion string `json:"current_version"`
		}{"M_WRONG_ROOM_KEYS_VERSION", err.Error(), wrongVersion.CurrentVersion})
	default:
		util.ResponseErrorUnknownJSON(w, r, err)
	}
}
