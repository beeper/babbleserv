package devices

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func (d *DevicesDirectory) keyForDeviceChange(version tuple.Versionstamp) fdb.Key {
	key, err := d.deviceChanges.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func deviceChangeValue(userID id.UserID, deviceID id.DeviceID, stream *types.DeviceListStream) tuple.Tuple {
	if stream == nil {
		return tuple.Tuple{userID.String(), deviceID.String()}
	}
	return tuple.Tuple{userID.String(), deviceID.String(), stream.StreamID, stream.PrevID}
}

func deviceChangeFromTuples(keyTup, valueTup tuple.Tuple) types.UserDeviceChange {
	change := types.UserDeviceChange{
		Version: keyTup[0].(tuple.Versionstamp),
		UserDevice: types.UserDevice{
			UserID:   id.UserID(valueTup[0].(string)),
			DeviceID: id.DeviceID(valueTup[1].(string)),
		},
	}
	if len(valueTup) == 4 {
		change.Stream = &types.DeviceListStream{
			StreamID: valueTup[2].(int64),
			PrevID:   valueTup[3].(int64),
		}
	}
	return change
}

func (d *DevicesDirectory) TxnStoreDeviceChange(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, version tuple.Versionstamp) {
	txn.SetVersionstampedKey(d.keyForDeviceChange(version), deviceChangeValue(userID, deviceID, nil).Pack())
}

func (d *DevicesDirectory) TxnStoreDeviceListChange(
	txn fdb.Transaction,
	userID id.UserID,
	deviceID id.DeviceID,
	version tuple.Versionstamp,
	stream types.DeviceListStream,
) {
	txn.SetVersionstampedKey(d.keyForDeviceChange(version), deviceChangeValue(userID, deviceID, &stream).Pack())
}

// Remove any device changes from zero through to and including toVersion
func (d *DevicesDirectory) TxnClearDeviceChanges(txn fdb.Transaction, toVersion tuple.Versionstamp) {
	txn.ClearRange(types.GetVersionRange(d.deviceChanges, types.ZeroVersionstamp, toVersion))
}

func (d *DevicesDirectory) TxnPaginateDeviceChanges(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
) ([]types.UserDeviceChange, error) {
	iter := txn.GetRange(
		types.GetVersionRange(d.deviceChanges, options.From, options.To),
		options.RangeOptions(),
	).Iterator()
	ids := make([]types.UserDeviceChange, 0, options.Limit)

	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}
		keyTup, _ := d.deviceChanges.Unpack(kv.Key)
		valueTup, _ := tuple.Unpack(kv.Value)
		ids = append(ids, deviceChangeFromTuples(keyTup, valueTup))
	}

	return ids, nil
}
