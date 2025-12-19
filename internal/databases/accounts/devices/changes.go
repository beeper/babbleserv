package devices

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/beeper/babbleserv/internal/types"
	"maunium.net/go/mautrix/id"
)

func (d *DevicesDirectory) keyForDeviceChange(version tuple.Versionstamp) fdb.Key {
	key, err := d.deviceChanges.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (d *DevicesDirectory) TxnStoreDeviceChange(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, version tuple.Versionstamp) {
	txn.SetVersionstampedKey(
		d.keyForDeviceChange(version),
		tuple.Tuple{userID.String(), deviceID.String()}.Pack(),
	)
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
		ids = append(ids, types.UserDeviceChange{
			Version: keyTup[0].(tuple.Versionstamp),
			UserDevice: types.UserDevice{
				UserID:   id.UserID(valueTup[0].(string)),
				DeviceID: id.DeviceID(valueTup[1].(string)),
			},
		})
	}

	return ids, nil
}
