package util

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Ingest compares stored keys byte for byte, so they are kept as canonical JSON without unsigned
func canonicalRemoteKeyObject[T any](key types.SignedJSON[T]) (types.SignedJSON[T], error) {
	if len(key.Raw) > types.MaxRemoteKeyObjectBytes {
		return types.SignedJSON[T]{}, fmt.Errorf("key object exceeds %d bytes", types.MaxRemoteKeyObjectBytes)
	}
	return key.Canonical()
}

func validateRemoteDeviceKeys(userID id.UserID, deviceID id.DeviceID, keys types.SignedDeviceKeys) (types.SignedDeviceKeys, error) {
	if keys.View.UserID != userID || keys.View.DeviceID != deviceID {
		return types.SignedDeviceKeys{}, fmt.Errorf("device keys for %s %s listed under %s %s", keys.View.UserID, keys.View.DeviceID, userID, deviceID)
	}
	return canonicalRemoteKeyObject(keys)
}

// CheckCrossSigningKey checks a cross-signing key belongs to the user, has the usage and holds one
// ed25519 key, named by itself
func CheckCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, key types.SignedCrossSigningKey) error {
	if key.View.UserID != userID {
		return fmt.Errorf("cross-signing key user ID %q does not match %q", key.View.UserID, userID)
	} else if !slices.Contains(key.View.Usage, usage) {
		return fmt.Errorf("cross-signing key usage %v does not include %q", key.View.Usage, usage)
	} else if len(key.View.Keys) != 1 {
		return fmt.Errorf("cross-signing key has %d keys, expected 1", len(key.View.Keys))
	}
	for keyID, publicKey := range key.View.Keys {
		if keyID != id.NewKeyID(id.KeyAlgorithmEd25519, publicKey.String()) {
			return fmt.Errorf("cross-signing key ID %q does not match its key", keyID)
		} else if _, err := decodeEd25519PublicKey(publicKey.String()); err != nil {
			return fmt.Errorf("cross-signing key %q: %w", keyID, err)
		}
	}
	return nil
}

func decodeEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := Base64Decode(encoded)
	if err != nil {
		return nil, err
	} else if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key is %d bytes", len(key))
	}
	return key, nil
}

func parseRemoteDevice(userID id.UserID, deviceID id.DeviceID, displayName string, keys types.SignedDeviceKeys) (types.RemoteDevice, error) {
	if len(displayName) > types.MaxRemoteDeviceDisplayNameBytes {
		displayName = ""
	}
	device := types.RemoteDevice{DisplayName: displayName}
	if keys.IsZero() {
		return device, nil
	}
	var err error
	device.Keys, err = validateRemoteDeviceKeys(userID, deviceID, keys)
	return device, err
}

// Errors only when the EDU cannot be attributed to a user; payload problems set Invalid
func ParseDeviceListUpdateEDU(content json.RawMessage) (id.UserID, types.RemoteDeviceListUpdate, error) {
	userID, err := parseRemoteEDUUserID(content)
	if err != nil {
		return "", types.RemoteDeviceListUpdate{}, err
	}

	var edu types.DeviceListUpdateEDUContent
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteDeviceListUpdate{Invalid: true}, nil
	}
	// Stream IDs start at 1. Without prev IDs a missing one resets the cache and notifies no one.
	if edu.StreamID == 0 {
		return userID, types.RemoteDeviceListUpdate{DeviceID: edu.DeviceID, Invalid: true}, nil
	}
	update := types.RemoteDeviceListUpdate{
		DeviceID: edu.DeviceID,
		StreamID: edu.StreamID,
		PrevIDs:  edu.PrevID,
		Deleted:  edu.Deleted,
	}
	if !edu.Deleted {
		update.Device, err = parseRemoteDevice(userID, edu.DeviceID, edu.DeviceDisplayName, edu.DeviceKeys)
		update.Invalid = err != nil
	}
	return userID, update, nil
}

func ParseSigningKeyUpdateEDU(content json.RawMessage) (id.UserID, types.RemoteSigningKeyUpdate, error) {
	userID, err := parseRemoteEDUUserID(content)
	if err != nil {
		return "", types.RemoteSigningKeyUpdate{}, err
	}

	var edu types.SigningKeyUpdateEDUContent
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteSigningKeyUpdate{Invalid: true}, nil
	}
	masterKey, masterValid := validateOptionalCrossSigningKey(userID, id.XSUsageMaster, edu.MasterKey)
	selfSigningKey, selfSigningValid := validateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, edu.SelfSigning)
	return userID, types.RemoteSigningKeyUpdate{
		MasterKey:      masterKey,
		SelfSigningKey: selfSigningKey,
		Invalid:        !masterValid || !selfSigningValid,
	}, nil
}

// An error is a failed fetch, never an empty device list. Invalid devices and keys are dropped.
func ParseRemoteDeviceSnapshot(userID id.UserID, body []byte) (*types.RemoteDeviceSnapshot, error) {
	if len(body) > types.MaxRemoteDeviceSnapshotBytes {
		return nil, fmt.Errorf("device snapshot exceeds %d bytes", types.MaxRemoteDeviceSnapshotBytes)
	}
	// Raw so one bad device or key is dropped on its own rather than failing the fetch
	var response struct {
		types.RespUserDevices
		Devices        []json.RawMessage `json:"devices"`
		MasterKey      json.RawMessage   `json:"master_key"`
		SelfSigningKey json.RawMessage   `json:"self_signing_key"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid device snapshot: %w", err)
	} else if response.UserID != userID {
		return nil, fmt.Errorf("device snapshot user ID %q does not match %q", response.UserID, userID)
	} else if len(response.Devices) > types.MaxRemoteDevicesPerUser {
		return nil, fmt.Errorf("device snapshot has %d devices, limit is %d", len(response.Devices), types.MaxRemoteDevicesPerUser)
	}

	snapshot := &types.RemoteDeviceSnapshot{
		StreamID: response.StreamID,
		Devices:  make(map[id.DeviceID]types.RemoteDevice, len(response.Devices)),
	}
	for _, raw := range response.Devices {
		var device types.RespUserDevice
		// The ID is part of the device's storage key
		if json.Unmarshal(raw, &device) != nil || len(device.ID) > types.MaxRemoteDeviceIDBytes {
			continue
		}
		if parsed, err := parseRemoteDevice(userID, device.ID, device.DisplayName, device.Keys); err == nil {
			snapshot.Devices[device.ID] = parsed
		}
	}
	snapshot.MasterKey, _ = ValidateOptionalCrossSigningKey(userID, id.XSUsageMaster, response.MasterKey)
	snapshot.SelfSigningKey, _ = ValidateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, response.SelfSigningKey)
	return snapshot, nil
}

// A queried device is only listed with its keys, which carry its display name in their unsigned
func ParseRemoteQueriedDevice(userID id.UserID, deviceID id.DeviceID, raw json.RawMessage) (types.RemoteDevice, bool) {
	keys, err := types.ParseSignedJSON[mautrix.DeviceKeys](raw)
	if len(deviceID) > types.MaxRemoteDeviceIDBytes || err != nil {
		return types.RemoteDevice{}, false
	}
	displayName, _ := keys.View.Unsigned["device_display_name"].(string)
	device, err := parseRemoteDevice(userID, deviceID, displayName, keys)
	return device, err == nil && !device.Keys.IsZero()
}

func ValidateOptionalCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, raw json.RawMessage) (types.SignedCrossSigningKey, bool) {
	key, err := types.ParseSignedJSON[mautrix.CrossSigningKeys](raw)
	if err != nil {
		return types.SignedCrossSigningKey{}, false
	}
	return validateOptionalCrossSigningKey(userID, usage, key)
}

func validateOptionalCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, key types.SignedCrossSigningKey) (types.SignedCrossSigningKey, bool) {
	if key.IsZero() {
		return key, true
	} else if CheckCrossSigningKey(userID, usage, key) != nil {
		return types.SignedCrossSigningKey{}, false
	}
	key, err := canonicalRemoteKeyObject(key)
	return key, err == nil
}

func parseRemoteEDUUserID(content json.RawMessage) (id.UserID, error) {
	var edu struct {
		UserID id.UserID `json:"user_id"`
	}
	if err := json.Unmarshal(content, &edu); err != nil {
		return "", err
	} else if _, _, err := edu.UserID.ParseAndValidateRelaxed(); err != nil {
		return "", fmt.Errorf("invalid EDU user ID %q: %w", edu.UserID, err)
	}
	return edu.UserID, nil
}
