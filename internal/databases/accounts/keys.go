package accounts

import (
	"context"
	"fmt"
	"maps"

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
) (map[id.KeyID]mautrix.OneTimeKey, error) {
	log := zerolog.Ctx(ctx)

	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (map[id.KeyID]mautrix.OneTimeKey, error) {
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
			return map[id.KeyID]mautrix.OneTimeKey{
				key.KeyID: key.Key,
			}, nil
		}

		log.Warn().Msg("No one time or fallback keys found")
		return nil, nil
	})
}

func (a *AccountsDatabase) StoreKeySignatures(
	ctx context.Context,
	signatures map[id.UserID]map[id.KeyID]signatures.Signatures,
) error {
	_, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (any, error) {
		for userID, keyToSignatures := range signatures {
			for keyID, signatures := range keyToSignatures {
				a.users.TxnStoreKeySignatures(txn, userID, keyID, signatures)
			}
		}
		return nil, nil
	})
	return err
}

func (a *AccountsDatabase) GetDeviceKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	requestUserID id.UserID,
) (*mautrix.DeviceKeys, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*mautrix.DeviceKeys, error) {
		keys, err := a.devices.TxnGetDeviceKeys(txn, userID, deviceID)
		if err != nil {
			return nil, err
		} else if keys == nil {
			return nil, nil
		}

		deviceKeyID := id.KeyID(deviceID)

		keysForSignatures := map[id.UserID]map[id.KeyID]struct{}{
			// Get any signatures from the device users own keys
			userID: {
				deviceKeyID: {},
			},
		}
		if userID != requestUserID {
			// If we're requesting another users keys, grab any signatures from our keys for their device
			keysForSignatures[requestUserID] = map[id.KeyID]struct{}{
				deviceKeyID: {},
			}
		}

		found, signatures := a.users.TxnGetKeySignatures(txn, userID, keysForSignatures)
		zerolog.Ctx(ctx).Debug().
			Int("signatures", found).
			Str("target_user_id", userID.String()).
			Str("target_device_id", deviceID.String()).
			Msg("Found signatures for user device keys")

		for keyID, keySigs := range signatures[userID] {
			if len(keySigs) == 0 {
				continue
			}
			if keyID != deviceKeyID {
				panic(fmt.Errorf("unexpected signature for keyID: %s", keyID))
			}
			if keys.Signatures == nil {
				keys.Signatures = map[id.UserID]map[id.KeyID]string{
					userID: make(map[id.KeyID]string, len(keySigs)),
				}
			}
			maps.Copy(keys.Signatures[userID], keySigs)
		}

		return keys, nil
	})
}

func (a *AccountsDatabase) StoreKeysForDevice(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	deviceKeys *mautrix.DeviceKeys,
	oneTimeKeys map[id.KeyID]mautrix.OneTimeKey,
	fallbackKeys map[id.KeyAlgorithm]types.FallbackKey,
) (mautrix.OTKCount, error) {
	log := a.getTxnLogContext(ctx, "StoreKeysForDevice").Logger()

	var counts mautrix.OTKCount
	changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		var changed bool

		if deviceKeys != nil {
			keys := *deviceKeys
			existingKeys, err := a.devices.TxnGetDeviceKeys(txn, userID, deviceID)
			if err != nil {
				return false, err
			}

			if a.config.SecretSwitches.DisableKeysEqualCheck || !util.CompareSignedObjectsJSON(existingKeys, keys) {
				a.devices.TxnStoreDeviceKeys(txn, userID, deviceID, keys)
				a.users.TxnIncrementUserDeviceListVersion(txn, userID)
				a.users.TxnStoreKeySignatures(txn, userID, id.KeyID(deviceID), keys.Signatures)

				// Store change for this device
				version := tuple.IncompleteVersionstamp(0)
				a.devices.TxnStoreDeviceChange(txn, userID, deviceID, version)
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

func (a *AccountsDatabase) GetUserCrossSigningKeys(
	ctx context.Context,
	userID id.UserID,
	requestUserID id.UserID,
) (*types.UserCrossSigningKeys, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.UserCrossSigningKeys, error) {
		keys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
		if err != nil {
			return nil, err
		} else if keys == nil {
			return nil, nil
		}

		keysForSignatures := map[id.UserID]map[id.KeyID]struct{}{
			// Always grab signatures for the xs keys from the owner of those xs keys
			userID: {
				keys.Master.KeyID():      {},
				keys.SelfSigning.KeyID(): {},
			},
		}

		if userID == requestUserID {
			// If we're requesting our own keys, also grab signatures for our user signing key
			keysForSignatures[userID][keys.UserSigning.KeyID()] = struct{}{}
		} else {
			// If we're requesting another users keys, also grab signatures from our keys
			keysForSignatures[requestUserID] = map[id.KeyID]struct{}{
				keys.Master.KeyID():      {},
				keys.SelfSigning.KeyID(): {},
			}
			// Empty the user signing xs key as this is private to the user only
			keys.UserSigning = types.CrossSigningKey{}
		}

		found, signatures := a.users.TxnGetKeySignatures(txn, userID, keysForSignatures)
		zerolog.Ctx(ctx).Debug().
			Int("signatures", found).
			Str("target_user_id", userID.String()).
			Msg("Found signatures for user cross signing keys")

		for keyID, keySigs := range signatures[userID] {
			if len(keySigs) == 0 {
				continue
			}
			switch keyID {
			case keys.Master.KeyID():
				if keys.Master.Signatures == nil {
					keys.Master.Signatures = map[id.UserID]map[id.KeyID]string{
						userID: make(map[id.KeyID]string, len(keySigs)),
					}
				}
				maps.Copy(keys.Master.Signatures[userID], keySigs)

			case keys.SelfSigning.KeyID():
				if keys.SelfSigning.Signatures == nil {
					keys.SelfSigning.Signatures = map[id.UserID]map[id.KeyID]string{
						userID: make(map[id.KeyID]string, len(keySigs)),
					}
				}
				maps.Copy(keys.SelfSigning.Signatures[userID], keySigs)

			case keys.UserSigning.KeyID():
				if keys.UserSigning.Signatures == nil {
					keys.UserSigning.Signatures = map[id.UserID]map[id.KeyID]string{
						userID: make(map[id.KeyID]string, len(keySigs)),
					}
				}
				maps.Copy(keys.UserSigning.Signatures[userID], keySigs)
			}
		}

		return keys, nil
	})
}

func (a *AccountsDatabase) StoreUserCrossSigningKeys(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	keys types.UserCrossSigningKeys,
) error {
	log := a.getTxnLogContext(ctx, "StoreUserCrossSigningKeys").Logger()

	if changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		existingKeys, err := a.users.TxnGetUserCrossSigningKeys(txn, userID)
		if err != nil {
			return false, err
		}

		if existingKeys == nil || a.config.SecretSwitches.DisableKeysEqualCheck ||
			!util.CompareSignedObjectsJSON(existingKeys.Master, keys.Master) ||
			!util.CompareSignedObjectsJSON(existingKeys.SelfSigning, keys.SelfSigning) ||
			!util.CompareSignedObjectsJSON(existingKeys.UserSigning, keys.UserSigning) {

			a.users.TxnStoreUserCrossSigningKeys(txn, userID, keys)
			a.users.TxnStoreKeySignatures(txn, userID, keys.Master.KeyID(), keys.Master.Signatures)
			a.users.TxnStoreKeySignatures(txn, userID, keys.SelfSigning.KeyID(), keys.SelfSigning.Signatures)
			a.users.TxnStoreKeySignatures(txn, userID, keys.UserSigning.KeyID(), keys.UserSigning.Signatures)

			// Store change for all devices
			version := tuple.IncompleteVersionstamp(0)
			a.devices.TxnStoreDeviceChange(txn, userID, id.DeviceID("*"), version)

			return true, nil
		}

		return false, nil
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
