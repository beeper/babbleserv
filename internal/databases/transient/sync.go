package transient

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (t *TransientDatabase) SyncTransientForUser(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	fromVersion tuple.Versionstamp,
	syncOpts types.SyncOptions,
) (tuple.Versionstamp, []*types.ToDevice, error) {
	var latestVersion tuple.Versionstamp
	var toDevice []*types.ToDevice

	_, err := util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		latestVersion = util.TxnGetLatestWriteVersion(txn)

		rng := t.todevice.RangeForLocalUserVersion(userID, deviceID, fromVersion, latestVersion)
		iter := txn.GetRange(
			rng,
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		tds := make([]*types.ToDevice, 0)

		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			version := t.todevice.KeyToLocalUserVersion(kv.Key)
			if types.VersionIsBefore(version, fromVersion) && fromVersion != types.ZeroVersionstamp {
				zerolog.Ctx(ctx).Error().
					Any("from_version", fromVersion).
					Any("to_device_version", version).
					Msg("Got to-device before our from version!")
			}
			td := types.MustBytesToToDevice(kv.Value)
			tds = append(tds, td)
		}

		toDevice = tds
		return nil, nil
	})

	if fromVersion != types.ZeroVersionstamp {
		backgroundCtx := zerolog.Ctx(ctx).WithContext(context.Background())
		go func() {
			if _, err := util.DoWriteTransaction(backgroundCtx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
				// If incremental sync remove anything prior to the fromVersion we are persisted up to
				txn.ClearRange(t.todevice.RangeForLocalUserVersion(userID, deviceID, types.ZeroVersionstamp, fromVersion))
				return nil, nil
			}); err != nil {
				zerolog.Ctx(backgroundCtx).Err(err).Msg("Failed to clear old to-device messages")
			}
		}()
	}

	return latestVersion, toDevice, err
}

func (t *TransientDatabase) SyncTransientForServer(
	ctx context.Context,
	serverName string,
	fromVersion tuple.Versionstamp,
	syncOpts types.SyncOptions,
) (tuple.Versionstamp, []*types.ToDevice, error) {
	var latestVersion tuple.Versionstamp
	var toDevice []*types.ToDevice

	_, err := util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		latestVersion = util.TxnGetLatestWriteVersion(txn)

		iter := txn.GetRange(
			t.todevice.RangeForRemoteServerVersion(serverName, fromVersion, latestVersion),
			fdb.RangeOptions{
				Mode: fdb.StreamingModeWantAll,
			},
		).Iterator()

		tds := make([]*types.ToDevice, 0)

		for iter.Advance() {
			kv, err := iter.Get()
			if err != nil {
				return nil, err
			}
			td := types.MustBytesToToDevice(kv.Value)
			tds = append(tds, td)
		}

		toDevice = tds
		return nil, nil
	})

	if fromVersion != types.ZeroVersionstamp {
		backgroundCtx := zerolog.Ctx(ctx).WithContext(context.Background())
		go func() {
			if _, err := util.DoWriteTransaction(backgroundCtx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
				// If incremental sync remove anything prior to the fromVersion we are persisted up to
				txn.ClearRange(t.todevice.RangeForRemoteServerVersion(serverName, types.ZeroVersionstamp, fromVersion))
				return nil, nil
			}); err != nil {
				zerolog.Ctx(backgroundCtx).Err(err).Msg("Failed to clear old to-device messages")
			}
		}()
	}
	return latestVersion, toDevice, err
}
