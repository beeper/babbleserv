package devices

import (
	"context"
	"encoding/json"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/rs/zerolog"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func (d *DevicesDirectory) keyForDeviceKeys(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return d.deviceKeys.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) TxnStoreDeviceKeys(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, keys mautrix.DeviceKeys) {
	b, err := json.Marshal(keys)
	if err != nil {
		panic(err)
	}

	// Device keys signatures are stored separately
	b, _ = sjson.DeleteBytes(b, "signatures")

	key := d.keyForDeviceKeys(userID, deviceID)
	txn.Set(key, b)
}

func (d *DevicesDirectory) TxnGetDeviceKeys(txn fdb.ReadTransaction, userID id.UserID, deviceID id.DeviceID) (*mautrix.DeviceKeys, error) {
	key := d.keyForDeviceKeys(userID, deviceID)
	b, err := txn.Get(key).Get()
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, nil
	}

	var keys mautrix.DeviceKeys
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}
	return &keys, nil
}

func (d *DevicesDirectory) keyForOneTimeKey(userID id.UserID, deviceID id.DeviceID, keyID id.KeyID) fdb.Key {
	algorithm, name := keyID.Parse()
	return d.deviceOneTimeKeys.Pack(tuple.Tuple{userID.String(), deviceID.String(), string(algorithm), name})
}

func (d *DevicesDirectory) TxnStoreOneTimeKeys(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, keys map[id.KeyID]mautrix.OneTimeKey) {
	var v uint16
	for keyID, key := range keys {
		b, err := json.Marshal(key)
		if err != nil {
			panic(err)
		}
		txn.Set(d.keyForOneTimeKey(userID, deviceID, keyID), b)

		algorithm, name := keyID.Parse()
		k, err := d.deviceOneTimeKeysByVersion.PackWithVersionstamp(tuple.Tuple{
			userID.String(),
			deviceID.String(),
			string(algorithm),
			tuple.IncompleteVersionstamp(v),
		})
		if err != nil {
			panic(err)
		}
		txn.SetVersionstampedKey(k, []byte(name))
		v++
	}
}

func (d *DevicesDirectory) TxnClaimOneTimeKeys(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	algorithm id.KeyAlgorithm,
	limit int, // always 1 currently
) map[id.KeyID]mautrix.OneTimeKey {
	kVersions := txn.GetRange(
		d.deviceOneTimeKeysByVersion.Sub(userID.String(), deviceID.String(), string(algorithm)),
		fdb.RangeOptions{
			Mode:  fdb.StreamingModeWantAll,
			Limit: limit,
		},
	).GetSliceOrPanic()
	if len(kVersions) == 0 {
		return nil
	}

	keys := make(map[id.KeyID]mautrix.OneTimeKey, len(kVersions))

	for _, kVersion := range kVersions {
		keyID := id.NewKeyID(algorithm, string(kVersion.Value))
		otkKey := d.keyForOneTimeKey(userID, deviceID, keyID)

		// Grab the key itself
		keyB := txn.Get(otkKey).MustGet()
		if keyB == nil {
			// Should be impossible as we clear both the key and version key together
			panic("got nil one time key from version")
		}
		var key mautrix.OneTimeKey
		if err := json.Unmarshal(keyB, &key); err != nil {
			panic(err)
		}

		keys[keyID] = key

		// Clear the key and the version key
		txn.Clear(otkKey)
		txn.Clear(kVersion.Key)
	}

	return keys
}

func (d *DevicesDirectory) TxnCountOneTimeKeys(ctx context.Context, txn fdb.ReadTransaction, userID id.UserID, deviceID id.DeviceID) mautrix.OTKCount {
	counts := mautrix.OTKCount{}
	kvs := txn.GetRange(
		d.deviceOneTimeKeys.Sub(userID.String(), deviceID.String()),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).GetSliceOrPanic()
	for _, kv := range kvs {
		kTup, err := d.deviceOneTimeKeys.Unpack(kv.Key)
		if err != nil {
			panic(err)
		}
		algorithm := id.KeyAlgorithm(kTup[2].(string))
		switch algorithm {
		case id.KeyAlgorithmSignedCurve25519:
			counts.SignedCurve25519 += 1
		case id.KeyAlgorithmCurve25519:
			counts.Curve25519 += 1
		default:
			zerolog.Ctx(ctx).Warn().Str("algorithm", string(algorithm)).Msg("Unknown one time key algorithm")
		}
	}

	return counts
}

func (d *DevicesDirectory) keyForFallbackKey(userID id.UserID, deviceID id.DeviceID, algorithm id.KeyAlgorithm) fdb.Key {
	return d.deviceFallbackKeys.Pack(tuple.Tuple{userID.String(), deviceID.String(), string(algorithm)})
}

func (d *DevicesDirectory) TxnStoreFallbackKeys(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, keys map[id.KeyAlgorithm]types.FallbackKey) {
	for algorithm, key := range keys {
		b, err := json.Marshal(key)
		if err != nil {
			panic(err)
		}
		txn.Set(d.keyForFallbackKey(userID, deviceID, algorithm), b)
	}
}

func (d *DevicesDirectory) TxnGetFallbackKeys(txn fdb.ReadTransaction, userID id.UserID, deviceID id.DeviceID) map[id.KeyAlgorithm]types.FallbackKey {
	kvs := txn.GetRange(
		d.deviceFallbackKeys.Sub(userID.String(), deviceID.String()),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).GetSliceOrPanic()

	keys := make(map[id.KeyAlgorithm]types.FallbackKey, len(kvs))

	for _, kv := range kvs {
		kTup, err := d.deviceFallbackKeys.Unpack(kv.Key)
		if err != nil {
			panic(err)
		}
		algorithm := id.KeyAlgorithm(kTup[2].(string))
		var key types.FallbackKey
		if err := json.Unmarshal(kv.Value, &key); err != nil {
			panic(err)
		}
		keys[algorithm] = key
	}

	return keys
}
