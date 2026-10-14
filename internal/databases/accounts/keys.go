package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (a *AccountsDatabase) CountOneTimeKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
) (mautrix.OTKCount, error) {
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (mautrix.OTKCount, error) {
		return a.devices.TxnCountOneTimeKeys(ctx, txn, userID, deviceID), nil
	})
}

func (a *AccountsDatabase) GetFallbackKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
) (map[id.KeyAlgorithm]types.FallbackKey, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (map[id.KeyAlgorithm]types.FallbackKey, error) {
		keys := a.devices.TxnGetFallbackKeys(txn, userID, deviceID)
		return keys, nil
	})
}

// Claim a pre-key for a given user/device, using one time keys if available and fallback keys if
// needed, flagging as used.
func (a *AccountsDatabase) ClaimOrGetPreKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	algorithm id.KeyAlgorithm,
	limit int,
) (map[id.KeyID]json.RawMessage, error) {
	log := zerolog.Ctx(ctx)

	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (map[id.KeyID]json.RawMessage, error) {
		if otks := a.devices.TxnClaimOneTimeKeys(txn, userID, deviceID, algorithm, limit); otks != nil {
			return otks, nil
		}

		fallbackKeys := a.devices.TxnGetFallbackKeys(txn, userID, deviceID)
		if key, ok := fallbackKeys[algorithm]; ok {
			// Flag the key as used
			key.Used = true
			a.devices.TxnStoreFallbackKeys(txn, userID, deviceID, map[id.KeyAlgorithm]types.FallbackKey{
				algorithm: key,
			})
			log.Warn().Msg("No one time key found, returning fallback key")
			return map[id.KeyID]json.RawMessage{
				key.KeyID: key.Key.Raw,
			}, nil
		}

		log.Warn().Msg("No one time or fallback keys found")
		return nil, nil
	})
}

// Changed signatures of the signer's own devices allocate a device-list version, of the signer's own
// master or self-signing key write a `*` record. Signatures of other users' keys are private
// cross-signatures, never federated.
func (a *AccountsDatabase) StoreKeySignatures(
	ctx context.Context,
	requestUserID id.UserID,
	targets []types.KeySignatureTarget,
) error {
	recorded, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		targetKeys := make(map[id.UserID]map[id.KeyID]struct{}, len(targets))
		for _, target := range targets {
			if targetKeys[target.UserID] == nil {
				targetKeys[target.UserID] = make(map[id.KeyID]struct{}, 1)
			}
			targetKeys[target.UserID][target.KeyID] = struct{}{}
		}
		_, stored := a.users.TxnGetKeySignatures(txn, requestUserID, targetKeys)

		var changedDevices []id.DeviceID
		var changedOwnKeys []id.KeyID
		for _, target := range targets {
			uploaded := target.Signatures[requestUserID]
			if !types.SignaturesChanged(stored[target.UserID][target.KeyID], uploaded) {
				continue
			}
			a.users.TxnStoreKeySignatures(txn, target.UserID, target.KeyID, signatures.Signatures{requestUserID: uploaded})

			switch {
			case target.UserID != requestUserID:
				continue
			case target.DeviceID != "":
				changedDevices = append(changedDevices, target.DeviceID)
			default:
				changedOwnKeys = append(changedOwnKeys, target.KeyID)
			}
		}

		if len(changedDevices) >= types.MaxVersionstampUserVersion {
			return false, fmt.Errorf("too many device signatures in one upload: %d", len(changedDevices))
		}
		for index, deviceID := range changedDevices {
			if err := a.txnStoreDeviceListChange(txn, requestUserID, deviceID, uint16(index)); err != nil {
				return false, err
			}
		}

		publicKeyChanged, err := a.txnContainsPublicCrossSigningKey(txn, requestUserID, changedOwnKeys)
		if err != nil {
			return false, err
		} else if publicKeyChanged {
			version := tuple.IncompleteVersionstamp(uint16(len(changedDevices)))
			a.devices.TxnStoreDeviceChange(txn, requestUserID, id.DeviceID("*"), version)
		}

		return len(changedDevices) > 0 || publicKeyChanged, nil
	})
	if err != nil {
		return err
	} else if recorded {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{requestUserID},
		})
	}
	return nil
}

// The user-signing key is private to its owner, so signatures of it never federate
func (a *AccountsDatabase) txnContainsPublicCrossSigningKey(txn fdb.ReadTransaction, userID id.UserID, keyIDs []id.KeyID) (bool, error) {
	if len(keyIDs) == 0 {
		return false, nil
	}
	keys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
	if err != nil || keys == nil {
		return false, err
	}
	master := id.KeyID(types.CrossSigningPublicKey(keys.Master))
	selfSigning := id.KeyID(types.CrossSigningPublicKey(keys.SelfSigning))
	return slices.ContainsFunc(keyIDs, func(keyID id.KeyID) bool {
		return keyID != "" && (keyID == master || keyID == selfSigning)
	}), nil
}

// Returns target user → target key → signer key → signature
func (a *AccountsDatabase) GetKeySignaturesBySigner(
	ctx context.Context,
	requestUserID id.UserID,
	targets map[id.UserID]map[id.KeyID]struct{},
) (map[id.UserID]map[id.KeyID]map[id.KeyID]string, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (map[id.UserID]map[id.KeyID]map[id.KeyID]string, error) {
		_, signatures := a.users.TxnGetKeySignatures(txn, requestUserID, targets)
		return signatures, nil
	})
}

// The owner's signatures by target key: a device ID or a cross-signing public key
func (a *AccountsDatabase) txnGetOwnerSignatures(
	txn fdb.ReadTransaction,
	userID id.UserID,
	keyIDs []id.KeyID,
) map[id.KeyID]map[id.KeyID]string {
	targets := make(map[id.KeyID]struct{}, len(keyIDs))
	for _, keyID := range keyIDs {
		if keyID != "" {
			targets[keyID] = struct{}{}
		}
	}
	_, found := a.users.TxnGetKeySignatures(txn, userID, map[id.UserID]map[id.KeyID]struct{}{userID: targets})
	return found[userID]
}

func ownerSignedCrossSigningKey(
	key types.SignedCrossSigningKey,
	userID id.UserID,
	ownerSignatures map[id.KeyID]map[id.KeyID]string,
) (types.SignedCrossSigningKey, error) {
	return key.WithOwnerSignatures(userID, ownerSignatures[id.KeyID(types.CrossSigningPublicKey(key))])
}

// Absent when the device has no keys
func (a *AccountsDatabase) GetDeviceKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
) (types.SignedDeviceKeys, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (types.SignedDeviceKeys, error) {
		keys, err := a.devices.TxnGetDeviceKeys(txn, userID, deviceID)
		if err != nil || keys.IsZero() {
			return keys, err
		}
		deviceKeyID := id.KeyID(deviceID)
		ownerSignatures := a.txnGetOwnerSignatures(txn, userID, []id.KeyID{deviceKeyID})
		return keys.WithOwnerSignatures(userID, ownerSignatures[deviceKeyID])
	})
}

// Every key is canonical signed JSON, already verified. Absent device keys are left as stored.
func (a *AccountsDatabase) StoreKeysForDevice(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	deviceKeys types.SignedDeviceKeys,
	oneTimeKeys map[id.KeyID]types.SignedOneTimeKey,
	fallbackKeys map[id.KeyAlgorithm]types.FallbackKey,
) (mautrix.OTKCount, error) {
	log := a.getTxnLogContext(ctx, "StoreKeysForDevice").Logger()

	var counts mautrix.OTKCount
	changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		var changed bool

		if !deviceKeys.IsZero() {
			existingKeys, err := a.devices.TxnGetDeviceKeys(txn, userID, deviceID)
			if err != nil {
				return false, err
			}

			if a.config.SecretSwitches.DisableKeysEqualCheck || existingKeys.IsZero() || !existingKeys.Matches(deviceKeys.Raw) {
				if err := a.devices.TxnStoreDeviceKeys(txn, userID, deviceID, deviceKeys); err != nil {
					return false, err
				}
				a.users.TxnStoreKeySignatures(txn, userID, id.KeyID(deviceID), deviceKeys.View.Signatures)
				if err := a.txnStoreDeviceListChange(txn, userID, deviceID, 0); err != nil {
					return false, err
				}
				changed = true
			}
		}

		if len(oneTimeKeys) > 0 {
			a.devices.TxnStoreOneTimeKeys(txn, userID, deviceID, oneTimeKeys)
		}

		if len(fallbackKeys) > 0 {
			a.devices.TxnStoreFallbackKeys(txn, userID, deviceID, fallbackKeys)
		}

		counts = a.devices.TxnCountOneTimeKeys(ctx, txn, userID, deviceID)

		return changed, nil
	})

	if err != nil {
		return counts, err
	} else if changed {
		a.notifier.SendChange(notifier.Change{
			UserIDs: []id.UserID{userID},
		})
		log.Info().Msg("Uploaded new device keys")
	} else {
		log.Warn().Msg("Ignored duplicate device keys")
	}
	return counts, nil
}

// The user-signing key is private to its owner, so is only returned to them
func (a *AccountsDatabase) GetUserCrossSigningKeys(
	ctx context.Context,
	userID id.UserID,
	requestUserID id.UserID,
) (*types.UserCrossSigningKeys, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.UserCrossSigningKeys, error) {
		keys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
		if err != nil || keys == nil {
			return nil, err
		}
		if userID != requestUserID {
			keys.UserSigning = types.SignedCrossSigningKey{}
		}

		ownedKeys := []*types.SignedCrossSigningKey{&keys.Master, &keys.SelfSigning, &keys.UserSigning}
		keyIDs := make([]id.KeyID, len(ownedKeys))
		for i, key := range ownedKeys {
			keyIDs[i] = id.KeyID(types.CrossSigningPublicKey(*key))
		}
		ownerSignatures := a.txnGetOwnerSignatures(txn, userID, keyIDs)
		for _, key := range ownedKeys {
			if *key, err = ownerSignedCrossSigningKey(*key, userID, ownerSignatures); err != nil {
				return nil, err
			}
		}
		return keys, nil
	})
}

// Keys the upload omits are kept. Every uploaded key is canonical signed JSON, already verified
// against the master key with the given public key, which an upload without a master key requires
// to still be stored.
func (a *AccountsDatabase) StoreUserCrossSigningKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	upload types.UserCrossSigningKeys,
	verifiedMaster id.Ed25519,
) error {
	log := a.getTxnLogContext(ctx, "StoreUserCrossSigningKeys").Logger()

	if changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		existingKeys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
		if err != nil {
			return false, err
		}
		var existing types.UserCrossSigningKeys
		if existingKeys != nil {
			existing = *existingKeys
		}
		if upload.Master.IsZero() && types.CrossSigningPublicKey(existing.Master) != verifiedMaster {
			return false, types.ErrMasterKeyChanged
		}
		keys := existing.Merge(upload)

		if existingKeys != nil && !a.config.SecretSwitches.DisableKeysEqualCheck &&
			sameSignedJSON(existing.Master, keys.Master) &&
			sameSignedJSON(existing.SelfSigning, keys.SelfSigning) &&
			sameSignedJSON(existing.UserSigning, keys.UserSigning) {
			return false, nil
		}

		if err := a.users.TxnStoreUserCrossSigningKeys(txn, userID, keys); err != nil {
			return false, err
		}
		for _, key := range []types.SignedCrossSigningKey{upload.Master, upload.SelfSigning, upload.UserSigning} {
			if !key.IsZero() {
				a.users.TxnStoreKeySignatures(txn, userID, id.KeyID(types.CrossSigningPublicKey(key)), key.View.Signatures)
			}
		}

		// Store change for all devices
		version := tuple.IncompleteVersionstamp(0)
		a.devices.TxnStoreDeviceChange(txn, userID, id.DeviceID("*"), version)

		return true, nil
	}); err != nil {
		return err
	} else {
		if changed {
			a.notifier.SendChange(notifier.Change{
				UserIDs: []id.UserID{userID},
			})
			log.Info().Msg("Uploaded new cross signing keys")
		} else {
			log.Warn().Msg("Ignored duplicate cross signing keys")
		}
		return nil
	}
}

func sameSignedJSON(a, b types.SignedCrossSigningKey) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() == b.IsZero()
	}
	return a.Matches(b.Raw)
}
