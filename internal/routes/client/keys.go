package client

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/rs/zerolog/hlog"
	"github.com/tidwall/gjson"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3keysupload
func (c *ClientRoutes) UploadKeys(w http.ResponseWriter, r *http.Request) {
	// Grab the raw JSON body for signature verification
	jsonB, err := io.ReadAll(r.Body)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	var req mautrix.ReqUploadKeys
	if err := json.Unmarshal(jsonB, &req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	if req.DeviceKeys != nil {
		if req.DeviceKeys.UserID != middleware.GetRequestUserID(r) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "UserID does not match")
			return
		} else if req.DeviceKeys.DeviceID != middleware.GetRequestDeviceID(r) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "DeviceID does not match")
			return
		}

		deviceKeysB := gjson.GetBytes(jsonB, "device_keys").Raw
		var verified bool
		for keyID, keyStr := range req.DeviceKeys.Keys {
			key, _ := util.Base64Decode(keyStr)
			if err := util.VerifyJSON(
				[]byte(deviceKeysB),
				string(req.DeviceKeys.UserID),
				string(keyID),
				ed25519.PublicKey(key),
			); err != nil {
				hlog.FromRequest(r).Warn().Err(err).Msg("Device keys object signature verify failed")
			} else {
				verified = true
			}
		}
		if !verified {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "DeviceKeys object is not signed by one of the device keys")
			return
		}

		// TODO: Save it!? Save the raw JSON as it comes?
		// do we also need to save bits of the key, are they returned/used anywhere else? (don't think so)
	}

	if len(req.OneTimeKeys) > 0 {
		// TODO: save OTKs
	}
}
