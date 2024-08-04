package federation

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"time"

	"github.com/beeper/babbleserv/internal/util"
)

type pubKey struct {
	Key string `json:"key"`
}

type expiredPubKey struct {
	pubKey
	Expired int64 `json:"expired_ts"`
}

func (f *FederationRoutes) GetKeys(w http.ResponseWriter, r *http.Request) {
	activeKeys := make(map[string]pubKey, len(f.config.SigningKeys))
	oldKeys := make(map[string]expiredPubKey, 0)

	for keyID, key := range f.config.SigningKeys {
		respKeyID := keyID
		publicKey := f.config.MustGetSigningKey(keyID).Public().(ed25519.PublicKey)
		pkey := pubKey{util.Base64Encode(publicKey)}
		if key.ExpiredTimestamp == 0 {
			activeKeys[respKeyID] = pkey
		} else {
			oldKeys[respKeyID] = expiredPubKey{pkey, key.ExpiredTimestamp}
		}
	}

	validUntil := time.Now().UTC().Add(f.config.SigningKeyRefreshInterval).UnixMilli()

	resp, err := json.Marshal(struct {
		ServerName    string                   `json:"server_name"`
		VerifyKeys    map[string]pubKey        `json:"verify_keys"`
		OldVerifyKeys map[string]expiredPubKey `json:"old_verify_keys"`
		ValidUntil    int64                    `json:"valid_until_ts"`
	}{f.config.ServerName, activeKeys, oldKeys, validUntil})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	keyID, key := f.config.MustGetActiveSigningKey()
	resp, err = util.SignJSON(resp, f.config.ServerName, keyID, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseRawJSON(w, r, http.StatusOK, resp)
}

func (f *FederationRoutes) QueryKeys(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

func (f *FederationRoutes) QueryKey(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
