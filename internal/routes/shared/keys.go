package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

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

// UserKeys is one user's keys before filtering to a request. Signatures other than the owner's are
// dropped when added to a response.
type UserKeys struct {
	Devices     map[id.DeviceID]UserDeviceKeys
	Master      *mautrix.CrossSigningKeys
	SelfSigning *mautrix.CrossSigningKeys
	// Only ever set for the requester's own local keys
	UserSigning *mautrix.CrossSigningKeys
}

type UserDeviceKeys struct {
	Keys        mautrix.DeviceKeys
	DisplayName string
}

// GetUserKeys answers a federation key query: no requester, so no signature overlay or user-signing key
func GetUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
) (mautrix.RespQueryKeys, error) {
	keys, _, err := CollectUserKeys(ctx, config, db, req, "")
	if err != nil {
		return mautrix.RespQueryKeys{}, err
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
		userKeys, err := UserKeysFromRemoteCache(cache)
		if err != nil {
			return nil, nil, err
		}
		keys[userID] = userKeys
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

	keys := UserKeys{
		Devices:     make(map[id.DeviceID]UserDeviceKeys, len(snapshot.Devices)),
		Master:      snapshot.MasterKey,
		SelfSigning: snapshot.SelfSigningKey,
	}
	for _, device := range snapshot.Devices {
		if device.Keys != nil {
			keys.Devices[device.Device.ID] = UserDeviceKeys{Keys: *device.Keys, DisplayName: device.Device.DisplayName}
		}
	}

	if userID == requestUserID {
		crossSigningKeys, err := db.Accounts.GetUserCrossSigningKeys(ctx, userID, requestUserID)
		if err != nil {
			return UserKeys{}, err
		} else if crossSigningKeys != nil && crossSigningKeys.UserSigning.KeyID() != "" {
			keys.UserSigning = &crossSigningKeys.UserSigning.CrossSigningKeys
		}
	}

	return keys, nil
}

func UserKeysFromRemoteCache(cache *types.RemoteDeviceCache) (UserKeys, error) {
	keys := UserKeys{Devices: make(map[id.DeviceID]UserDeviceKeys, len(cache.Devices))}
	for deviceID, device := range cache.Devices {
		// A device without E2E keys is valid device-list data but has nothing to return
		if len(device.Keys) == 0 {
			continue
		}
		var deviceKeys mautrix.DeviceKeys
		if err := json.Unmarshal(device.Keys, &deviceKeys); err != nil {
			return UserKeys{}, fmt.Errorf("cached device keys for %s %s: %w", cache.UserID, deviceID, err)
		}
		keys.Devices[deviceID] = UserDeviceKeys{Keys: deviceKeys, DisplayName: device.DisplayName}
	}

	var err error
	if keys.Master, err = unmarshalCachedCrossSigningKey(cache.MasterKey); err != nil {
		return UserKeys{}, fmt.Errorf("cached master key for %s: %w", cache.UserID, err)
	}
	if keys.SelfSigning, err = unmarshalCachedCrossSigningKey(cache.SelfSigningKey); err != nil {
		return UserKeys{}, fmt.Errorf("cached self-signing key for %s: %w", cache.UserID, err)
	}
	return keys, nil
}

func unmarshalCachedCrossSigningKey(raw json.RawMessage) (*mautrix.CrossSigningKeys, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var key mautrix.CrossSigningKeys
	if err := json.Unmarshal(raw, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// UserKeysFromQueryResponse takes the user's keys from a live federation query. A remote user-signing
// key is private to its owner and never taken.
func UserKeysFromQueryResponse(userID id.UserID, res mautrix.RespQueryKeys) UserKeys {
	devices := res.DeviceKeys[userID]
	keys := UserKeys{Devices: make(map[id.DeviceID]UserDeviceKeys, len(devices))}
	for deviceID, deviceKeys := range devices {
		if deviceKeys.UserID != userID || deviceKeys.DeviceID != deviceID {
			continue
		}
		displayName, _ := deviceKeys.Unsigned["device_display_name"].(string)
		keys.Devices[deviceID] = UserDeviceKeys{
			Keys:        deviceKeys,
			DisplayName: util.BoundRemoteDeviceDisplayName(displayName),
		}
	}
	if master, ok := res.MasterKeys[userID]; ok && master.UserID == userID {
		keys.Master = &master
	}
	if selfSigning, ok := res.SelfSigningKeys[userID]; ok && selfSigning.UserID == userID {
		keys.SelfSigning = &selfSigning
	}
	return keys
}

// BuildUserKeysResponse filters each user's keys to the request and overlays the requester's own
// signatures of them, read in one transaction
func BuildUserKeysResponse(
	ctx context.Context,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
	keys map[id.UserID]UserKeys,
	requestUserID id.UserID,
) (mautrix.RespQueryKeys, error) {
	var overlay map[id.UserID]map[id.KeyID]map[id.KeyID]string
	if targets := signatureOverlayTargets(req, keys, requestUserID); len(targets) > 0 {
		var err error
		if overlay, err = db.Accounts.GetKeySignaturesBySigner(ctx, requestUserID, targets); err != nil {
			return mautrix.RespQueryKeys{}, err
		}
	}

	resp := newQueryKeysResponse(len(keys))
	for userID, userKeys := range keys {
		AddUserKeys(&resp, userID, req[userID], userKeys, requestUserID, overlay[userID])
	}
	return resp, nil
}

func newQueryKeysResponse(users int) mautrix.RespQueryKeys {
	return mautrix.RespQueryKeys{
		DeviceKeys:      make(map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys, users),
		MasterKeys:      make(map[id.UserID]mautrix.CrossSigningKeys, users),
		SelfSigningKeys: make(map[id.UserID]mautrix.CrossSigningKeys, users),
		UserSigningKeys: make(map[id.UserID]mautrix.CrossSigningKeys, 1),
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
		for _, key := range []*mautrix.CrossSigningKeys{userKeys.Master, userKeys.SelfSigning} {
			if key != nil && key.FirstKey() != "" {
				keyIDs[id.KeyID(key.FirstKey())] = struct{}{}
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
// requester themselves.
func AddUserKeys(
	resp *mautrix.RespQueryKeys,
	userID id.UserID,
	deviceIDs mautrix.DeviceIDList,
	keys UserKeys,
	requestUserID id.UserID,
	overlay map[id.KeyID]map[id.KeyID]string,
) {
	devices := resp.DeviceKeys[userID]
	if devices == nil {
		devices = make(map[id.DeviceID]mautrix.DeviceKeys, len(keys.Devices))
		resp.DeviceKeys[userID] = devices
	}
	for deviceID, device := range requestedDevices(keys.Devices, deviceIDs) {
		deviceKeys := device.Keys
		deviceKeys.Signatures = permittedSignatures(deviceKeys.Signatures, userID, requestUserID, overlay[id.KeyID(deviceID)])
		deviceKeys.Unsigned = nil
		if device.DisplayName != "" {
			deviceKeys.Unsigned = map[string]any{"device_display_name": device.DisplayName}
		}
		devices[deviceID] = deviceKeys
	}

	if keys.Master != nil {
		resp.MasterKeys[userID] = permittedCrossSigningKey(*keys.Master, userID, requestUserID, overlay)
	}
	if keys.SelfSigning != nil {
		resp.SelfSigningKeys[userID] = permittedCrossSigningKey(*keys.SelfSigning, userID, requestUserID, overlay)
	}
	if keys.UserSigning != nil && userID == requestUserID {
		resp.UserSigningKeys[userID] = permittedCrossSigningKey(*keys.UserSigning, userID, requestUserID, overlay)
	}
}

func requestedDevices(devices map[id.DeviceID]UserDeviceKeys, deviceIDs mautrix.DeviceIDList) map[id.DeviceID]UserDeviceKeys {
	if len(deviceIDs) == 0 {
		return devices
	}
	requested := make(map[id.DeviceID]UserDeviceKeys, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		if device, ok := devices[deviceID]; ok {
			requested[deviceID] = device
		}
	}
	return requested
}

func permittedCrossSigningKey(
	key mautrix.CrossSigningKeys,
	userID id.UserID,
	requestUserID id.UserID,
	overlay map[id.KeyID]map[id.KeyID]string,
) mautrix.CrossSigningKeys {
	key.Signatures = permittedSignatures(key.Signatures, userID, requestUserID, overlay[id.KeyID(key.FirstKey())])
	return key
}

// Only the owner's signatures are public. Others' signatures are private to their signer, so the
// requester only sees their own, from the overlay.
func permittedSignatures(
	sigs signatures.Signatures,
	userID id.UserID,
	requestUserID id.UserID,
	overlay map[id.KeyID]string,
) signatures.Signatures {
	permitted := make(signatures.Signatures, 2)
	if owner := sigs[userID]; len(owner) > 0 {
		permitted[userID] = maps.Clone(owner)
	}
	if requestUserID != userID && len(overlay) > 0 {
		permitted[requestUserID] = maps.Clone(overlay)
	}
	return permitted
}

const errCodeInvalidSignature = "M_INVALID_SIGNATURE"

func signatureUploadFailure(errCode string, message string) *mautrix.RespError {
	return &mautrix.RespError{ErrCode: errCode, Err: message}
}

// RemoteSignatureTargetFailure checks a signature upload for another server's user: only their
// cached master key can be signed, by the requester's user-signing key. An uncached key is never
// fetched live. Signatures by other users are ignored, they are never stored.
func RemoteSignatureTargetFailure(
	requestUserID id.UserID,
	userSigningKey *mautrix.CrossSigningKeys,
	cache *types.RemoteDeviceCache,
	targetKey string,
	target mautrix.ReqKeysSignatures,
) *mautrix.RespError {
	if target.DeviceID != "" {
		return signatureUploadFailure(mautrix.MInvalidParam.ErrCode, "Cannot sign another server's user's device")
	}

	var master *mautrix.CrossSigningKeys
	if cache != nil {
		master, _ = unmarshalCachedCrossSigningKey(cache.MasterKey)
	}
	if master == nil {
		return signatureUploadFailure(mautrix.MNotFound.ErrCode, "Target user's master key is not known")
	} else if targetKey != master.FirstKey().String() {
		return signatureUploadFailure(mautrix.MNotFound.ErrCode, "Target key is not the user's master key")
	} else if !util.CompareSignedObjectsJSON(target, master) {
		return signatureUploadFailure(mautrix.MInvalidParam.ErrCode, "Input master key does not match stored")
	}

	requesterSignatures := target.Signatures[requestUserID]
	if userSigningKey == nil || len(requesterSignatures) == 0 {
		return signatureUploadFailure(errCodeInvalidSignature, "Missing signature by the user-signing key")
	}
	userSigningKeyID := id.NewKeyID(id.KeyAlgorithmEd25519, userSigningKey.FirstKey().String())
	for keyID := range requesterSignatures {
		if keyID != userSigningKeyID {
			return signatureUploadFailure(errCodeInvalidSignature, "Signature is not by the user-signing key")
		}
	}
	verifyKeys := map[id.KeyID]id.Ed25519{userSigningKeyID: userSigningKey.FirstKey()}
	if err := util.VerifyObjectWithEd25519Keys(requestUserID, verifyKeys, target); err != nil {
		return signatureUploadFailure(errCodeInvalidSignature, fmt.Sprintf("Unable to verify signature: %s", err))
	}
	return nil
}
