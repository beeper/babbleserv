package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
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

	keys, missing, err := shared.CollectUserKeys(r.Context(), c.config, c.db, req.DeviceKeys, u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	var wg sync.WaitGroup
	wg.Go(func() { c.ensureRemoteDeviceCaches(r, u.UserID, missing) })

	// Live results are returned without populating the cache: they carry no stream ID
	ctx, cancel := context.WithTimeout(r.Context(), remoteKeyQueryTimeout(req.Timeout))
	liveKeys, failures := c.queryRemoteUserKeys(ctx, missing)
	cancel()
	wg.Wait()
	maps.Copy(keys, liveKeys)

	resp, err := shared.BuildUserKeysResponse(r.Context(), c.db, req.DeviceKeys, keys, u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if len(failures) > 0 {
		resp.Failures = failures
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

// A remote user without a cache gets one when a local user sharing an encrypted room with them
// queries their keys; the worker fills it, so this query is still served live
func (c *ClientRoutes) ensureRemoteDeviceCaches(r *http.Request, requester id.UserID, missing shared.MissingUserKeysByServer) {
	log := hlog.FromRequest(r)
	var userIDs []id.UserID
	for _, users := range missing {
		for userID := range users {
			userIDs = append(userIDs, userID)
		}
	}
	if len(userIDs) == 0 {
		return
	}
	sharing, err := c.db.Rooms.UsersSharingEncryptedRoom(r.Context(), requester, userIDs)
	if err != nil {
		log.Err(err).Msg("Failed to check shared encrypted rooms for remote device caches")
		return
	}
	userIDs = userIDs[:0]
	for userID, shared := range sharing {
		if shared {
			userIDs = append(userIDs, userID)
		}
	}
	if len(userIDs) == 0 {
		return
	}
	if created, err := c.db.Accounts.EnsureRemoteDeviceCaches(r.Context(), userIDs); err != nil {
		log.Err(err).Msg("Failed to create remote device caches")
	} else if len(created) > 0 {
		log.Debug().Int("created", len(created)).Msg("Created remote device caches")
	}
}

const (
	maxRemoteKeyQueryServers       = 10
	minRemoteKeyQueryTimeoutMillis = 1000
	maxRemoteKeyQueryTimeoutMillis = 10_000
)

func remoteKeyQueryTimeout(timeoutMillis int64) time.Duration {
	if timeoutMillis <= 0 {
		timeoutMillis = maxRemoteKeyQueryTimeoutMillis
	}
	return time.Duration(min(max(timeoutMillis, minRemoteKeyQueryTimeoutMillis), maxRemoteKeyQueryTimeoutMillis)) * time.Millisecond
}

func (c *ClientRoutes) queryRemoteUserKeys(
	ctx context.Context,
	missing shared.MissingUserKeysByServer,
) (map[id.UserID]shared.UserKeys, map[string]any) {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		keys     = make(map[id.UserID]shared.UserKeys)
		failures = make(map[string]any)
		running  = make(chan struct{}, maxRemoteKeyQueryServers)
	)
	for server, userDeviceIDs := range missing {
		running <- struct{}{}
		wg.Go(func() {
			defer func() { <-running }()
			res, err := c.queryServerUserKeys(ctx, server, userDeviceIDs)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				zerolog.Ctx(ctx).Warn().
					Err(err).
					Str("server", server).
					Msg("Failed to fetch user keys from server")
				failures[server] = remoteKeyQueryFailure(err)
				return
			}
			for userID := range userDeviceIDs {
				keys[userID] = shared.UserKeysFromQueryResponse(userID, res)
			}
		})
	}
	wg.Wait()
	return keys, failures
}

func (c *ClientRoutes) queryServerUserKeys(
	ctx context.Context,
	server string,
	userDeviceIDs map[id.UserID]mautrix.DeviceIDList,
) (mautrix.RespQueryKeys, error) {
	query := make(map[string][]string, len(userDeviceIDs))
	for userID, deviceIDs := range userDeviceIDs {
		query[userID.String()] = util.StringersToStrs(deviceIDs)
	}

	res, err := c.fclient.QueryKeys(ctx, spec.ServerName(c.config.ServerName), spec.ServerName(server), query)
	if err != nil {
		return mautrix.RespQueryKeys{}, err
	}

	// Gross: convert fclient.RespQueryKeys -> mautrix.RespQueryKeys
	b, err := json.Marshal(res)
	if err != nil {
		return mautrix.RespQueryKeys{}, err
	}
	var converted mautrix.RespQueryKeys
	err = json.Unmarshal(b, &converted)
	return converted, err
}

func remoteKeyQueryFailure(err error) map[string]any {
	status := http.StatusServiceUnavailable
	var httpErr gomatrix.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
	}
	return map[string]any{"status": status, "message": err.Error()}
}

type respUploadSignatures struct {
	Failures map[id.UserID]map[string]*mautrix.RespError `json:"failures,omitempty"`
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keyssignaturesupload
func (c *ClientRoutes) UploadSignatures(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[mautrix.ReqUploadSignatures](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	ctx := r.Context()
	u := middleware.GetRequestUserDevice(r)

	// Memoize cross signing keys since they contain 3 keys and might need dupe fetches
	getCrossSigningKeys := util.MemoizeMap(func(uid id.UserID) (*types.UserCrossSigningKeys, error) {
		return c.db.Accounts.GetUserCrossSigningKeys(ctx, uid, u.UserID)
	}, len(req))

	userXSKeys, err := getCrossSigningKeys(u.UserID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	var userSigningKey *mautrix.CrossSigningKeys
	if userXSKeys != nil && userXSKeys.UserSigning.KeyID() != "" {
		userSigningKey = &userXSKeys.UserSigning.CrossSigningKeys
	}

	remoteUserIDs := make([]id.UserID, 0, len(req))
	for targetUserID := range req {
		if targetUserID.Homeserver() != c.config.ServerName {
			remoteUserIDs = append(remoteUserIDs, targetUserID)
		}
	}
	remoteCaches, err := c.db.Accounts.GetRemoteDeviceCaches(ctx, remoteUserIDs)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	targets := make([]types.KeySignatureTarget, 0, len(req))
	localObjects := make([]mautrix.ReqKeysSignatures, 0, len(req))
	failures := make(map[id.UserID]map[string]*mautrix.RespError)

	// First pass: loop through the target objects to sign and validate they exist and match. Remote
	// targets are fully checked here and fail individually.
	for targetUserID, targetObjects := range req {
		for targetKey, targetObject := range targetObjects {
			if targetUserID.Homeserver() != c.config.ServerName {
				failure := shared.RemoteSignatureTargetFailure(u.UserID, userSigningKey, remoteCaches[targetUserID], targetKey, targetObject)
				if failure != nil {
					if failures[targetUserID] == nil {
						failures[targetUserID] = make(map[string]*mautrix.RespError, 1)
					}
					failures[targetUserID][targetKey] = failure
					continue
				}
				targets = append(targets, types.KeySignatureTarget{
					UserID:     targetUserID,
					KeyID:      id.KeyID(targetKey),
					Signatures: targetObject.Signatures,
				})
				continue
			}

			if targetObject.DeviceID == "" {
				// Target is one of the users cross signing keys
				targetXSKeys, err := getCrossSigningKeys(targetUserID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				}

				// Find and validate the relevant cross signing key matches the input
				var targetXSKey *types.CrossSigningKey
				if targetXSKeys != nil {
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
				}
				if targetXSKey == nil {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target cross signing key does not exist")
					return
				} else if !util.CompareSignedObjectsJSON(targetObject, targetXSKey) {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Input cross signing key does not match stored")
					return
				}
			} else {
				// Target is one of the users device keys. Synapse keys the target by the map key, so
				// a different device_id would sign one device and announce another.
				if targetObject.DeviceID.String() != targetKey {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target device ID does not match its key")
					return
				}
				targetDeviceKeys, err := c.db.Accounts.GetDeviceKeys(ctx, targetUserID, targetObject.DeviceID)
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

			targets = append(targets, types.KeySignatureTarget{
				UserID:     targetUserID,
				KeyID:      id.KeyID(targetKey),
				DeviceID:   targetObject.DeviceID,
				Signatures: targetObject.Signatures,
			})
			localObjects = append(localObjects, targetObject)
		}
	}

	// Second pass: now we're happy with the local objects, pull all the users keys (ie all the
	// possible signing keys), loop the target objects to sign and validate the signatures from those
	// keys.
	if len(localObjects) > 0 {
		userKeys, err := c.getSignerPublicKeys(ctx, u.UserID, userXSKeys)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		for _, targetObject := range localObjects {
			// Now we need to, for each signature we need to grab the relevant device or xs key
			for signingUserID, keyToSig := range targetObject.Signatures {
				if signingUserID != u.UserID {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid signing user ID")
					return
				}
				// For each signature check the relevant key has signed the object. Only ed25519
				// keys are verified, so any other key ID would pass unchecked.
				for keyID := range keyToSig {
					key, ok := userKeys[keyID]
					algorithm, _ := keyID.Parse()
					if !ok || algorithm != id.KeyAlgorithmEd25519 {
						util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Sprintf("Unknown signing key: %s", keyID))
						return
					}
					keys := map[id.KeyID]id.Ed25519{
						keyID: key,
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
	if len(targets) > 0 {
		if err := c.db.Accounts.StoreKeySignatures(ctx, u.UserID, targets); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		if err := c.notifyCrossSignatures(ctx, u.UserID, targets); err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, respUploadSignatures{Failures: failures})
}

func (c *ClientRoutes) getSignerPublicKeys(
	ctx context.Context,
	userID id.UserID,
	xsKeys *types.UserCrossSigningKeys,
) (map[id.KeyID]id.Ed25519, error) {
	devices, err := c.db.Accounts.GetUserDevices(ctx, userID)
	if err != nil {
		return nil, err
	}
	keys := make(map[id.KeyID]id.Ed25519, 3+len(devices))
	for _, device := range devices {
		deviceKeys, err := c.db.Accounts.GetDeviceKeys(ctx, userID, device.ID)
		if err != nil {
			return nil, err
		} else if deviceKeys == nil {
			continue
		}
		for keyID, key := range deviceKeys.Keys {
			keys[id.KeyID(keyID)] = id.Ed25519(key)
		}
	}
	if xsKeys != nil {
		maps.Copy(keys, xsKeys.Master.Keys)
		maps.Copy(keys, xsKeys.SelfSigning.Keys)
		maps.Copy(keys, xsKeys.UserSigning.Keys)
	}
	return keys, nil
}

// Cross-signatures are private to the signer, so only their own devices see the target change. No
// change record, device-list version or EDU. Every accepted target is announced, changed or not, so
// retrying an upload whose notification failed still reaches the signer's other devices.
func (c *ClientRoutes) notifyCrossSignatures(ctx context.Context, requestUserID id.UserID, targets []types.KeySignatureTarget) error {
	targetUserIDs := make(map[id.UserID]struct{}, len(targets))
	for _, target := range targets {
		if target.UserID != requestUserID {
			targetUserIDs[target.UserID] = struct{}{}
		}
	}
	if len(targetUserIDs) == 0 {
		return nil
	}
	tds := make([]*types.ToDevice, 0, len(targetUserIDs))
	for targetUserID := range targetUserIDs {
		tds = append(tds, &types.ToDevice{
			UserID:   requestUserID,
			DeviceID: id.DeviceID("*"),
			Sender:   targetUserID,
			Type:     types.BabbleservLocalDeviceChange,
		})
	}
	_, err := c.db.SendToDeviceEvents(ctx, tds, transient.SendToDeviceOptions{})
	return err
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
		deviceKeys, err = c.db.Accounts.GetDeviceKeys(r.Context(), u.UserID, u.DeviceID)
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
		members, err := c.db.Rooms.RoomMembers(r.Context(), roomID, event.MembershipJoin)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
		for memberID := range members {
			userIDsInterestedIn[memberID] = struct{}{}
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
