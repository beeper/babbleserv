package util

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Ingest compares stored keys byte for byte, so they are kept as canonical JSON without unsigned.
// Duplicate names are rejected because gjson and encoding/json would resolve them differently.
func canonicalRemoteKeyObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > types.MaxRemoteKeyObjectBytes {
		return nil, fmt.Errorf("key object exceeds %d bytes", types.MaxRemoteKeyObjectBytes)
	} else if !jsontext.Value(raw).IsValid() {
		return nil, errors.New("key object is not valid JSON or has duplicate names")
	}
	withoutUnsigned, err := sjson.DeleteBytes(raw, "unsigned")
	if err != nil {
		return nil, err
	}
	return gomatrixserverlib.CanonicalJSON(withoutUnsigned)
}

func validateRemoteDeviceKeys(userID id.UserID, deviceID id.DeviceID, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicalRemoteKeyObject(raw)
	if err != nil {
		return nil, err
	}
	var keys mautrix.DeviceKeys
	if err := json.Unmarshal(canonical, &keys); err != nil {
		return nil, err
	} else if keys.UserID != userID || keys.DeviceID != deviceID {
		return nil, fmt.Errorf("device keys for %s %s listed under %s %s", keys.UserID, keys.DeviceID, userID, deviceID)
	}
	return canonical, nil
}

func validateRemoteCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicalRemoteKeyObject(raw)
	if err != nil {
		return nil, err
	}
	var key mautrix.CrossSigningKeys
	if err := json.Unmarshal(canonical, &key); err != nil {
		return nil, err
	} else if key.UserID != userID {
		return nil, fmt.Errorf("cross-signing key user ID %q does not match %q", key.UserID, userID)
	} else if !slices.Contains(key.Usage, usage) {
		return nil, fmt.Errorf("cross-signing key usage %v does not include %q", key.Usage, usage)
	} else if len(key.Keys) != 1 {
		return nil, fmt.Errorf("cross-signing key has %d keys, expected 1", len(key.Keys))
	}

	for keyID, publicKey := range key.Keys {
		if keyID != id.NewKeyID(id.KeyAlgorithmEd25519, publicKey.String()) {
			return nil, fmt.Errorf("cross-signing key ID %q does not match its key", keyID)
		} else if _, err := decodeEd25519PublicKey(publicKey.String()); err != nil {
			return nil, fmt.Errorf("cross-signing key %q: %w", keyID, err)
		}
	}
	return canonical, nil
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

func parseRemoteDevice(userID id.UserID, deviceID id.DeviceID, displayName string, keys json.RawMessage) (types.RemoteDevice, error) {
	if len(displayName) > types.MaxRemoteDeviceDisplayNameBytes {
		displayName = ""
	}
	device := types.RemoteDevice{DisplayName: displayName}
	if isAbsentJSON(keys) {
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

	var edu struct {
		types.DeviceListUpdateEDUContent
		// Raw, as mautrix.DeviceKeys drops signed members it doesn't know
		DeviceKeys json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteDeviceListUpdate{Invalid: true}, nil
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

	var edu struct {
		MasterKey   json.RawMessage `json:"master_key"`
		SelfSigning json.RawMessage `json:"self_signing_key"`
	}
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteSigningKeyUpdate{Invalid: true}, nil
	}
	masterKey, masterValid := ValidateOptionalCrossSigningKey(userID, id.XSUsageMaster, edu.MasterKey)
	selfSigningKey, selfSigningValid := ValidateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, edu.SelfSigning)
	return userID, types.RemoteSigningKeyUpdate{
		MasterKey:      masterKey,
		SelfSigningKey: selfSigningKey,
		Invalid:        !masterValid || !selfSigningValid,
	}, nil
}

// An error is a failed fetch, never an empty device list. Devices with invalid keys are dropped.
func ParseRemoteDeviceSnapshot(userID id.UserID, body []byte) (*types.RemoteDeviceSnapshot, error) {
	if len(body) > types.MaxRemoteDeviceSnapshotBytes {
		return nil, fmt.Errorf("device snapshot exceeds %d bytes", types.MaxRemoteDeviceSnapshotBytes)
	}
	var response struct {
		Devices []struct {
			Keys        json.RawMessage `json:"keys"`
			ID          id.DeviceID     `json:"device_id"`
			DisplayName string          `json:"device_display_name"`
		} `json:"devices"`
		MasterKey      json.RawMessage `json:"master_key"`
		SelfSigningKey json.RawMessage `json:"self_signing_key"`
		StreamID       int64           `json:"stream_id"`
		UserID         id.UserID       `json:"user_id"`
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
	for _, device := range response.Devices {
		// The ID is part of the device's storage key
		if len(device.ID) > types.MaxRemoteDeviceIDBytes {
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
	var keys mautrix.DeviceKeys
	if len(deviceID) > types.MaxRemoteDeviceIDBytes || json.Unmarshal(raw, &keys) != nil {
		return types.RemoteDevice{}, false
	}
	displayName, _ := keys.Unsigned["device_display_name"].(string)
	device, err := parseRemoteDevice(userID, deviceID, displayName, raw)
	return device, err == nil && device.Keys != nil
}

func ValidateOptionalCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, raw json.RawMessage) (json.RawMessage, bool) {
	if isAbsentJSON(raw) {
		return nil, true
	}
	key, err := validateRemoteCrossSigningKey(userID, usage, raw)
	return key, err == nil
}

func isAbsentJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
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
