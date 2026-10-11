package types

import (
	"encoding/json"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"
)

type UserCrossSigningKeys struct {
	Master      SignedCrossSigningKey `json:"master_key,omitzero"`
	SelfSigning SignedCrossSigningKey `json:"self_signing_key,omitzero"`
	UserSigning SignedCrossSigningKey `json:"user_signing_key,omitzero"`
}

// Older rows hold an empty object for each key the user has none of
func (k *UserCrossSigningKeys) UnmarshalJSON(data []byte) error {
	type stored UserCrossSigningKeys
	if err := json.Unmarshal(data, (*stored)(k)); err != nil {
		return err
	}
	for _, key := range []*SignedCrossSigningKey{&k.Master, &k.SelfSigning, &k.UserSigning} {
		if len(key.View.Keys) == 0 {
			*key = SignedCrossSigningKey{}
		}
	}
	return nil
}

// Merge replaces each stored key with its uploaded one, keeping the keys an upload omits
func (k UserCrossSigningKeys) Merge(upload UserCrossSigningKeys) UserCrossSigningKeys {
	if !upload.Master.IsZero() {
		k.Master = upload.Master
	}
	if !upload.SelfSigning.IsZero() {
		k.SelfSigning = upload.SelfSigning
	}
	if !upload.UserSigning.IsZero() {
		k.UserSigning = upload.UserSigning
	}
	return k
}

// RespQueryKeys is mautrix.RespQueryKeys with each signed key object kept as raw JSON, since
// decoding into the mautrix structs drops signed members they don't know and breaks the signatures
type RespQueryKeys struct {
	Failures        map[string]any                                `json:"failures,omitempty"`
	DeviceKeys      map[id.UserID]map[id.DeviceID]json.RawMessage `json:"device_keys"`
	MasterKeys      map[id.UserID]json.RawMessage                 `json:"master_keys"`
	SelfSigningKeys map[id.UserID]json.RawMessage                 `json:"self_signing_keys"`
	UserSigningKeys map[id.UserID]json.RawMessage                 `json:"user_signing_keys"`
}

// RespClaimKeys is mautrix.RespClaimKeys with each key kept as raw JSON, as RespQueryKeys
type RespClaimKeys struct {
	Failures    map[string]any                                             `json:"failures,omitempty"`
	OneTimeKeys map[id.UserID]map[id.DeviceID]map[id.KeyID]json.RawMessage `json:"one_time_keys"`
}

type KeySignatureTarget struct {
	UserID id.UserID
	// The signature index key: a device ID, or a cross-signing public key
	KeyID      id.KeyID
	DeviceID   id.DeviceID // set when the target is a device
	Signatures signatures.Signatures
}

func SignaturesChanged(stored, uploaded map[id.KeyID]string) bool {
	for keyID, signature := range uploaded {
		if existing, ok := stored[keyID]; !ok || existing != signature {
			return true
		}
	}
	return false
}

type FallbackKey struct {
	Key   SignedOneTimeKey
	KeyID id.KeyID
	Used  bool
}

type ReqUploadKeys struct {
	mautrix.ReqUploadKeys `json:",inline"`
	// TODO: mautrix field missing?
	FallbackKeys map[id.KeyID]mautrix.OneTimeKey `json:"fallback_keys"`
}
