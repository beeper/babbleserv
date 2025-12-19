package main

import (
	"context"

	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func getOlmMachine() *crypto.OlmMachine {
	olm := crypto.NewOlmMachine(nil, &log.Logger, &crypto.MemoryStore{}, nil)
	if err := olm.Load(context.TODO()); err != nil {
		panic(err)
	}
	return olm
}

func GetDeviceKeys(userID id.UserID, deviceID id.DeviceID) types.ReqUploadKeys {
	olm := getOlmMachine()

	account := olm.GetAccount()

	deviceKeys := &mautrix.DeviceKeys{
		UserID:     userID,
		DeviceID:   deviceID,
		Algorithms: []id.Algorithm{id.AlgorithmMegolmV1, id.AlgorithmOlmV1},
		Keys: map[id.DeviceKeyID]string{
			id.NewDeviceKeyID(id.KeyAlgorithmCurve25519, deviceID): string(account.IdentityKey()),
			id.NewDeviceKeyID(id.KeyAlgorithmEd25519, deviceID):    string(account.SigningKey()),
		},
	}

	signature, err := account.SignJSON(deviceKeys)
	if err != nil {
		panic(err)
	}

	deviceKeys.Signatures = signatures.NewSingleSignature(userID, id.KeyAlgorithmEd25519, deviceID.String(), signature)

	account.Internal.GenOneTimeKeys(11)
	rawOtks, err := account.Internal.OneTimeKeys()
	if err != nil {
		panic(err)
	}

	otks := make(map[id.KeyID]mautrix.OneTimeKey)
	fbkey := make(map[id.KeyID]mautrix.OneTimeKey)
	for name, key := range rawOtks {
		otk := mautrix.OneTimeKey{
			Key: id.Curve25519(key),
		}
		if len(fbkey) == 0 {
			otk.Fallback = true
		}
		signature, err := account.SignJSON(otk)
		if err != nil {
			panic(err)
		}
		otk.Signatures = signatures.NewSingleSignature(userID, id.KeyAlgorithmEd25519, deviceID.String(), signature)
		keyID := id.NewKeyID(id.KeyAlgorithmSignedCurve25519, name)
		if len(fbkey) == 0 {
			fbkey[keyID] = otk
		} else {
			otks[keyID] = otk
		}
	}

	return types.ReqUploadKeys{
		ReqUploadKeys: mautrix.ReqUploadKeys{
			DeviceKeys:  deviceKeys,
			OneTimeKeys: otks,
		},
		FallbackKeys: fbkey,
	}
}

func GetCrossSigningKeys(userID id.UserID, deviceID id.DeviceID) mautrix.UploadCrossSigningKeysReq {
	olm := getOlmMachine()

	keys, err := olm.GenerateCrossSigningKeys()
	if err != nil {
		panic(err)
	}

	masterKeyID := id.NewKeyID(id.KeyAlgorithmEd25519, keys.MasterKey.PublicKey().String())
	masterKey := mautrix.CrossSigningKeys{
		UserID: userID,
		Usage:  []id.CrossSigningUsage{id.XSUsageMaster},
		Keys: map[id.KeyID]id.Ed25519{
			masterKeyID: keys.MasterKey.PublicKey(),
		},
	}
	// account := olm.GetAccount()
	masterSig, err := keys.MasterKey.SignJSON(masterKey)
	// masterSig, err := account.SignJSON(masterKey)
	if err != nil {
		panic(err)
	}
	masterKey.Signatures = signatures.NewSingleSignature(userID, id.KeyAlgorithmEd25519, keys.MasterKey.PublicKey().String(), masterSig)

	selfKey := mautrix.CrossSigningKeys{
		UserID: userID,
		Usage:  []id.CrossSigningUsage{id.XSUsageSelfSigning},
		Keys: map[id.KeyID]id.Ed25519{
			id.NewKeyID(id.KeyAlgorithmEd25519, keys.SelfSigningKey.PublicKey().String()): keys.SelfSigningKey.PublicKey(),
		},
	}
	selfSig, err := keys.MasterKey.SignJSON(selfKey)
	if err != nil {
		panic(err)
	}
	selfKey.Signatures = signatures.NewSingleSignature(userID, id.KeyAlgorithmEd25519, keys.MasterKey.PublicKey().String(), selfSig)

	userKey := mautrix.CrossSigningKeys{
		UserID: userID,
		Usage:  []id.CrossSigningUsage{id.XSUsageUserSigning},
		Keys: map[id.KeyID]id.Ed25519{
			id.NewKeyID(id.KeyAlgorithmEd25519, keys.UserSigningKey.PublicKey().String()): keys.UserSigningKey.PublicKey(),
		},
	}
	userSig, err := keys.MasterKey.SignJSON(userKey)
	if err != nil {
		panic(err)
	}
	userKey.Signatures = signatures.NewSingleSignature(userID, id.KeyAlgorithmEd25519, keys.MasterKey.PublicKey().String(), userSig)

	secondUserSig, err := keys.SelfSigningKey.SignJSON(userKey)
	if err != nil {
		panic(err)
	}
	userKey.Signatures[userID][id.KeyID("ed25519:"+keys.SelfSigningKey.PublicKey().String())] = secondUserSig

	return mautrix.UploadCrossSigningKeysReq{
		Master:      masterKey,
		SelfSigning: selfKey,
		UserSigning: userKey,
	}
}
