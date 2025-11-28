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
	byUserDeviceID subspace.Subspace

	// UserID/DeviceID to last seen time, separate to device bytes since updated on any req
	userDeviceLastSeen subspace.Subspace

	// UserID/DeviceID/ConnID to types.UserSyncConn msgpack bytes, stores basic information about
	// (native/sliding) sync connections which v2/streaming also use. Also acts as a lock preventing
	// parallel sync calls with the same deviceid/connid. Updated every sync req.
	syncConns subspace.Subspace
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

		byUserDeviceID:     devicesDir.Sub("udi"),
		syncConns:          devicesDir.Sub("scn"),
		userDeviceLastSeen: devicesDir.Sub("uds"),
	}
}

func (d *DevicesDirectory) RangeForUserDevices(userID id.UserID) fdb.ExactRange {
	return d.byUserDeviceID.Sub(userID.String())
}

func (d *DevicesDirectory) KeyForDevice(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return d.byUserDeviceID.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) KeyForDeviceLastSeen(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return d.userDeviceLastSeen.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) KeyForDeviceSyncConn(userID id.UserID, deviceID id.DeviceID, connID string) fdb.Key {
	return d.syncConns.Pack(tuple.Tuple{userID.String(), deviceID.String(), connID})
}

func (d *DevicesDirectory) RangeForDeviceSyncConns(userID id.UserID, deviceID id.DeviceID) fdb.ExactRange {
	return d.syncConns.Sub(tuple.Tuple{userID.String(), deviceID.String()})
}

func (d *DevicesDirectory) TxnDeleteDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID) {
	txn.Clear(d.KeyForDevice(userID, deviceID))
	txn.Clear(d.KeyForDeviceLastSeen(userID, deviceID))
	txn.ClearRange(d.RangeForDeviceSyncConns(userID, deviceID))
}

func (d *DevicesDirectory) TxnGetOrCreateDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, initialDisplayName string) (*types.Device, error) {
	key := d.KeyForDevice(userID, deviceID)
	if kv, err := txn.Get(key).Get(); err != nil {
		return nil, err
	} else if kv == nil {
		device := types.NewDevice(deviceID, initialDisplayName)
		txn.Set(key, device.ToMsgpack())
		return device, nil
	} else {
		device := types.MustNewDeviceFromBytes(kv)
		return device, nil
	}
}

func (d *DevicesDirectory) TxnSetDeviceLastSeen(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	ip string,
	time time.Time,
) {
	key := d.KeyForDeviceLastSeen(userID, deviceID)
	value := tuple.Tuple{ip, time}.Pack()
	txn.Set(key, value)
}
