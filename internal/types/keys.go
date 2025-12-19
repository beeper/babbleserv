package types

import (
	"maunium.net/go/mautrix"
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
