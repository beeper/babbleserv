package databases

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (d *Databases) SyncForUser(
	ctx context.Context,
	userID id.UserID,
	deviceID id.DeviceID,
	options types.SyncOptions,
	versions types.VersionMap,
) (*types.Sync, error) {
	nextRoomsVersion, rooms, err := d.Rooms.SyncRoomsForUser(ctx, userID, versions[types.RoomsVersionKey], options)
	if err != nil {
		return nil, err
	} else {
		versions[types.RoomsVersionKey] = nextRoomsVersion
	}

	nextAccountsVersion, accounts, err := d.Accounts.SyncAccountsForuser(ctx, userID, versions[types.AccountsVersionKey], options)
	if err != nil {
		return nil, err
	} else {
		versions[types.AccountsVersionKey] = nextAccountsVersion
	}

	nextTransientVersion, toDevice, err := d.Transient.SyncTransientForUser(ctx, userID, deviceID, versions[types.TransientVersionKey], options)
	if err != nil {
		return nil, err
	} else {
		versions[types.TransientVersionKey] = nextTransientVersion
	}

	sync := types.NewSync(rooms, accounts, toDevice)

	sync.NextBatch = util.VersionMapToString(versions)
	return sync, nil
}

func (d *Databases) SyncForServer(
	ctx context.Context,
	serverName string,
	options types.SyncOptions,
	versions types.VersionMap,
) (*types.Sync, error) {
	nextRoomsVersion, rooms, err := d.Rooms.SyncRoomsForServer(ctx, serverName, versions[types.RoomsVersionKey], options)
	if err != nil {
		return nil, err
	} else {
		versions[types.RoomsVersionKey] = nextRoomsVersion
	}

	sync := types.NewSync(rooms, nil, nil)

	// TODO
	// 1. get joined rooms
	// 2. parallel sync transient db x rooms + to-device-outgoing

	sync.NextBatch = util.VersionMapToString(versions)
	return sync, nil
}
