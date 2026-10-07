package util

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// The cache stores and compares signed objects as canonical JSON without unsigned, so the owner's
// signatures are part of the content. Duplicate names are rejected because gjson and encoding/json
// would resolve them differently.
func canonicalSignedJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !jsontext.Value(raw).IsValid() {
		return nil, errors.New("signed object is not valid JSON or has duplicate names")
	} else if !gjson.ParseBytes(raw).IsObject() {
		return nil, errors.New("signed object is not a JSON object")
	}
	return canonicalJSONWithout(raw, "unsigned")
}

// The self-signature is verified over the canonical bytes as received rather than a re-marshalled
// struct, so unknown signed members stay covered and a malformed foreign signature cannot reject
// the device. Unsigned keys are accepted as local uploads accept them: the spec asks clients, not
// servers, to verify them and Synapse never does.
func validateRemoteDeviceKeys(userID id.UserID, deviceID id.DeviceID, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicalRemoteKeyObject(userID, raw)
	if err != nil {
		return nil, err
	}
	var keys mautrix.DeviceKeys
	if err := json.Unmarshal(canonical, &keys); err != nil {
		return nil, err
	} else if keys.UserID != userID {
		return nil, fmt.Errorf("device keys user ID %q does not match %q", keys.UserID, userID)
	} else if keys.DeviceID != deviceID {
		return nil, fmt.Errorf("device keys device ID %q does not match %q", keys.DeviceID, deviceID)
	} else if err := checkOnlyOwnerSignatures(userID, keys.Signatures); err != nil {
		return nil, err
	}

	keyID := id.NewDeviceKeyID(id.KeyAlgorithmEd25519, deviceID)
	publicKey, err := decodeEd25519PublicKey(keys.Keys[keyID])
	if err != nil {
		return nil, fmt.Errorf("device key %q: %w", keyID, err)
	} else if len(keys.Signatures) == 0 {
		return canonical, nil
	} else if err := VerifyJSON(canonical, userID.String(), keyID.String(), publicKey); err != nil {
		return nil, err
	}
	return canonical, nil
}

// No signature is required or verified on cross-signing keys
func validateRemoteCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicalRemoteKeyObject(userID, raw)
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
	} else if err := checkOnlyOwnerSignatures(userID, key.Signatures); err != nil {
		return nil, err
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

// Keeps the owner's signatures and drops everyone else's; the result is canonical
func canonicalRemoteKeyObject(userID id.UserID, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > types.MaxRemoteKeyObjectBytes {
		return nil, fmt.Errorf("key object exceeds %d bytes", types.MaxRemoteKeyObjectBytes)
	}
	canonical, err := canonicalSignedJSON(raw)
	if err != nil {
		return nil, err
	}

	signatures := gjson.GetBytes(canonical, "signatures")
	if !signatures.Exists() {
		return canonical, nil
	}
	var signers map[id.UserID]json.RawMessage
	if err := json.Unmarshal([]byte(signatures.Raw), &signers); err != nil {
		return nil, fmt.Errorf("key object signatures: %w", err)
	}
	if owner, signed := signers[userID]; signed {
		canonical, err = sjson.SetRawBytes(canonical, "signatures", exerrors.Must(json.Marshal(map[id.UserID]json.RawMessage{userID: owner})))
	} else {
		canonical, err = sjson.DeleteBytes(canonical, "signatures")
	}
	if err != nil {
		return nil, err
	}
	return canonicalSignedJSON(canonical)
}

// A case-variant Signatures member survives the filtering above but encoding/json merges it into
// the decoded map, which is what clients would be served
func checkOnlyOwnerSignatures(userID id.UserID, signatures map[id.UserID]map[id.KeyID]string) error {
	for signer := range signatures {
		if signer != userID {
			return fmt.Errorf("key object carries signatures from %q", signer)
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

func BoundRemoteDeviceDisplayName(name string) string {
	if len(name) > types.MaxRemoteDeviceDisplayNameBytes || !utf8.ValidString(name) {
		return ""
	}
	return name
}

// Checked as raw JSON because decoding replaces invalid UTF-8 with U+FFFD. Any other unusable value
// is cosmetic and bounds to "" rather than invalidating the device.
func remoteDeviceDisplayName(raw json.RawMessage) string {
	var name string
	if !jsontext.Value(raw).IsValid() || json.Unmarshal(raw, &name) != nil {
		return ""
	}
	return BoundRemoteDeviceDisplayName(name)
}

// An over-long device ID exceeds the storage key limit even when only read
func boundRemoteDeviceID(deviceID id.DeviceID) (id.DeviceID, bool) {
	if deviceID == "" || len(deviceID) > types.MaxRemoteDeviceIDBytes {
		return "", false
	}
	return deviceID, true
}

type remoteDeviceListUpdateEDU struct {
	DeviceID          id.DeviceID     `json:"device_id"`
	StreamID          *int64          `json:"stream_id"`
	PrevIDs           []int64         `json:"prev_id"`
	Deleted           bool            `json:"deleted"`
	DeviceDisplayName json.RawMessage `json:"device_display_name"`
	Keys              json.RawMessage `json:"keys"`
}

// Errors only when the EDU cannot be attributed to a user; payload problems set Invalid
func ParseDeviceListUpdateEDU(content json.RawMessage) (id.UserID, types.RemoteDeviceListUpdate, error) {
	userID, err := parseRemoteEDUUserID(content)
	if err != nil {
		return "", types.RemoteDeviceListUpdate{}, err
	}

	// Without a usable stream ID the update cannot be recognised as a replay, so it carries no prev
	// IDs and is handled as a reset
	var edu remoteDeviceListUpdateEDU
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteDeviceListUpdate{Invalid: true}, nil
	}
	deviceID, deviceIDValid := boundRemoteDeviceID(edu.DeviceID)
	if edu.StreamID == nil {
		return userID, types.RemoteDeviceListUpdate{DeviceID: deviceID, Invalid: true}, nil
	}

	update := types.RemoteDeviceListUpdate{
		DeviceID: deviceID,
		StreamID: *edu.StreamID,
		PrevIDs:  edu.PrevIDs,
		Deleted:  edu.Deleted,
	}
	if !deviceIDValid {
		update.Invalid = true
	} else if !edu.Deleted {
		update.Device.DisplayName = remoteDeviceDisplayName(edu.DeviceDisplayName)
		if !isAbsentJSON(edu.Keys) {
			update.Device.Keys, err = validateRemoteDeviceKeys(userID, deviceID, edu.Keys)
			update.Invalid = err != nil
		}
	}
	return userID, update, nil
}

func ParseSigningKeyUpdateEDU(content json.RawMessage) (id.UserID, types.RemoteSigningKeyUpdate, error) {
	userID, err := parseRemoteEDUUserID(content)
	if err != nil {
		return "", types.RemoteSigningKeyUpdate{}, err
	}

	var edu struct {
		MasterKey      json.RawMessage `json:"master_key"`
		SelfSigningKey json.RawMessage `json:"self_signing_key"`
	}
	if err := json.Unmarshal(content, &edu); err != nil {
		return userID, types.RemoteSigningKeyUpdate{Invalid: true}, nil
	}

	masterKey, masterValid := validateOptionalCrossSigningKey(userID, id.XSUsageMaster, edu.MasterKey)
	selfSigningKey, selfSigningValid := validateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, edu.SelfSigningKey)
	return userID, types.RemoteSigningKeyUpdate{
		MasterKey:      masterKey,
		SelfSigningKey: selfSigningKey,
		Invalid:        !masterValid || !selfSigningValid,
	}, nil
}

type remoteDeviceSnapshotResponse struct {
	UserID         id.UserID       `json:"user_id"`
	StreamID       *int64          `json:"stream_id"`
	Devices        json.RawMessage `json:"devices"`
	MasterKey      json.RawMessage `json:"master_key"`
	SelfSigningKey json.RawMessage `json:"self_signing_key"`
}

type remoteSnapshotDevice struct {
	DeviceID          id.DeviceID     `json:"device_id"`
	DeviceDisplayName json.RawMessage `json:"device_display_name"`
	Keys              json.RawMessage `json:"keys"`
}

// An error is a failed fetch, never an empty device list
func ParseRemoteDeviceSnapshot(userID id.UserID, body []byte) (*types.RemoteDeviceSnapshot, error) {
	if len(body) > types.MaxRemoteDeviceSnapshotBytes {
		return nil, fmt.Errorf("device snapshot exceeds %d bytes", types.MaxRemoteDeviceSnapshotBytes)
	}
	var response remoteDeviceSnapshotResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid device snapshot: %w", err)
	} else if response.UserID != userID {
		return nil, fmt.Errorf("device snapshot user ID %q does not match %q", response.UserID, userID)
	} else if response.StreamID == nil {
		return nil, errors.New("device snapshot has no stream ID")
	}

	// Counted before decoding so an oversized list is rejected without allocating per entry
	devices := gjson.ParseBytes(response.Devices)
	if !devices.IsArray() {
		return nil, errors.New("device snapshot devices is not an array")
	} else if count := devices.Get("#").Int(); count > types.MaxRemoteDevicesPerUser {
		return nil, fmt.Errorf("device snapshot has %d devices, limit is %d", count, types.MaxRemoteDevicesPerUser)
	}

	snapshot := &types.RemoteDeviceSnapshot{
		StreamID: *response.StreamID,
		Devices:  make(map[id.DeviceID]types.RemoteDevice),
	}
	for _, entry := range devices.Array() {
		var device remoteSnapshotDevice
		if err := json.Unmarshal([]byte(entry.Raw), &device); err != nil {
			continue
		} else if _, valid := boundRemoteDeviceID(device.DeviceID); !valid {
			continue
		} else if _, duplicate := snapshot.Devices[device.DeviceID]; duplicate {
			continue
		}

		var keys json.RawMessage
		if !isAbsentJSON(device.Keys) {
			var err error
			if keys, err = validateRemoteDeviceKeys(userID, device.DeviceID, device.Keys); err != nil {
				continue
			}
		}
		snapshot.Devices[device.DeviceID] = types.RemoteDevice{
			DisplayName: remoteDeviceDisplayName(device.DeviceDisplayName),
			Keys:        keys,
		}
	}

	snapshot.MasterKey, _ = validateOptionalCrossSigningKey(userID, id.XSUsageMaster, response.MasterKey)
	snapshot.SelfSigningKey, _ = validateOptionalCrossSigningKey(userID, id.XSUsageSelfSigning, response.SelfSigningKey)
	return snapshot, nil
}

func validateOptionalCrossSigningKey(userID id.UserID, usage id.CrossSigningUsage, raw json.RawMessage) (json.RawMessage, bool) {
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
