package users

import (
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (u *UsersDirectory) TxnGetUserCrossSigningKeys(txn fdb.ReadTransaction, userID id.UserID) (*types.UserCrossSigningKeys, error) {
	key := u.userCrossSigningKeys.Pack(tuple.Tuple{userID.String()})
	b, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	} else if b == nil {
		return nil, nil
	}

	var keys types.UserCrossSigningKeys
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}

	return &keys, nil
}

func (u *UsersDirectory) TxnStoreUserCrossSigningKeys(txn fdb.Transaction, userID id.UserID, keys types.UserCrossSigningKeys) {
	b, err := json.Marshal(keys)
	if err != nil {
		panic(err)
	}

	// Signures are stored separately
	b, _ = sjson.DeleteBytes(b, "master_key.signatures")
	b, _ = sjson.DeleteBytes(b, "self_signing_key.signatures")
	b, _ = sjson.DeleteBytes(b, "user_signing_key.signatures")

	key := u.userCrossSigningKeys.Pack(tuple.Tuple{userID.String()})
	txn.Set(key, b)
}

func (u *UsersDirectory) TxnStoreKeySignatures(txn fdb.Transaction, userID id.UserID, keyID id.KeyID, signatures signatures.Signatures) {
	for signingUserID, keyMap := range signatures {
		for signingKeyID, signature := range keyMap {
			u.txnStoreKeySignature(txn, signingUserID, signingKeyID, userID, keyID, []byte(signature))
		}
	}
}

func (u *UsersDirectory) txnStoreKeySignature(
	txn fdb.Transaction,
	signingUserID id.UserID,
	signingKeyID id.KeyID,
	targetUserID id.UserID,
	targetKeyID id.KeyID,
	signature []byte,
) {
	keyTup := tuple.Tuple{
		signingUserID.String(),
		targetUserID.String(),
		targetKeyID.String(),
		// Note our signing key ID goes at the end - we want to fetch by signing user/target
		signingKeyID.String(),
	}
	txn.Set(u.userKeySignatures.Pack(keyTup), signature)
}

// Returns targetUserID -> targetKeyID -> ourSigningKeyID -> signature
func (u *UsersDirectory) TxnGetKeySignatures(
	txn fdb.ReadTransaction,
	userID id.UserID,
	targetUserKeys map[id.UserID]map[id.KeyID]struct{},
) (int, map[id.UserID]map[id.KeyID]map[id.KeyID]string) {
	ranges := make(map[id.UserID]map[id.KeyID]fdb.RangeResult, len(targetUserKeys))
	for targetUserID, keys := range targetUserKeys {
		if _, ok := ranges[targetUserID]; !ok {
			ranges[targetUserID] = make(map[id.KeyID]fdb.RangeResult, len(keys))
		}
		for kid := range keys {
			ranges[targetUserID][kid] = txn.GetRange(
				u.userKeySignatures.Sub(userID.String(), targetUserID.String(), kid.String()),
				fdb.RangeOptions{
					Mode: fdb.StreamingModeWantAll,
				},
			)
		}
	}

	var found int
	signatures := make(map[id.UserID]map[id.KeyID]map[id.KeyID]string, len(ranges))

	for targetUserID, keyResults := range ranges {
		if _, ok := signatures[targetUserID]; !ok {
			signatures[targetUserID] = make(map[id.KeyID]map[id.KeyID]string, len(keyResults))
		}
		for targetKeyID, res := range keyResults {
			matches := res.GetSliceOrPanic()
			if _, ok := signatures[targetUserID][targetKeyID]; !ok {
				signatures[targetUserID][targetKeyID] = make(map[id.KeyID]string, len(matches))
			}
			for _, kv := range matches {
				tup, _ := u.userKeySignatures.Unpack(kv.Key)
				signingKeyID := id.KeyID(tup[3].(string))
				signatures[targetUserID][targetKeyID][signingKeyID] = string(kv.Value)
				found++
			}
		}
	}

	return found, signatures
}
