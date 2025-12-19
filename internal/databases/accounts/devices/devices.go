package devices

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
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
	// value: types.UserDevice
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
	return d.deviceSyncConns.Sub(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) TxnDeleteDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID) {
	txn.Clear(d.keyForDevice(userID, deviceID))
	txn.Clear(d.keyForDeviceLastSeen(userID, deviceID))
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
	txn.Set(d.keyForDevice(userID, device.ID), device.ToMsgpack())
}

func (d *DevicesDirectory) txnStoreNewDevice(txn fdb.Transaction, userID id.UserID, device *types.Device) {
	// Store a change (device list update) for this user/device
	d.TxnStoreDeviceChange(txn, userID, device.ID, tuple.IncompleteVersionstamp(0))
	// Store the device itself
	d.TxnStoreDevice(txn, userID, device)
}

func (d *DevicesDirectory) TxnGetOrCreateDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, initialDisplayName string) (*types.Device, error) {
	device, err := d.TxnGetDevice(txn, userID, deviceID)
	if err != nil {
		return nil, err
	} else if device == nil {
		device = types.NewDevice(deviceID, initialDisplayName)
		d.txnStoreNewDevice(txn, userID, device)
	}
	return device, nil
}

func (d *DevicesDirectory) TxnSetDeviceLastSeen(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	ip string,
	time time.Time,
) {
	key := d.keyForDeviceLastSeen(userID, deviceID)
	value := tuple.Tuple{ip, time}.Pack()
	txn.Set(key, value)
}
