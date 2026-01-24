// The rooms database provides Matrix rooms, events & receipts transactions.

package rooms

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exsync"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/receipts"
	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/databases/rooms/users"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION = 710

type RoomsDatabase struct {
	log      zerolog.Logger
	db       fdb.Database
	config   config.BabbleConfig
	notifier *notifier.Notifier

	events   *events.EventsDirectory
	users    *users.UsersDirectory
	servers  *servers.ServersDirectory
	receipts *receipts.ReceiptsDirectory

	// Room ID to bytes we decode to `*types.Room` items
	idToRoom subspace.Subspace

	// Room ID to depth int64
	idToDepth subspace.Subspace

	// Room ID to tuple.Versionstamp of the most recent receipt or event in the room
	idToVersion subspace.Subspace

	// Aliases to room IDs (and owner to authz delete)
	//
	// key: RoomAlias
	// value: (RoomID, UserID)
	aliasToID subspace.Subspace

	// Aliases by roomID
	//
	// key: (RoomID, RoomAlias)
	// value: empty
	idAliases subspace.Subspace

	// Per-look lock used to serialize per-room DB writes, this is an optional optimization since
	// FDB will enforce serialization at the DB level.
	roomLocks *exsync.Map[id.RoomID, *sync.Mutex]
}

func NewRoomsDatabase(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	notifier *notifier.Notifier,
) *RoomsDatabase {
	log := logger.With().
		Str("database", "rooms").
		Logger()

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.Rooms.Database.ClusterFilePath)
	log.Info().
		Str("cluster_file", cfg.Rooms.Database.ClusterFilePath).
		Msg("Connecting to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Rooms.Database.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Rooms.Database.TransactionRetryLimit)

	roomsDir, err := directory.CreateOrOpen(db, []string{"rooms"}, nil)
	if err != nil {
		panic(err)
	}

	log.Debug().
		Bytes("prefix", roomsDir.Bytes()).
		Msg("Init rooms directory")

	return &RoomsDatabase{
		log:      log,
		db:       db,
		config:   cfg,
		notifier: notifier,

		events:   events.NewEventsDirectory(log, db, roomsDir),
		users:    users.NewUsersDirectory(log, db, roomsDir),
		servers:  servers.NewServersDirectory(log, db, roomsDir),
		receipts: receipts.NewReceiptsDirectory(log, db, roomsDir),

		idToRoom:    roomsDir.Sub("id"),
		idToDepth:   roomsDir.Sub("idd"),
		idToVersion: roomsDir.Sub("iev"),

		aliasToID: roomsDir.Sub("aid"),
		idAliases: roomsDir.Sub("ida"),

		roomLocks: exsync.NewMap[id.RoomID, *sync.Mutex](),
	}
}

func (r *RoomsDatabase) Stop() {
}

func (r *RoomsDatabase) getTxnLogContext(ctx context.Context, name string) zerolog.Context {
	return zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("database", "rooms").
		Str("transaction", name)
}

func (r *RoomsDatabase) GetTimeForVersion(ctx context.Context, version tuple.Versionstamp) (*time.Time, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*time.Time, error) {
		time := util.TxnGetTimeForVersion(txn, version)
		return time, nil
	})
}

func (r *RoomsDatabase) GenerateRoomID(ctx context.Context) id.RoomID {
	for {
		rid := util.GenerateRandomStringBase32Hex(16)
		roomID := id.RoomID("!" + rid + ":" + r.config.ServerName)

		// Extremely unlikely but check the room ID isn't taken, the chance of this is incredibly
		// small but nice to be sure.
		if existing, err := r.GetRoom(ctx, roomID); err != nil {
			panic(fmt.Errorf("failed to check for existing room: %w", err))
		} else if existing != nil {
			zerolog.Ctx(ctx).Warn().Stringer("room_id", roomID).Msg("Generated duplicate roomID!")
			continue
		}

		return roomID
	}
}

func (r *RoomsDatabase) GetRoom(ctx context.Context, roomID id.RoomID) (*types.Room, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Room, error) {
		key := r.KeyForRoom(roomID)
		b := txn.Get(key).MustGet()
		if b == nil {
			return nil, nil
		}
		ev := types.MustNewRoomFromBytes(b, roomID)
		return ev, nil
	})
}

func (r *RoomsDatabase) GetRoomCurrentExtremEventIDs(ctx context.Context, roomID id.RoomID) ([]id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.EventID, error) {
		return r.events.TxnLookupCurrentRoomExtremEventIDs(txn, roomID), nil
	})
}

func (r *RoomsDatabase) KeyForRoom(roomID id.RoomID) fdb.Key {
	return r.idToRoom.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) KeyForRoomVersion(roomID id.RoomID) fdb.Key {
	return r.idToVersion.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) KeyForRoomDepth(roomID id.RoomID) fdb.Key {
	return r.idToDepth.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) KeyForRoomAlias(roomAlias id.RoomAlias) fdb.Key {
	return r.aliasToID.Pack(tuple.Tuple{roomAlias.String()})
}

func (r *RoomsDatabase) KeyForIdAlias(roomID id.RoomID, roomAlias id.RoomAlias) fdb.Key {
	return r.idAliases.Pack(tuple.Tuple{roomID.String(), roomAlias.String()})
}

func (r *RoomsDatabase) RangeForIDAliases(roomID id.RoomID) fdb.Range {
	return r.idAliases.Sub(roomID.String())
}

func (r *RoomsDatabase) IDAliasKeyToRoomAlias(key fdb.Key) id.RoomAlias {
	tup, _ := r.idAliases.Unpack(key)
	return id.RoomAlias(tup[1].(string))
}
