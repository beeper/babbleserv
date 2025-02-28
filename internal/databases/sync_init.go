package databases

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (d *Databases) InitForUser(
	ctx context.Context,
	userID id.UserID,
) (*types.Sync, error) {
	versions := make(types.VersionMap, 3)

	nextRoomsVersion, rooms, err := d.Rooms.InitRoomsForUser(ctx, userID)
	if err != nil {
		return nil, err
	} else {
		versions[types.RoomsVersionKey] = nextRoomsVersion
	}

	sync := types.NewSyncFromRooms(rooms)

	sync.NextBatch = util.VersionMapToString(versions)
	return sync, nil
}
