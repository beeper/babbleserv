package types

import (
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"
)

type CrossSigningKey struct {
	mautrix.CrossSigningKeys `json:",inline"`
	Unsigned                 map[string]any `json:"unsigned,omitempty"`
}

func (csk CrossSigningKey) KeyID() id.KeyID {
	for _, kid := range csk.Keys {
		return id.KeyID(kid)
	}
	return ""
}

type UserCrossSigningKeys struct {
	Master      CrossSigningKey `json:"master_key"`
	SelfSigning CrossSigningKey `json:"self_signing_key"`
	UserSigning CrossSigningKey `json:"user_signing_key"`
}

type KeySignatureTarget struct {
	UserID id.UserID
	// The signature index key: a device ID, or a cross-signing public key (CrossSigningKey.KeyID())
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
	Key   mautrix.OneTimeKey
	KeyID id.KeyID
	Used  bool
}

type ReqUploadKeys struct {
	mautrix.ReqUploadKeys `json:",inline"`
	// TODO: mautrix field missing?
	FallbackKeys map[id.KeyID]mautrix.OneTimeKey `json:"fallback_keys"`
}
