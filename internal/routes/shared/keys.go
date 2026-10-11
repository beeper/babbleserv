package shared

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type MissingUserKeyClaimsByServer map[string]map[id.UserID]map[id.DeviceID]id.KeyAlgorithm

func ClaimUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.OneTimeKeysRequest,
) (mautrix.RespClaimKeys, MissingUserKeyClaimsByServer, error) {
	resp := mautrix.RespClaimKeys{
		OneTimeKeys: make(map[id.UserID]map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey),
	}

	serverToUserDevices := make(MissingUserKeyClaimsByServer, len(req))

	for userID, deviceIDToAlgorithm := range req {
		// Build per server remote claim requests
		if userID.Homeserver() != config.ServerName {
			if _, ok := serverToUserDevices[userID.Homeserver()]; !ok {
				serverToUserDevices[userID.Homeserver()] = make(map[id.UserID]map[id.DeviceID]id.KeyAlgorithm, 1)
			}
			if _, ok := serverToUserDevices[userID.Homeserver()][userID]; !ok {
				serverToUserDevices[userID.Homeserver()][userID] = make(map[id.DeviceID]id.KeyAlgorithm, 1)
			}
			serverToUserDevices[userID.Homeserver()][userID] = deviceIDToAlgorithm
			continue
		}

		// Claim keys for local users
		for deviceID, algorithm := range deviceIDToAlgorithm {
			keys, err := db.Accounts.ClaimOrGetPreKeys(ctx, userID, deviceID, algorithm, 1)
			if err != nil {
				return resp, nil, err
			} else if keys == nil {
				continue
			}
			if _, ok := resp.OneTimeKeys[userID]; !ok {
				resp.OneTimeKeys[userID] = make(map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey, 1)
			}
			resp.OneTimeKeys[userID][deviceID] = keys
		}
	}

	return resp, serverToUserDevices, nil
}

type MissingUserKeysByServer map[string]map[id.UserID]mautrix.DeviceIDList

// UserKeys is one user's keys before filtering to a request, each a signed object as raw JSON, nil
// when absent. Signatures other than the owner's are dropped and unsigned replaced when added to a
// response.
type UserKeys struct {
	Devices     map[id.DeviceID]UserDeviceKeys
	Master      json.RawMessage
	SelfSigning json.RawMessage
	// Only ever set for the requester's own local keys
	UserSigning json.RawMessage
}

type UserDeviceKeys struct {
	Keys        json.RawMessage
	DisplayName string
}

// GetUserKeys answers a federation key query: no requester, so no signature overlay or user-signing key
func GetUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
) (types.RespQueryKeys, error) {
	keys, _, err := CollectUserKeys(ctx, config, db, req, "")
	if err != nil {
		return types.RespQueryKeys{}, err
	}
	return BuildUserKeysResponse(ctx, db, req, keys, "")
}

// CollectUserKeys reads local users' keys and remote users' valid caches. Remote users without a
// valid cache are returned by server, to be queried live.
func CollectUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
	requestUserID id.UserID,
) (map[id.UserID]UserKeys, MissingUserKeysByServer, error) {
	keys := make(map[id.UserID]UserKeys, len(req))
	remoteUserIDs := make([]id.UserID, 0, len(req))

	for userID := range req {
		if userID.Homeserver() != config.ServerName {
			remoteUserIDs = append(remoteUserIDs, userID)
			continue
		}
		userKeys, err := getLocalUserKeys(ctx, db, userID, requestUserID)
		if err != nil {
			return nil, nil, err
		}
		keys[userID] = userKeys
	}

	caches, err := db.Accounts.GetRemoteDeviceCaches(ctx, remoteUserIDs)
	if err != nil {
		return nil, nil, err
	}

	missing := make(MissingUserKeysByServer)
	for _, userID := range remoteUserIDs {
		cache, ok := caches[userID]
		if !ok {
			server := userID.Homeserver()
			if missing[server] == nil {
				missing[server] = make(map[id.UserID]mautrix.DeviceIDList, 1)
			}
			missing[server][userID] = req[userID]
			continue
		}
		keys[userID] = UserKeysFromRemoteCache(cache)
	}

	return keys, missing, nil
}

func getLocalUserKeys(
	ctx context.Context,
	db *databases.Databases,
	userID id.UserID,
	requestUserID id.UserID,
) (UserKeys, error) {
	snapshot, err := db.Accounts.GetLocalUserDevicesSnapshot(ctx, userID)
	if err != nil || snapshot == nil {
		return UserKeys{}, err
	}

	keys := UserKeys{Devices: make(map[id.DeviceID]UserDeviceKeys, len(snapshot.Devices))}
	for _, device := range snapshot.Devices {
		if device.Keys == nil {
			continue
		}
		deviceKeys, err := marshalLocalKey(device.Keys)
		if err != nil {
			return UserKeys{}, err
		}
		keys.Devices[device.Device.ID] = UserDeviceKeys{Keys: deviceKeys, DisplayName: device.Device.DisplayName}
	}
	if keys.Master, err = marshalLocalKey(snapshot.MasterKey); err != nil {
		return UserKeys{}, err
	}
	if keys.SelfSigning, err = marshalLocalKey(snapshot.SelfSigningKey); err != nil {
		return UserKeys{}, err
	}

	if userID == requestUserID {
		crossSigningKeys, err := db.Accounts.GetUserCrossSigningKeys(ctx, userID, requestUserID)
		if err != nil {
			return UserKeys{}, err
		} else if crossSigningKeys != nil && crossSigningKeys.UserSigning.KeyID() != "" {
			if keys.UserSigning, err = marshalLocalKey(&crossSigningKeys.UserSigning.CrossSigningKeys); err != nil {
				return UserKeys{}, err
			}
		}
	}

	return keys, nil
}

func marshalLocalKey[T any](key *T) (json.RawMessage, error) {
	if key == nil {
		return nil, nil
	}
	return json.Marshal(key)
}

func UserKeysFromRemoteCache(cache *types.RemoteDeviceCache) UserKeys {
	keys := UserKeys{
		Devices:     make(map[id.DeviceID]UserDeviceKeys, len(cache.Devices)),
		Master:      cache.MasterKey,
		SelfSigning: cache.SelfSigningKey,
	}
	for deviceID, device := range cache.Devices {
		// A device without E2E keys is valid device-list data but has nothing to return
		if len(device.Keys) > 0 {
			keys.Devices[deviceID] = UserDeviceKeys{Keys: device.Keys, DisplayName: device.DisplayName}
		}
	}
	return keys
}

// Each user's devices stay raw so a malformed value drops only that user, rather than failing the
// server or serving them with no devices. Failures and user-signing keys are never taken from a remote.
type remoteQueryKeysResponse struct {
	DeviceKeys      map[id.UserID]json.RawMessage `json:"device_keys"`
	MasterKeys      map[id.UserID]json.RawMessage `json:"master_keys"`
	SelfSigningKeys map[id.UserID]json.RawMessage `json:"self_signing_keys"`
}

// UserKeysFromQueryResponse takes the requested users' keys from a live federation query, applying
// the cache ingest's validation rules. Only the requested devices are validated when a user's
// request names any.
func UserKeysFromQueryResponse(body []byte, req map[id.UserID]mautrix.DeviceIDList) (map[id.UserID]UserKeys, error) {
	var res remoteQueryKeysResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("invalid key query response: %w", err)
	}
	keys := make(map[id.UserID]UserKeys, len(req))
	for userID, deviceIDs := range req {
		var devices map[id.DeviceID]json.RawMessage
		if raw, ok := res.DeviceKeys[userID]; ok && json.Unmarshal(raw, &devices) != nil {
			continue
		}
		// No MaxRemoteDevicesPerUser cap: the cache refuses such users outright, so this is their only
		// source, and the response size limit bounds the work
		userKeys := UserKeys{Devices: make(map[id.DeviceID]UserDeviceKeys, len(devices))}
		for deviceID, raw := range requestedDevices(devices, deviceIDs) {
			if device, valid := util.ParseRemoteQueriedDevice(userID, deviceID, raw); valid {
				userKeys.Devices[deviceID] = UserDeviceKeys{Keys: device.Keys, DisplayName: device.DisplayName}
			}
		}
		userKeys.Master, _ = util.ValidateOptionalCrossSigningKey(userID, id.XSUsageMaster, res.MasterKeys[userID])
		userKeys.SelfSigning, _ = util.ValidateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, res.SelfSigningKeys[userID])
		keys[userID] = userKeys
	}
	return keys, nil
}

// BuildUserKeysResponse filters each user's keys to the request and overlays the requester's own
// signatures of them, read in one transaction
func BuildUserKeysResponse(
	ctx context.Context,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
	keys map[id.UserID]UserKeys,
	requestUserID id.UserID,
) (types.RespQueryKeys, error) {
	var overlay map[id.UserID]map[id.KeyID]map[id.KeyID]string
	if targets := signatureOverlayTargets(req, keys, requestUserID); len(targets) > 0 {
		var err error
		if overlay, err = db.Accounts.GetKeySignaturesBySigner(ctx, requestUserID, targets); err != nil {
			return types.RespQueryKeys{}, err
		}
	}

	resp := newQueryKeysResponse(len(keys))
	for userID, userKeys := range keys {
		if err := AddUserKeys(&resp, userID, req[userID], userKeys, requestUserID, overlay[userID]); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("user_id", userID).Msg("Skipped user keys that could not be served")
		}
	}
	return resp, nil
}

func newQueryKeysResponse(users int) types.RespQueryKeys {
	return types.RespQueryKeys{
		DeviceKeys:      make(map[id.UserID]map[id.DeviceID]json.RawMessage, users),
		MasterKeys:      make(map[id.UserID]json.RawMessage, users),
		SelfSigningKeys: make(map[id.UserID]json.RawMessage, users),
		UserSigningKeys: make(map[id.UserID]json.RawMessage, 1),
	}
}

func signatureOverlayTargets(
	req mautrix.DeviceKeysRequest,
	keys map[id.UserID]UserKeys,
	requestUserID id.UserID,
) map[id.UserID]map[id.KeyID]struct{} {
	if requestUserID == "" {
		return nil
	}
	targets := make(map[id.UserID]map[id.KeyID]struct{}, len(keys))
	for userID, userKeys := range keys {
		// The requester's signatures of their own keys are already the owner's
		if userID == requestUserID {
			continue
		}
		keyIDs := make(map[id.KeyID]struct{})
		for deviceID := range requestedDevices(userKeys.Devices, req[userID]) {
			keyIDs[id.KeyID(deviceID)] = struct{}{}
		}
		for _, key := range []json.RawMessage{userKeys.Master, userKeys.SelfSigning} {
			if publicKey := crossSigningPublicKey(key); publicKey != "" {
				keyIDs[id.KeyID(publicKey)] = struct{}{}
			}
		}
		if len(keyIDs) > 0 {
			targets[userID] = keyIDs
		}
	}
	return targets
}

// AddUserKeys adds the user's requested devices, all of them for an empty list, keeping only the
// owner's signatures plus the requester's overlay. The user-signing key is only added for the
// requester themselves. An object that cannot be served is skipped and returned in the error.
func AddUserKeys(
	resp *types.RespQueryKeys,
	userID id.UserID,
	deviceIDs mautrix.DeviceIDList,
	keys UserKeys,
	requestUserID id.UserID,
	overlay map[id.KeyID]map[id.KeyID]string,
) error {
	var errs []error
	devices := resp.DeviceKeys[userID]
	if devices == nil {
		devices = make(map[id.DeviceID]json.RawMessage, len(keys.Devices))
		resp.DeviceKeys[userID] = devices
	}
	for deviceID, device := range requestedDevices(keys.Devices, deviceIDs) {
		var unsigned map[string]any
		if device.DisplayName != "" {
			unsigned = map[string]any{"device_display_name": device.DisplayName}
		}
		sigs := permittedSignatures(device.Keys, userID, requestUserID, overlay[id.KeyID(deviceID)])
		deviceKeys, err := util.OverlaySignedJSON(device.Keys, sigs, unsigned)
		if err != nil {
			errs = append(errs, fmt.Errorf("device keys for %s: %w", deviceID, err))
			continue
		}
		devices[deviceID] = deviceKeys
	}

	addCrossSigningKey := func(name string, key json.RawMessage, served map[id.UserID]json.RawMessage) {
		if len(key) == 0 {
			return
		}
		permitted, err := permittedCrossSigningKey(key, userID, requestUserID, overlay)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s key: %w", name, err))
			return
		}
		served[userID] = permitted
	}
	addCrossSigningKey("master", keys.Master, resp.MasterKeys)
	addCrossSigningKey("self-signing", keys.SelfSigning, resp.SelfSigningKeys)
	if userID == requestUserID {
		addCrossSigningKey("user-signing", keys.UserSigning, resp.UserSigningKeys)
	}
	return errors.Join(errs...)
}

func requestedDevices[T any](devices map[id.DeviceID]T, deviceIDs mautrix.DeviceIDList) map[id.DeviceID]T {
	if len(deviceIDs) == 0 {
		return devices
	}
	requested := make(map[id.DeviceID]T, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		if device, ok := devices[deviceID]; ok {
			requested[deviceID] = device
		}
	}
	return requested
}

func permittedCrossSigningKey(
	key json.RawMessage,
	userID id.UserID,
	requestUserID id.UserID,
	overlay map[id.KeyID]map[id.KeyID]string,
) (json.RawMessage, error) {
	sigs := permittedSignatures(key, userID, requestUserID, overlay[id.KeyID(crossSigningPublicKey(key))])
	return util.OverlaySignedJSON(key, sigs, nil)
}

// Cross-signatures of a cross-signing key are indexed by its public key
func crossSigningPublicKey(key json.RawMessage) id.Ed25519 {
	var publicKey id.Ed25519
	gjson.GetBytes(key, "keys").ForEach(func(_, value gjson.Result) bool {
		publicKey = id.Ed25519(value.String())
		return false
	})
	return publicKey
}

// Only the owner's signatures are public. Others' signatures are private to their signer, so the
// requester only sees their own, from the overlay.
func permittedSignatures(
	object json.RawMessage,
	userID id.UserID,
	requestUserID id.UserID,
	overlay map[id.KeyID]string,
) signatures.Signatures {
	permitted := make(signatures.Signatures, 2)
	var owner map[id.KeyID]string
	ownerJSON := gjson.GetBytes(object, "signatures."+gjson.Escape(userID.String())).Raw
	if json.Unmarshal([]byte(ownerJSON), &owner) == nil && len(owner) > 0 {
		permitted[userID] = owner
	}
	if requestUserID != userID && len(overlay) > 0 {
		permitted[requestUserID] = overlay
	}
	return permitted
}

const errCodeInvalidSignature = "M_INVALID_SIGNATURE"

func signatureUploadFailure(errCode string, message string) *mautrix.RespError {
	return &mautrix.RespError{ErrCode: errCode, Err: message}
}

// RemoteSignatureTargetFailure checks a signature upload for another server's user: only their
// cached master key can be signed, by the requester's user-signing key. An uncached key is never
// fetched live. Signatures by other users are ignored, they are never stored. The target is checked
// as the raw JSON the client signed, since the served master key may carry members the mautrix
// structs drop.
func RemoteSignatureTargetFailure(
	requestUserID id.UserID,
	userSigningKey *mautrix.CrossSigningKeys,
	cache *types.RemoteDeviceCache,
	targetKey string,
	target json.RawMessage,
) *mautrix.RespError {
	var targetObject struct {
		DeviceID   id.DeviceID           `json:"device_id"`
		Signatures signatures.Signatures `json:"signatures"`
	}
	if err := json.Unmarshal(target, &targetObject); err != nil {
		return signatureUploadFailure(mautrix.MBadJSON.ErrCode, "Invalid signature target")
	} else if targetObject.DeviceID != "" {
		return signatureUploadFailure(mautrix.MInvalidParam.ErrCode, "Cannot sign another server's user's device")
	}

	if cache == nil || len(cache.MasterKey) == 0 {
		return signatureUploadFailure(mautrix.MNotFound.ErrCode, "Target user's master key is not known")
	} else if targetKey != crossSigningPublicKey(cache.MasterKey).String() {
		return signatureUploadFailure(mautrix.MNotFound.ErrCode, "Target key is not the user's master key")
	} else if !util.CompareSignedJSON(target, cache.MasterKey) {
		return signatureUploadFailure(mautrix.MInvalidParam.ErrCode, "Input master key does not match stored")
	}

	requesterSignatures := targetObject.Signatures[requestUserID]
	if userSigningKey == nil || len(requesterSignatures) == 0 {
		return signatureUploadFailure(errCodeInvalidSignature, "Missing signature by the user-signing key")
	}
	userSigningKeyID := id.NewKeyID(id.KeyAlgorithmEd25519, userSigningKey.FirstKey().String())
	for keyID := range requesterSignatures {
		if keyID != userSigningKeyID {
			return signatureUploadFailure(errCodeInvalidSignature, "Signature is not by the user-signing key")
		}
	}
	publicKey, err := util.Base64Decode(userSigningKey.FirstKey().String())
	if err == nil {
		err = util.VerifyJSON(target, requestUserID.String(), userSigningKeyID.String(), publicKey)
	}
	if err != nil {
		return signatureUploadFailure(errCodeInvalidSignature, fmt.Sprintf("Unable to verify signature: %s", err))
	}
	return nil
}
