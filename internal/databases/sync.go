package databases

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SyncOptions struct {
	Limit int
}

func (d *Databases) SyncForUser(
	ctx context.Context,
	userID id.UserID,
	versions types.VersionMap,
	options SyncOptions,
) (*types.Sync, error) {
	nextRoomsVersion, rooms, err := d.Rooms.SyncRoomsForUser(ctx, userID, rooms.SyncOptions{
		From:  versions[types.RoomsVersionKey],
		Limit: options.Limit,
	})
	if err != nil {
		return nil, err
	} else {
		versions[types.RoomsVersionKey] = nextRoomsVersion
	}

	sync := types.NewSyncFromRooms(rooms)

	// TODO
	// 1. get joined rooms
	// 2. parallel sync transient db x rooms + accounts + to-device

	sync.NextBatch = util.VersionMapToString(versions)
	return sync, nil
}

func (d *Databases) SyncForServer(
	ctx context.Context,
	serverName string,
	versions types.VersionMap,
	options SyncOptions,
) (*types.Sync, error) {
	nextRoomsVersion, rooms, err := d.Rooms.SyncRoomsForServer(ctx, serverName, rooms.SyncOptions{
		From:  versions[types.RoomsVersionKey],
		Limit: options.Limit,
	})
	if err != nil {
		return nil, err
	} else {
		versions[types.RoomsVersionKey] = nextRoomsVersion
	}

	sync := types.NewSyncFromRooms(rooms)

	// TODO
	// 1. get joined rooms
	// 2. parallel sync transient db x rooms + to-device-outgoing

	sync.NextBatch = util.VersionMapToString(versions)
	return sync, nil
}
