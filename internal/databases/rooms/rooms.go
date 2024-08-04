// The rooms database provides Matrix rooms, events & receipts transactions.

package rooms

import (
	"context"
	"crypto/md5"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/databases/rooms/users"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const API_VERSION int = 710

type RoomsDatabase struct {
	backgroundWg sync.WaitGroup

	log      zerolog.Logger
	db       fdb.Database
	config   config.BabbleConfig
	notifier *notifier.Notifier

	root  subspace.Subspace
	locks subspace.Subspace

	events  *events.EventsDirectory
	users   *users.UsersDirectory
	servers *servers.ServersDirectory

	byID,
	byAlias,
	byPublic subspace.Subspace

	// The super stream combines, by room, events and receipts
	superStream subspace.Subspace
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
	db := fdb.MustOpenDatabase(cfg.Databases.Rooms.ClusterFilePath)
	log.Debug().
		Str("cluster_file", cfg.Databases.Rooms.ClusterFilePath).
		Msg("Connected to FoundationDB")

	db.Options().SetTransactionTimeout(cfg.Databases.Rooms.TransactionTimeout)
	db.Options().SetTransactionRetryLimit(cfg.Databases.Rooms.TransactionRetryLimit)

	roomsDir, err := directory.CreateOrOpen(db, []string{"rooms"}, nil)
	if err != nil {
		panic(err)
	}

	log.Trace().
		Bytes("prefix", roomsDir.Bytes()).
		Msg("Init rooms directory")

	return &RoomsDatabase{
		log:      log,
		db:       db,
		config:   cfg,
		notifier: notifier,

		root:  roomsDir,
		locks: roomsDir.Sub("lck"),

		events:  events.NewEventsDirectory(log, db, roomsDir),
		users:   users.NewUsersDirectory(log, db, roomsDir),
		servers: servers.NewServersDirectory(log, db, roomsDir),

		byID:     roomsDir.Sub("id"),
		byAlias:  roomsDir.Sub("as"),
		byPublic: roomsDir.Sub("pb"),

		superStream: roomsDir.Sub("ss"),
	}
}

func (r *RoomsDatabase) Stop() {
	r.log.Debug().Msg("Waiting for any background jobs to complete...")
	r.backgroundWg.Wait()
}

func (r *RoomsDatabase) GetLockPrimitives() (fdb.Database, subspace.Subspace) {
	return r.db, r.locks
}

func (r *RoomsDatabase) getTxnLogContext(ctx context.Context, name string) zerolog.Context {
	return zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("database", "rooms").
		Str("transaction", name)
}

func (r *RoomsDatabase) GenerateRoomID() (id.RoomID, error) {
	// RoomID's don't need to be cryptographically secure, so we use xid, but
	// to make them easier to identify (clearly distinct strings) we md5 the
	// xid and base64 the result.
	sum := md5.Sum([]byte(xid.New().Bytes()))
	rid := util.Base64EncodeURLSafe(sum[:])
	return id.RoomID("!" + rid + ":" + r.config.ServerName), nil
}

func (r *RoomsDatabase) GetRoom(ctx context.Context, roomID id.RoomID) (*types.Room, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Room, error) {
		key := r.KeyForRoom(roomID)
		b := txn.Get(key).MustGet()
		if b == nil {
			return nil, nil
		}
		ev := types.MustNewRoomFromBytes(b)
		return ev, nil
	})
}

func (r *RoomsDatabase) GetRoomCurrentExtremEventIDs(ctx context.Context, roomID id.RoomID) ([]id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.EventID, error) {
		return r.events.TxnLookupCurrentRoomExtremEventIDs(txn, roomID)
	})
}

func (r *RoomsDatabase) KeyForRoom(roomID id.RoomID) fdb.Key {
	return r.byID.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) KeyForRoomSuperStreamVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	if key, err := r.superStream.PackWithVersionstamp(tuple.Tuple{
		roomID.String(), version,
	}); err != nil {
		panic(err)
	} else {
		return key
	}
}

func (r *RoomsDatabase) KeyToRoomSuperStreamVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := r.superStream.Unpack(key)
	return tup[1].(tuple.Versionstamp)
}

func (r *RoomsDatabase) RangeForRoomSuperStream(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.superStream, fromVersion, toVersion, roomID.String())
}
