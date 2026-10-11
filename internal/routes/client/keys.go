package client

import (
	"cmp"
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
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/signedjson"
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
		keys   map[id.UserID]map[id.DeviceID]map[id.KeyID]json.RawMessage
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
			resultsCh <- result{server, err, remoteClaimedKeys(res)}
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

		for userID, keys := range res.keys {
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

// Claimed keys pass through as received, so signatures over members we don't know still verify for
// the client
func remoteClaimedKeys(res fclient.RespClaimKeys) map[id.UserID]map[id.DeviceID]map[id.KeyID]json.RawMessage {
	claimed := make(map[id.UserID]map[id.DeviceID]map[id.KeyID]json.RawMessage, len(res.OneTimeKeys))
	for userID, devices := range res.OneTimeKeys {
		userKeys := make(map[id.DeviceID]map[id.KeyID]json.RawMessage, len(devices))
		for deviceID, keys := range devices {
			deviceKeys := make(map[id.KeyID]json.RawMessage, len(keys))
			for keyID, key := range keys {
				deviceKeys[id.KeyID(keyID)] = key
			}
			userKeys[id.DeviceID(deviceID)] = deviceKeys
		}
		claimed[id.UserID(userID)] = userKeys
	}
	return claimed
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
	maxRemoteKeyQueryRequests      = 10
	maxRemoteKeyQueryBatchUsers    = 100
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
		running  = make(chan struct{}, maxRemoteKeyQueryRequests)
	)
	for _, batch := range remoteKeyQueryBatches(missing) {
		running <- struct{}{}
		wg.Go(func() {
			defer func() { <-running }()
			batchKeys, err := c.queryServerUserKeys(ctx, batch.server, batch.users)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				zerolog.Ctx(ctx).Warn().
					Err(err).
					Str("server", batch.server).
					Int("users", len(batch.users)).
					Msg("Failed to fetch user keys from server")
				failures[batch.server] = remoteKeyQueryFailure(err)
				return
			}
			maps.Copy(keys, batchKeys)
		})
	}
	wg.Wait()
	return keys, failures
}

type remoteKeyQueryBatch struct {
	server string
	round  int
	users  map[id.UserID]mautrix.DeviceIDList
}

// Each server's users are split so a response fits the size limit, and the batches are ordered
// round by round so one large server can't hold every request slot
func remoteKeyQueryBatches(missing shared.MissingUserKeysByServer) []remoteKeyQueryBatch {
	var batches []remoteKeyQueryBatch
	for server, users := range missing {
		round := 0
		for userIDs := range slices.Chunk(slices.Sorted(maps.Keys(users)), maxRemoteKeyQueryBatchUsers) {
			batch := remoteKeyQueryBatch{server: server, round: round, users: make(map[id.UserID]mautrix.DeviceIDList, len(userIDs))}
			for _, userID := range userIDs {
				batch.users[userID] = users[userID]
			}
			batches = append(batches, batch)
			round++
		}
	}
	slices.SortStableFunc(batches, func(a, b remoteKeyQueryBatch) int { return cmp.Compare(a.round, b.round) })
	return batches
}

func (c *ClientRoutes) queryServerUserKeys(
	ctx context.Context,
	server string,
	userDeviceIDs map[id.UserID]mautrix.DeviceIDList,
) (map[id.UserID]shared.UserKeys, error) {
	query := make(map[string][]string, len(userDeviceIDs))
	for userID, deviceIDs := range userDeviceIDs {
		query[userID.String()] = util.StringersToStrs(deviceIDs)
	}

	body, _, err := c.keysClient.MakeFullRequest(ctx, federation.RequestParams{
		ServerName:   server,
		Method:       http.MethodPost,
		Path:         federation.URLPath{"v1", "user", "keys", "query"},
		Authenticate: true,
		RequestJSON:  map[string]any{"device_keys": query},
	})
	if err != nil {
		return nil, err
	}
	return shared.UserKeysFromQueryResponse(body, userDeviceIDs)
}

// Only an error response has a status to report: a success whose body is unusable is unavailable
func remoteKeyQueryFailure(err error) map[string]any {
	status := http.StatusServiceUnavailable
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.Response != nil && httpErr.Response.StatusCode >= http.StatusMultipleChoices {
		status = httpErr.Response.StatusCode
	}
	return map[string]any{"status": status, "message": err.Error()}
}

type respUploadSignatures struct {
	Failures map[id.UserID]map[string]*mautrix.RespError `json:"failures,omitempty"`
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keyssignaturesupload
func (c *ClientRoutes) UploadSignatures(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[map[id.UserID]map[string]types.SignatureUploadTarget](r)
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
	var userSigningKey types.SignedCrossSigningKey
	if userXSKeys != nil {
		userSigningKey = userXSKeys.UserSigning
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
	localTargets := make([]types.SignatureUploadTarget, 0, len(req))
	failures := make(map[id.UserID]map[string]*mautrix.RespError)

	// First pass: loop through the target objects to sign and validate they exist and match. Remote
	// targets are fully checked here and fail individually.
	for targetUserID, targetObjects := range req {
		for targetKey, target := range targetObjects {
			if targetUserID.Homeserver() != c.config.ServerName {
				failure := shared.RemoteSignatureTargetFailure(u.UserID, userSigningKey, remoteCaches[targetUserID], targetKey, target)
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
					Signatures: target.View.Signatures,
				})
				continue
			}

			if target.View.DeviceID == "" {
				// Target is one of the users cross signing keys
				targetXSKeys, err := getCrossSigningKeys(targetUserID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				}

				// Find and validate the relevant cross signing key matches the input
				var targetXSKey types.SignedCrossSigningKey
				if targetXSKeys != nil {
					switch id.Ed25519(targetKey) {
					case types.CrossSigningPublicKey(targetXSKeys.Master):
						targetXSKey = targetXSKeys.Master
					case types.CrossSigningPublicKey(targetXSKeys.SelfSigning):
						targetXSKey = targetXSKeys.SelfSigning
					case types.CrossSigningPublicKey(targetXSKeys.UserSigning):
						if u.UserID != targetUserID {
							// This should be impossible, if the client has access to the actual user
							// signing key of another user we screwed up big time.
							util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Cannot access another users user signing key")
							return
						}
						targetXSKey = targetXSKeys.UserSigning
					}
				}
				if targetXSKey.IsZero() {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target cross signing key does not exist")
					return
				} else if !target.Matches(targetXSKey.Raw) {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Input cross signing key does not match stored")
					return
				}
			} else {
				// Target is one of the users device keys. Synapse keys the target by the map key, so
				// a different device_id would sign one device and announce another.
				if target.View.DeviceID.String() != targetKey {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target device ID does not match its key")
					return
				}
				targetDeviceKeys, err := c.db.Accounts.GetDeviceKeys(ctx, targetUserID, target.View.DeviceID)
				if err != nil {
					util.ResponseErrorUnknownJSON(w, r, err)
					return
				} else if targetDeviceKeys.IsZero() {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Target device keys does not exist")
					return
				} else if !target.Matches(targetDeviceKeys.Raw) {
					util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Input device key does not match stored")
					return
				}
			}

			targets = append(targets, types.KeySignatureTarget{
				UserID:     targetUserID,
				KeyID:      id.KeyID(targetKey),
				DeviceID:   target.View.DeviceID,
				Signatures: target.View.Signatures,
			})
			localTargets = append(localTargets, target)
		}
	}

	// Second pass: now we're happy with the local objects, pull all the users keys (ie all the
	// possible signing keys), loop the target objects to sign and validate the signatures from those
	// keys.
	if len(localTargets) > 0 {
		userKeys, err := c.getSignerPublicKeys(ctx, u.UserID, userXSKeys)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}

		for _, target := range localTargets {
			if err := verifyLocalTargetSignatures(u.UserID, target, userKeys); err != nil {
				util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
				return
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

// Every signature on a local target must be the requester's, by one of their ed25519 keys, by key ID
// to unpadded base64 public key. Only ed25519 keys are verified, so any other key ID would pass
// unchecked.
func verifyLocalTargetSignatures(requestUserID id.UserID, target types.SignatureUploadTarget, signerKeys map[id.KeyID]string) error {
	for signingUserID, keyToSig := range target.View.Signatures {
		if signingUserID != requestUserID {
			return errors.New("Invalid signing user ID")
		}
		for keyID := range keyToSig {
			key, ok := signerKeys[keyID]
			algorithm, _ := keyID.Parse()
			if !ok || algorithm != id.KeyAlgorithmEd25519 {
				return fmt.Errorf("Unknown signing key: %s", keyID)
			} else if err := target.Verify(requestUserID, keyID.String(), key); err != nil {
				return fmt.Errorf("unable to verify object: %w", err)
			}
		}
	}
	return nil
}

// Public keys by key ID, unpadded base64
func (c *ClientRoutes) getSignerPublicKeys(
	ctx context.Context,
	userID id.UserID,
	xsKeys *types.UserCrossSigningKeys,
) (map[id.KeyID]string, error) {
	devices, err := c.db.Accounts.GetUserDevices(ctx, userID)
	if err != nil {
		return nil, err
	}
	keys := make(map[id.KeyID]string, 3+len(devices))
	for _, device := range devices {
		deviceKeys, err := c.db.Accounts.GetDeviceKeys(ctx, userID, device.ID)
		if err != nil {
			return nil, err
		}
		for keyID, key := range deviceKeys.View.Keys {
			keys[id.KeyID(keyID)] = key
		}
	}
	if xsKeys != nil {
		for _, xsKey := range []types.SignedCrossSigningKey{xsKeys.Master, xsKeys.SelfSigning, xsKeys.UserSigning} {
			for keyID, key := range xsKey.View.Keys {
				keys[keyID] = key.String()
			}
		}
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

type reqUploadCrossSigningKeys struct {
	Master      types.SignedCrossSigningKey `json:"master_key"`
	SelfSigning types.SignedCrossSigningKey `json:"self_signing_key"`
	UserSigning types.SignedCrossSigningKey `json:"user_signing_key"`
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3keysdevice_signingupload
func (c *ClientRoutes) UploadCrossSigningKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[reqUploadCrossSigningKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	var storedMaster types.SignedCrossSigningKey
	if len(req.Master.View.Keys) == 0 {
		existingKeys, err := c.db.Accounts.GetUserCrossSigningKeys(r.Context(), u.UserID, u.UserID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if existingKeys != nil {
			storedMaster = existingKeys.Master
		}
	}

	upload, master, respErr := parseUploadedCrossSigningKeys(u.UserID, req, storedMaster)
	if respErr != nil {
		util.ResponseErrorMessageJSON(w, r, *respErr, respErr.Err)
		return
	}

	err := c.db.Accounts.StoreUserCrossSigningKeys(r.Context(), u.UserID, u.DeviceID, upload, master)
	if errors.Is(err, types.ErrMasterKeyChanged) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Master key changed during the upload")
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

func canonicalUploadedKey[T any](uploaded types.SignedJSON[T]) (types.SignedJSON[T], *mautrix.RespError) {
	key, err := uploaded.Canonical()
	if err == nil {
		err = signedjson.CheckReservedNames(key.Raw)
	}
	if err != nil {
		return types.SignedJSON[T]{}, shared.KeyFailure(mautrix.MBadJSON.ErrCode, err.Error())
	}
	return key, nil
}

// Others sign through /keys/signatures/upload, which verifies them, so an upload keeps only the
// owner's signatures
func ownerSigned[T any](userID id.UserID, key types.SignedJSON[T], owner map[id.KeyID]string) (types.SignedJSON[T], *mautrix.RespError) {
	signed, err := key.WithOwnerSignatures(userID, owner)
	if err != nil {
		return types.SignedJSON[T]{}, shared.KeyFailure(mautrix.MBadJSON.ErrCode, err.Error())
	}
	return signed, nil
}

// A key without public keys counts as omitted. Self-signing and user-signing keys must be signed by
// the master key, uploaded or stored, whose public key is returned. A master key's signatures are not
// required, but a signature by its own key must verify. Signatures are checked over the keys as
// uploaded, which are returned canonical with only the owner's signatures.
func parseUploadedCrossSigningKeys(
	userID id.UserID,
	req reqUploadCrossSigningKeys,
	storedMaster types.SignedCrossSigningKey,
) (types.UserCrossSigningKeys, id.Ed25519, *mautrix.RespError) {
	var upload types.UserCrossSigningKeys
	for _, key := range []struct {
		uploaded types.SignedCrossSigningKey
		usage    id.CrossSigningUsage
		parsed   *types.SignedCrossSigningKey
	}{
		{req.Master, id.XSUsageMaster, &upload.Master},
		{req.SelfSigning, id.XSUsageSelfSigning, &upload.SelfSigning},
		{req.UserSigning, id.XSUsageUserSigning, &upload.UserSigning},
	} {
		if len(key.uploaded.View.Keys) == 0 {
			continue
		}
		parsed, respErr := canonicalUploadedKey(key.uploaded)
		if respErr != nil {
			return types.UserCrossSigningKeys{}, "", respErr
		} else if err := util.CheckCrossSigningKey(userID, key.usage, parsed); err != nil {
			return types.UserCrossSigningKeys{}, "", shared.KeyFailure(mautrix.MInvalidParam.ErrCode, err.Error())
		}
		*key.parsed = parsed
	}

	master := upload.Master
	if master.IsZero() {
		master = storedMaster
	}
	if len(master.View.Keys) == 0 {
		return types.UserCrossSigningKeys{}, "", shared.KeyFailure(mautrix.MInvalidParam.ErrCode, "No master key available")
	}

	if !upload.Master.IsZero() {
		publicKey := types.CrossSigningPublicKey(upload.Master).String()
		selfKeyID := id.NewKeyID(id.KeyAlgorithmEd25519, publicKey)
		if _, selfSigned := upload.Master.View.Signatures[userID][selfKeyID]; selfSigned {
			if err := upload.Master.Verify(userID, selfKeyID.String(), publicKey); err != nil {
				return types.UserCrossSigningKeys{}, "", shared.KeyFailure(mautrix.MInvalidParam.ErrCode, fmt.Sprintf("unable to verify master key: %s", err))
			}
		}
	}
	for _, key := range []types.SignedCrossSigningKey{upload.SelfSigning, upload.UserSigning} {
		if key.IsZero() {
			continue
		} else if err := types.VerifyKeys(key, userID, master.View.Keys); err != nil {
			return types.UserCrossSigningKeys{}, "", shared.KeyFailure(mautrix.MInvalidParam.ErrCode, err.Error())
		}
	}

	for _, key := range []*types.SignedCrossSigningKey{&upload.Master, &upload.SelfSigning, &upload.UserSigning} {
		if key.IsZero() {
			continue
		}
		var respErr *mautrix.RespError
		if *key, respErr = ownerSigned(userID, *key, key.View.Signatures[userID]); respErr != nil {
			return types.UserCrossSigningKeys{}, "", respErr
		}
	}
	return upload, types.CrossSigningPublicKey(master), nil
}

type reqUploadKeys struct {
	DeviceKeys   types.SignedDeviceKeys              `json:"device_keys"`
	OneTimeKeys  map[id.KeyID]types.SignedOneTimeKey `json:"one_time_keys"`
	FallbackKeys map[id.KeyID]types.SignedOneTimeKey `json:"fallback_keys"`
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3keysupload
func (c *ClientRoutes) UploadKeys(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[reqUploadKeys](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}

	u := middleware.GetRequestUserDevice(r)

	// One-time and fallback keys are verified against these keys, which are only stored when uploaded
	var deviceKeys types.SignedDeviceKeys
	storeDeviceKeys := !req.DeviceKeys.IsZero()
	if !storeDeviceKeys {
		var err error
		deviceKeys, err = c.db.Accounts.GetDeviceKeys(r.Context(), u.UserID, u.DeviceID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if deviceKeys.IsZero() {
			util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Missing device keys")
			return
		}
	} else {
		if deviceKeys, respErr = parseUploadedDeviceKeys(u.UserID, u.DeviceID, req.DeviceKeys); respErr != nil {
			util.ResponseErrorMessageJSON(w, r, *respErr, respErr.Err)
			return
		} else if len(deviceKeys.View.Signatures) == 0 {
			hlog.FromRequest(r).Warn().Msg("Storing device key with no signatures")
		}
	}

	oneTimeKeys := make(map[id.KeyID]types.SignedOneTimeKey, len(req.OneTimeKeys))
	for keyID, uploaded := range req.OneTimeKeys {
		if oneTimeKeys[keyID], respErr = parseUploadedOneTimeKey(u.UserID, deviceKeys, uploaded, false); respErr != nil {
			util.ResponseErrorMessageJSON(w, r, *respErr, respErr.Err)
			return
		}
	}

	fallbackKeys := make(map[id.KeyAlgorithm]types.FallbackKey, len(req.FallbackKeys))
	for keyID, uploaded := range req.FallbackKeys {
		key, respErr := parseUploadedOneTimeKey(u.UserID, deviceKeys, uploaded, true)
		if respErr != nil {
			util.ResponseErrorMessageJSON(w, r, *respErr, respErr.Err)
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
			Key:   key,
			KeyID: keyID,
		}
	}

	if !storeDeviceKeys {
		deviceKeys = types.SignedDeviceKeys{}
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

// Device keys must belong to the uploading device and be signed by every ed25519 key they hold, when
// signed at all. Signatures are checked over the keys as uploaded, which are returned canonical with
// only the owner's signatures.
func parseUploadedDeviceKeys(userID id.UserID, deviceID id.DeviceID, uploaded types.SignedDeviceKeys) (types.SignedDeviceKeys, *mautrix.RespError) {
	keys, respErr := canonicalUploadedKey(uploaded)
	if respErr != nil {
		return types.SignedDeviceKeys{}, respErr
	} else if len(keys.View.Keys) == 0 || len(keys.View.Algorithms) == 0 {
		return types.SignedDeviceKeys{}, shared.KeyFailure(mautrix.MBadJSON.ErrCode, "Missing keys or algorithms")
	} else if keys.View.UserID != userID {
		return types.SignedDeviceKeys{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, "UserID does not match")
	} else if keys.View.DeviceID != deviceID {
		return types.SignedDeviceKeys{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, "DeviceID does not match")
	} else if len(keys.View.Signatures) == 0 {
		return keys, nil
	} else if err := types.VerifyKeys(keys, userID, keys.View.Keys); err != nil {
		return types.SignedDeviceKeys{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, err.Error())
	}
	return ownerSigned(userID, keys, keys.View.Signatures[userID])
}

// One-time and fallback keys must be signed by the device they belong to, and carry the fallback flag
// exactly when they are fallback keys. Signatures are checked over the key as uploaded, which is
// returned canonical.
func parseUploadedOneTimeKey(
	userID id.UserID,
	deviceKeys types.SignedDeviceKeys,
	uploaded types.SignedOneTimeKey,
	fallback bool,
) (types.SignedOneTimeKey, *mautrix.RespError) {
	key, respErr := canonicalUploadedKey(uploaded)
	if respErr != nil {
		return types.SignedOneTimeKey{}, respErr
	} else if fallback && !key.View.Fallback {
		return types.SignedOneTimeKey{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, "Fallback keys must have fallback set")
	} else if !fallback && key.View.Fallback {
		return types.SignedOneTimeKey{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, "One time keys cannot have fallback set")
	} else if err := types.VerifyKeys(key, userID, deviceKeys.View.Keys); err != nil {
		return types.SignedOneTimeKey{}, shared.KeyFailure(mautrix.MInvalidParam.ErrCode, err.Error())
	}
	return key, nil
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
