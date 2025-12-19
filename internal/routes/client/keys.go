package client

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/hlog"
)

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keysclaim
func (c *ClientRoutes) ClaimKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqClaimKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	resp, serverToUserDevices, err := shared.ClaimUserKeys(r.Context(), c.config, c.db, req.OneTimeKeys)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	var wg sync.WaitGroup
	type result struct {
		server string
		err    error
		res    mautrix.RespClaimKeys
	}
	resultsCh := make(chan result, len(serverToUserDevices))
	for server, userDevices := range serverToUserDevices {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req := make(map[string]map[string]string, len(userDevices))
			for u, devices := range userDevices {
				ds := make(map[string]string, len(devices))
				for i, d := range devices {
					ds[i.String()] = string(d)
				}
				req[u.String()] = ds
			}

			res, err := c.fclient.ClaimKeys(
				r.Context(),
				spec.ServerName(c.config.ServerName),
				spec.ServerName(server),
				req,
			)

			// Gross: convert fclient.RespClaimKeys -> mautrix.RespClaimKeys
			b, _ := json.Marshal(res)
			var mres mautrix.RespClaimKeys
			json.Unmarshal(b, &mres)

			resultsCh <- result{server, err, mres}
		}()
	}

	wg.Wait()
	close(resultsCh)

	for res := range resultsCh {
		if res.err != nil {
			hlog.FromRequest(r).Warn().
				Err(res.err).
				Str("server", res.server).
				Msg("Failed to claim one time keys from server")
			continue
		}

		for userID, keys := range res.res.OneTimeKeys {
			if _, found := serverToUserDevices[res.server][userID]; !found {
				hlog.FromRequest(r).Warn().
					Str("server", res.server).
					Str("user_id", userID.String()).
					Msg("Got claimed one time keys for user we did not request")
				continue
			}
			resp.OneTimeKeys[userID] = keys
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keysquery
func (c *ClientRoutes) QueryKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqQueryKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	resp, serverToUserDevices, err := shared.GetUserKeys(r.Context(), c.config, c.db, req.DeviceKeys, u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	var wg sync.WaitGroup
	type result struct {
		server string
		err    error
		res    mautrix.RespQueryKeys
	}
	resultsCh := make(chan result, len(serverToUserDevices))
	for server, userToDeviceIDs := range serverToUserDevices {
		wg.Add(1)
		go func() {
			defer wg.Done()

			udmap := make(map[string][]string, len(userToDeviceIDs))
			for userID, deviceIDs := range userToDeviceIDs {
				dids := make([]string, len(deviceIDs))
				for i, did := range deviceIDs {
					dids[i] = did.String()
				}
				udmap[userID.String()] = dids
			}

			res, err := c.fclient.QueryKeys(
				r.Context(),
				spec.ServerName(c.config.ServerName),
				spec.ServerName(server),
				udmap,
			)

			// Gross: convert fclient.RespQueryKeys -> mautrix.RespQueryKeys
			b, _ := json.Marshal(res)
			var mres mautrix.RespQueryKeys
			json.Unmarshal(b, &mres)

			resultsCh <- result{server, err, mres}
		}()
	}

	wg.Wait()
	close(resultsCh)

	// TODO: cache the results?

	for res := range resultsCh {
		if res.err != nil {
			hlog.FromRequest(r).Warn().
				Err(res.err).
				Str("server", res.server).
				Msg("Failed to fetch user keys from server")
			continue
		}

		for uid, key := range res.res.MasterKeys {
			if _, found := serverToUserDevices[res.server][uid]; !found {
				hlog.FromRequest(r).Warn().
					Str("server", res.server).
					Str("user_id", uid.String()).
					Msg("Got remote master keys for user we did not request")
				continue
			}
			resp.MasterKeys[uid] = key
		}
		for uid, key := range res.res.UserSigningKeys {
			if _, found := serverToUserDevices[res.server][uid]; !found {
				hlog.FromRequest(r).Warn().
					Str("server", res.server).
					Str("user_id", uid.String()).
					Msg("Got remote user signing keys for user we did not request")
				continue
			}
			resp.UserSigningKeys[uid] = key
		}
		// By definition, should be impossible: SelfSigningKeys
		for uid, devices := range res.res.DeviceKeys {
			if _, found := serverToUserDevices[res.server][uid]; !found {
				hlog.FromRequest(r).Warn().
					Str("server", res.server).
					Str("user_id", uid.String()).
					Msg("Got remote device keys for user we did not request")
				continue
			}
			resp.DeviceKeys[uid] = devices
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keyssignaturesupload
func (c *ClientRoutes) UploadSignatures(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqUploadSignatures](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	// Memoize cross signing keys since they contain 3 keys and might need dupe fetches
	getCrossSigningKeys := util.MemoizeMap(func(uid id.UserID) (*types.UserCrossSigningKeys, error) {
		return c.db.Accounts.GetUserCrossSigningKeys(r.Context(), uid, u.UserID)
	}, len(req))

	// First pass: loop through the target objects to sign and validate they exist and match
	for targetUserID, targetObjects := range req {
		for targetKey, targetObject := range targetObjects {
			if targetObject.DeviceID == "" {
				// Target is one of the users cross signing keys
				targetXSKeys, err := getCrossSigningKeys(targetUserID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				}

				// Find and validate the relevant cross signing key matches the input
				var targetXSKey *types.CrossSigningKey
				switch targetKey {
				case targetXSKeys.Master.FirstKey().String():
					targetXSKey = &targetXSKeys.Master
				case targetXSKeys.SelfSigning.FirstKey().String():
					targetXSKey = &targetXSKeys.SelfSigning
				case targetXSKeys.UserSigning.FirstKey().String():
					if u.UserID != targetUserID {
						// This should be impossible, if the client has access to the actual user
						// signing key of another user we screwed up big time.
						util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Cannot access another users user signing key")
						return
					}
					targetXSKey = &targetXSKeys.UserSigning
				}
				if targetXSKey == nil {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target cross signing key does not exist")
					return
				} else if !util.CompareSignedObjectsJSON(targetObject, targetXSKey) {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Input cross signing key does not match stored")
					return
				}
			} else {
				// Target is one of the users device keys
				targetDeviceKeys, err := c.db.Accounts.GetDeviceKeys(r.Context(), targetUserID, targetObject.DeviceID, u.UserID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				} else if targetDeviceKeys == nil {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target device keys does not exist")
					return
				} else if !util.CompareSignedObjectsJSON(targetObject, targetDeviceKeys) {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Input device key does not match stored")
					return
				}
			}
		}
	}

	// Second pass: now we're happy with the objects, pull all the users keys (ie all the possible
	// signing keys), loop the target objects to sign and validate the signatures from those keys.
	userDevices, err := c.db.Accounts.GetUserDevices(r.Context(), u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	userKeys := make(map[id.KeyID]id.Ed25519, 3+len(userDevices))
	for _, device := range userDevices {
		deviceKeys, err := c.db.Accounts.GetDeviceKeys(r.Context(), u.UserID, device.ID, u.UserID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		keys := make(map[id.KeyID]id.Ed25519)
		for k, v := range deviceKeys.Keys {
			keys[id.KeyID(k)] = id.Ed25519(v)
		}
		maps.Copy(userKeys, keys)
	}
	userXSKeys, err := getCrossSigningKeys(u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	maps.Copy(userKeys, userXSKeys.Master.Keys)
	maps.Copy(userKeys, userXSKeys.SelfSigning.Keys)
	maps.Copy(userKeys, userXSKeys.UserSigning.Keys)

	for _, targetObjects := range req {
		for _, targetObject := range targetObjects {
			// Now we need to, for each signature we need to grab the relevant device or xs key
			for signingUserID, keyToSig := range targetObject.Signatures {
				if signingUserID != u.UserID {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid signing user ID")
					return
				}
				// For each signature check the relevant key has signed the object
				for keyID := range keyToSig {
					keys := map[id.KeyID]id.Ed25519{
						keyID: userKeys[keyID],
					}
					if verifyErr := util.VerifyObjectWithEd25519Keys(u.UserID, keys, targetObject); verifyErr != nil {
						util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Errorf("unable to verify object: %w", verifyErr).Error())
						return
					}
				}
			}
		}
	}

	// Third pass: now we've confirmed all target objects both a) exist/match and b) have valid
	// signatures, we can safely store those signatures.
	signaturesToStore := make(map[id.UserID]map[id.KeyID]signatures.Signatures, len(req))
	for targetUserID, targetObjects := range req {
		for targetKey, targetObject := range targetObjects {
			if _, ok := signaturesToStore[targetUserID]; !ok {
				signaturesToStore[targetUserID] = make(map[id.KeyID]signatures.Signatures, 1)
			}
			signaturesToStore[targetUserID][id.KeyID(targetKey)] = targetObject.Signatures
		}
	}
	if err := c.db.Accounts.StoreKeySignatures(r.Context(), signaturesToStore); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keysdevice_signingupload
func (c *ClientRoutes) UploadCrossSigningKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.UploadCrossSigningKeysReq](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	// Find the master key (in request or stored previously)
	var masterKey *mautrix.CrossSigningKeys
	if len(req.Master.Keys) > 0 {
		masterKey = &req.Master
	} else {
		existingKeys, err := c.db.Accounts.GetUserCrossSigningKeys(r.Context(), u.UserID, u.UserID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if existingKeys != nil && len(existingKeys.Master.Keys) > 0 {
			masterKey = &existingKeys.Master.CrossSigningKeys
		}
	}
	if masterKey == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "No master key available")
		return
	}

	keys := types.UserCrossSigningKeys{}

	if len(req.Master.Keys) > 0 {
		if len(req.Master.Signatures) > 0 {
			// Only verify the master key if it has signatures included as these are optional
			if verifyErr := util.VerifyObjectWithEd25519Keys(u.UserID, masterKey.Keys, req.Master); verifyErr != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Errorf("unable to verify master key: %w", verifyErr).Error())
				return
			}
		}
		keys.Master = types.CrossSigningKey{CrossSigningKeys: req.Master}
	}

	// Both user and self signing keys *must* be signed by the master key
	if len(req.SelfSigning.Keys) > 0 {
		if verifyErr := util.VerifyObjectWithEd25519Keys(u.UserID, masterKey.Keys, req.SelfSigning); verifyErr != nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
			return
		}
		keys.SelfSigning = types.CrossSigningKey{CrossSigningKeys: req.SelfSigning}
	}
	if len(req.UserSigning.Keys) > 0 {
		if verifyErr := util.VerifyObjectWithEd25519Keys(u.UserID, masterKey.Keys, req.UserSigning); verifyErr != nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
			return
		}
		keys.UserSigning = types.CrossSigningKey{CrossSigningKeys: req.UserSigning}
	}

	if err := c.db.Accounts.StoreUserCrossSigningKeys(r.Context(), u.UserID, u.DeviceID, keys); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3keysupload
func (c *ClientRoutes) UploadKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[types.ReqUploadKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	var storeDeviceKeys bool
	var deviceKeys *mautrix.DeviceKeys
	var oneTimeKeys map[id.KeyID]mautrix.OneTimeKey
	var fallbackKeys map[id.KeyAlgorithm]types.FallbackKey

	if req.DeviceKeys == nil {
		var err error
		deviceKeys, err = c.db.Accounts.GetDeviceKeys(r.Context(), u.UserID, u.DeviceID, u.UserID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if deviceKeys == nil {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing device keys")
			return
		}
	} else {
		if len(req.DeviceKeys.Keys) == 0 || len(req.DeviceKeys.Algorithms) == 0 {
			util.ResponseErrorMessageJSON(w, r, mautrix.MBadJSON, "Missing keys or algorithms")
			return
		} else if req.DeviceKeys.UserID != middleware.GetRequestUserID(r) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "UserID does not match")
			return
		} else if req.DeviceKeys.DeviceID != middleware.GetRequestDeviceID(r) {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "DeviceID does not match")
			return
		}

		// Check that the device keys object is signed by one of the keys it contains
		if len(req.DeviceKeys.Signatures) == 0 {
			hlog.FromRequest(r).Warn().Msg("Storing device key with no signatures")
		} else {
			if verifyErr := util.VerifyObjectWithKeyMap(u.UserID, req.DeviceKeys.Keys, req.DeviceKeys); verifyErr != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
				return
			}
		}

		deviceKeys = req.DeviceKeys
		storeDeviceKeys = true
	}

	if len(req.OneTimeKeys) > 0 {
		for _, otk := range req.OneTimeKeys {
			if otk.Fallback {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "One time keys cannot have fallback set")
				return
			}
			// One time keys must be signed by the device they belong to
			if verifyErr := util.VerifyObjectWithKeyMap(u.UserID, deviceKeys.Keys, otk); verifyErr != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
				return
			}
		}
		oneTimeKeys = req.OneTimeKeys
	}

	if len(req.FallbackKeys) > 0 {
		fallbackKeys = make(map[id.KeyAlgorithm]types.FallbackKey, len(req.FallbackKeys))
		for keyID, fbkey := range req.FallbackKeys {
			if !fbkey.Fallback {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Fallback keys must have fallback set")
				return
			}
			// Fallback keys must be signed by the device they belong to
			if verifyErr := util.VerifyObjectWithKeyMap(u.UserID, deviceKeys.Keys, fbkey); verifyErr != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
				return
			}
			algorithm, _ := keyID.Parse()
			if _, ok := fallbackKeys[algorithm]; ok {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "One fallback key allowed per algorithm")
				return
			}
			// Note: this does mean if a device re-uploads the same fallback key it'll be unused
			// until the next claim.
			fallbackKeys[algorithm] = types.FallbackKey{
				Key:   fbkey,
				KeyID: keyID,
			}
		}
	}

	if !storeDeviceKeys {
		deviceKeys = nil
	}

	otkCounts, err := c.db.Accounts.StoreKeysForDevice(
		r.Context(),
		u.UserID,
		u.DeviceID,
		deviceKeys,
		oneTimeKeys,
		fallbackKeys,
	)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, mautrix.RespUploadKeys{
		OneTimeKeyCounts: otkCounts,
	})
}

// Key changes API is expensive to calculate since we need to paginate all user device changes
// and filter by the ones relevant to the requesting user. However this avoids storing copies
// of the changes by room or user.
//
// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3keyschanges
func (c *ClientRoutes) GetKeyChanges(w http.ResponseWriter, r *http.Request) {
	fromVersions, err := util.VersionMapFromRequestQuery(r, "from")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	toVersions, err := util.VersionMapFromRequestQuery(r, "to")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	userID := middleware.GetRequestUserID(r)

	// Pull the users encrypted join memberships and then every member within each of those rooms,
	// we use this to filter the device changes.
	memberships, err := c.db.Rooms.GetUserJoinedMembershipsWithEncryption(r.Context(), userID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	userIDsInterestedIn := make(map[id.UserID]struct{}, len(memberships))
	userIDsInterestedIn[userID] = struct{}{}

	for roomID := range memberships {
		memberIDs, err := c.db.Rooms.GetCurrentRoomMemberships(r.Context(), roomID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		for memberID, membershipTup := range memberIDs {
			if membershipTup.Membership == event.MembershipJoin {
				userIDsInterestedIn[memberID] = struct{}{}
			}
		}
	}

	// Now we paginate the global device changes index between from -> to and select the userIDs
	// relevant to the requesting user.
	changedUserIDs := make(map[id.UserID]struct{}, 10)
	changes, err := c.db.Accounts.PaginateDeviceChanges(r.Context(), types.PaginationOptions{
		From: fromVersions[types.AccountsVersionKey],
		To:   toVersions[types.AccountsVersionKey],
	})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	for _, change := range changes {
		if _, ok := userIDsInterestedIn[change.UserID]; ok {
			changedUserIDs[change.UserID] = struct{}{}
		}
	}

	userIDs := slices.Collect(maps.Keys(changedUserIDs))
	util.ResponseJSON(w, r, http.StatusOK, struct {
		Changed []id.UserID `json:"changed,omitzero"`
	}{userIDs})
}
