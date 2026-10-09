package devices

import (
	"encoding/json"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type DevicesDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// UserID/DeviceID to types.Device msgpack bytes
	//
	// key: (id.userID, id.DeviceID)
	// value: types.Device
	userDevices subspace.Subspace

	// UserID/DeviceID to last seen time, separate to device bytes since updated on any req
	deviceLastSeen subspace.Subspace

	deviceKeys subspace.Subspace

	// key: (id.UserID, id.DeviceID, id.KeyAlgorithm, id.KeyID)
	// value: mautrix.OneTimeKey
	deviceOneTimeKeys subspace.Subspace

	// Index keys by algo/version to claim in uploaded order (MSC4225), can also be used to clean
	// old keys in the future.
	//
	// key: (id.UserID, id.DeviceID, id.KeyAlgorithm, tuple.Versionstamp)
	// value: id.KeyID
	deviceOneTimeKeysByVersion subspace.Subspace

	// key: (id.UserID, id.DeviceID, id.KeyAlgorithm)
	// value: mautrix.OneTimeKey
	deviceFallbackKeys subspace.Subspace

	// version -> device change, paginated and cleared by the DeviceChangeIterator worker
	//
	// key: tuple.Versionstamp
	// value: (id.UserID, id.DeviceID) or, when a local device-list version was allocated,
	// (id.UserID, id.DeviceID, stream ID, prev ID)
	deviceChanges subspace.Subspace

	// UserID/DeviceID/ConnID to types.UserSyncConn msgpack bytes, stores basic information about
	// (native/sliding) sync connections which v2/streaming also use. Also acts as a lock preventing
	// parallel sync calls with the same deviceid/connid. Updated every sync req.
	deviceSyncConns subspace.Subspace
}

func NewDevicesDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *DevicesDirectory {
	devicesDir, err := parentDir.CreateOrOpen(db, []string{"devices"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "devices").Logger()
	log.Debug().
		Bytes("prefix", devicesDir.Bytes()).
		Msg("Init accounts/devices directory")

	return &DevicesDirectory{
		log: log,
		db:  db,

		userDevices:                devicesDir.Sub("ud"),
		deviceLastSeen:             devicesDir.Sub("ds"),
		deviceKeys:                 devicesDir.Sub("dk"),
		deviceOneTimeKeys:          devicesDir.Sub("otk"),
		deviceOneTimeKeysByVersion: devicesDir.Sub("otv"),
		deviceFallbackKeys:         devicesDir.Sub("fbk"),
		deviceChanges:              devicesDir.Sub("dch"),
		deviceSyncConns:            devicesDir.Sub("dsc"),
	}
}

func (d *DevicesDirectory) RangeForUserDevices(userID id.UserID) fdb.ExactRange {
	return d.userDevices.Sub(userID.String())
}

func (d *DevicesDirectory) keyForDevice(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return d.userDevices.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) keyForDeviceLastSeen(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return d.deviceLastSeen.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) RangeForDeviceSyncConns(userID id.UserID, deviceID id.DeviceID) fdb.ExactRange {
	return d.deviceSyncConns.Sub(userID.String(), deviceID.String())
}

func (d *DevicesDirectory) TxnDeleteDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID) {
	txn.Clear(d.keyForDevice(userID, deviceID))
	txn.Clear(d.keyForDeviceLastSeen(userID, deviceID))
	txn.Clear(d.deviceKeys.Pack(tuple.Tuple{userID.String(), deviceID.String()}))
	txn.ClearRange(d.deviceOneTimeKeys.Sub(userID.String(), deviceID.String()))
	txn.ClearRange(d.deviceOneTimeKeysByVersion.Sub(userID.String(), deviceID.String()))
	txn.ClearRange(d.deviceFallbackKeys.Sub(userID.String(), deviceID.String()))
	txn.ClearRange(d.RangeForDeviceSyncConns(userID, deviceID))
}

func (d *DevicesDirectory) TxnGetDevice(txn fdb.ReadTransaction, userID id.UserID, deviceID id.DeviceID) (*types.Device, error) {
	key := d.keyForDevice(userID, deviceID)
	if kv, err := txn.Get(key).Get(); err != nil {
		return nil, err
	} else if kv == nil {
		return nil, nil
	} else {
		return types.NewDeviceFromBytes(kv)
	}
}

func (d *DevicesDirectory) TxnStoreDevice(txn fdb.Transaction, userID id.UserID, device *types.Device) {
	txn.Set(d.keyForDevice(userID, device.ID), device.ToBytes())
}

func (d *DevicesDirectory) TxnGetOrCreateDevice(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	initialDisplayName string,
) (*types.Device, bool, error) {
	device, err := d.TxnGetDevice(txn, userID, deviceID)
	if err != nil {
		return nil, false, err
	} else if device != nil {
		return device, false, nil
	}
	device = types.NewDevice(deviceID, initialDisplayName)
	d.TxnStoreDevice(txn, userID, device)
	return device, true, nil
}

func (d *DevicesDirectory) TxnGetUserDevicesWithKeys(txn fdb.ReadTransaction, userID id.UserID) ([]types.LocalSnapshotDevice, error) {
	devicesRange := txn.GetRange(d.RangeForUserDevices(userID), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})
	keysRange := txn.GetRange(d.deviceKeys.Sub(userID.String()), fdb.RangeOptions{Mode: fdb.StreamingModeWantAll})

	deviceKVs, err := devicesRange.GetSliceWithError()
	if err != nil {
		return nil, err
	}
	keyKVs, err := keysRange.GetSliceWithError()
	if err != nil {
		return nil, err
	}

	keysByDevice := make(map[id.DeviceID]*mautrix.DeviceKeys, len(keyKVs))
	for _, kv := range keyKVs {
		keyTup, err := d.deviceKeys.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		if keysByDevice[id.DeviceID(keyTup[1].(string))], err = deviceKeysFromBytes(kv.Value); err != nil {
			return nil, err
		}
	}

	devices := make([]types.LocalSnapshotDevice, len(deviceKVs))
	for i, kv := range deviceKVs {
		device := types.MustNewDeviceFromBytes(kv.Value)
		devices[i] = types.LocalSnapshotDevice{Device: *device, Keys: keysByDevice[device.ID]}
	}
	return devices, nil
}

// Both reads are issued before either is awaited; keys are nil for a missing device
func (d *DevicesDirectory) TxnGetDeviceWithKeys(
	txn fdb.ReadTransaction,
	userID id.UserID,
	deviceID id.DeviceID,
) (*types.Device, *mautrix.DeviceKeys, error) {
	deviceFuture := txn.Get(d.keyForDevice(userID, deviceID))
	keysFuture := txn.Get(d.keyForDeviceKeys(userID, deviceID))

	deviceBytes, err := deviceFuture.Get()
	if err != nil || deviceBytes == nil {
		return nil, nil, err
	}
	device, err := types.NewDeviceFromBytes(deviceBytes)
	if err != nil {
		return nil, nil, err
	}
	keysBytes, err := keysFuture.Get()
	if err != nil || keysBytes == nil {
		return device, nil, err
	}
	keys, err := deviceKeysFromBytes(keysBytes)
	return device, keys, err
}

func deviceKeysFromBytes(b []byte) (*mautrix.DeviceKeys, error) {
	var keys mautrix.DeviceKeys
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, err
	}
	return &keys, nil
}

func (d *DevicesDirectory) TxnSetDeviceLastSeen(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, ip string, time time.Time) {
	key := d.keyForDeviceLastSeen(userID, deviceID)
	value := tuple.Tuple{ip, time}.Pack()
	txn.Set(key, value)
}
