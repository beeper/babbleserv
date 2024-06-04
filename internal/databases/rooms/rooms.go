// The rooms database provides Matrix rooms, events & receipts transactions.

package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/sony/sonyflake"
	"github.com/sqids/sqids-go"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/receipts"
	"github.com/beeper/babbleserv/internal/databases/rooms/users"
)

const API_VERSION int = 710

type RoomsDatabase struct {
	log zerolog.Logger
	db  fdb.Database

	roomIDGen     *sonyflake.Sonyflake
	roomIDGenHash *sqids.Sqids

	events   *events.EventsDirectory
	receipts *receipts.ReceiptsDirectory
	users    *users.UsersDirectory

	ByID,
	ByAlias,
	ByPublic subspace.Subspace
}

func NewRoomsDatabase(cfg config.BabbleConfig, logger zerolog.Logger) *RoomsDatabase {
	log := logger.With().
		Str("database", "rooms").
		Logger()

	roomIDGen, err := sonyflake.New(sonyflake.Settings{})
	if err != nil {
		panic(err)
	}

	fdb.MustAPIVersion(API_VERSION)
	db := fdb.MustOpenDatabase(cfg.Databases.Rooms.ClusterFilePath)
	log.Debug().
		Str("cluster_file", cfg.Databases.Rooms.ClusterFilePath).
		Msg("Connected to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Databases.Rooms.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Databases.Rooms.TransactionRetryLimit)

	roomsDir, err := directory.CreateOrOpen(db, []string{"rm"}, nil)
	if err != nil {
		panic(err)
	}

	roomIDGenHash, err := sqids.New(sqids.Options{MinLength: 32})
	if err != nil {
		panic(err)
	}

	return &RoomsDatabase{
		log:           log,
		db:            db,
		roomIDGen:     roomIDGen,
		roomIDGenHash: roomIDGenHash,
		events:        events.NewEventsDirectory(log, db),
		receipts:      receipts.NewReceiptsDirectory(log, db),
		users:         users.NewUsersDirectory(log, db),

		ByID:     roomsDir.Sub("id"),
		ByAlias:  roomsDir.Sub("as"),
		ByPublic: roomsDir.Sub("pb"),
	}
}

func (r *RoomsDatabase) getTxnLogContext(ctx context.Context, name string) zerolog.Context {
	return log.Ctx(ctx).With().
		Str("component", "database").
		Str("database", "rooms").
		Str("transaction", name)
}

func (r *RoomsDatabase) KeyForRoom(roomID id.RoomID) fdb.Key {
	return r.ByID.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) GenerateRoomID() (id.RoomID, error) {
	rid, err := r.roomIDGen.NextID()
	if err != nil {
		return "", nil
	}

	sqid, err := r.roomIDGenHash.Encode([]uint64{rid})
	if err != nil {
		return "", err
	}

	return id.RoomID("!" + sqid + ":localhost"), nil
}
