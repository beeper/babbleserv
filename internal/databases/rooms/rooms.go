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
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exsync"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/receipts"
	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
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
	state    *state.StateDirectory

	// Room ID to bytes we decode to `*types.Room` items
	idToRoom subspace.Subspace

	// Room ID to depth int64
	idToDepth subspace.Subspace

	// Room ID to tuple.Versionstamp of the most recent event or receipt in the room
	idToVersion subspace.Subspace

	// Joined members of this server by room
	//
	// key: (RoomID, UserID)
	// value: types.MembershipTup
	localMembers subspace.Subspace

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

	// Published rooms ordered by descending joined-member count, then room ID.
	// key: (-MemberCount, RoomID); value: empty
	publishedRooms subspace.Subspace

	// (UserID, DeviceID, RoomID, Endpoint, TransactionID) -> EventID
	eventTransactions subspace.Subspace
	// (EventID, UserID, DeviceID) -> TransactionID
	eventTransactionIDs subspace.Subspace

	// Per-look lock used to serialize per-room DB writes, this is an optional optimization since
	// FDB will enforce serialization at the DB level.
	roomLocks *exsync.Map[id.RoomID, *sync.Mutex]
	// Serializes this process's remote joins to a room, see lockJoin
	joinLocks *exsync.Map[id.RoomID, chan struct{}]
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
		state:    state.NewStateDirectory(log, db, roomsDir),

		idToRoom:    roomsDir.Sub("id"),
		idToDepth:   roomsDir.Sub("idd"),
		idToVersion: roomsDir.Sub("iev"),

		localMembers: roomsDir.Sub("rlm"),

		aliasToID:      roomsDir.Sub("aid"),
		idAliases:      roomsDir.Sub("ida"),
		publishedRooms: roomsDir.Sub("pub"),

		eventTransactions:   roomsDir.Sub("etx"),
		eventTransactionIDs: roomsDir.Sub("eti"),

		roomLocks: exsync.NewMap[id.RoomID, *sync.Mutex](),
		joinLocks: exsync.NewMap[id.RoomID, chan struct{}](),
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

// lockRoom serializes this process's writes to a room until the returned func is called
func (r *RoomsDatabase) lockRoom(roomID id.RoomID) (unlock func()) {
	lock, _ := r.roomLocks.GetOrSet(roomID, &sync.Mutex{})
	lock.Lock()
	return lock.Unlock
}

// lockJoin serializes this process's remote joins to a room until the returned func is called, or
// fails once ctx ends while it waits.
func (r *RoomsDatabase) lockJoin(ctx context.Context, roomID id.RoomID) (unlock func(), err error) {
	lock, _ := r.joinLocks.GetOrSet(roomID, make(chan struct{}, 1))
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for another join to the room: %w", ctx.Err())
	}
}

func (r *RoomsDatabase) GetTimeForVersion(ctx context.Context, version tuple.Versionstamp) (*time.Time, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*time.Time, error) {
		time := util.TxnGetTimeForVersion(txn, version)
		return time, nil
	})
}

// GenerateRoomID returns the ID of the room a local create event creates. A domainless room ID is
// derived from the create event as SendLocalEvents stores it, so this fixes the create event's
// timestamp, including across retries of the send.
func (r *RoomsDatabase) GenerateRoomID(ctx context.Context, createEv *types.PartialEvent) (id.RoomID, error) {
	if roomVersion := createRoomVersion(createEv); util.RoomVersionHas(roomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) {
		return r.domainlessRoomID(createEv, roomVersion)
	}
	for {
		rid := util.GenerateRandomStringBase32Hex(16)
		roomID := id.RoomID("!" + rid + ":" + r.config.ServerName)

		// Extremely unlikely but check the room ID isn't taken, the chance of this is incredibly
		// small but nice to be sure.
		if existing, err := r.GetRoom(ctx, roomID); err != nil {
			return "", fmt.Errorf("failed to check for existing room: %w", err)
		} else if existing != nil {
			zerolog.Ctx(ctx).Warn().Stringer("room_id", roomID).Msg("Generated duplicate roomID!")
			continue
		}

		return roomID, nil
	}
}

func (r *RoomsDatabase) domainlessRoomID(createEv *types.PartialEvent, roomVersion string) (id.RoomID, error) {
	now := time.Now()
	if createEv.Timestamp == 0 {
		createEv.Timestamp = now.UnixMilli()
	}
	// The first event of a room has no depth or prev events
	create := newLocalEvent(createEv, roomVersion, 0, nil, now)
	keyID, key := r.config.MustGetActiveSigningKey()
	if err := util.HashAndSignEvent(create, r.config.ServerName, keyID, key); err != nil {
		return "", err
	}
	return create.DomainlessRoomID(), nil
}

func (r *RoomsDatabase) GetRoom(ctx context.Context, roomID id.RoomID) (*types.Room, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.Room, error) {
		return roomOrNil(r.txnGetRoom(txn, roomID))
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

func (r *RoomsDatabase) keyForLocalMember(roomID id.RoomID, userID id.UserID) fdb.Key {
	return r.localMembers.Pack(tuple.Tuple{roomID.String(), userID.String()})
}

func (r *RoomsDatabase) isLocalUser(userID id.UserID) bool {
	return userID.Homeserver() == r.config.ServerName
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

func (r *RoomsDatabase) keyForPublishedRoom(memberCount int, roomID id.RoomID) fdb.Key {
	return r.publishedRooms.Pack(tuple.Tuple{-int64(memberCount), roomID.String()})
}

func (r *RoomsDatabase) txnStoreRoom(txn fdb.Transaction, room *types.Room, previousMemberCount int) {
	if room.Public && room.MemberCount != previousMemberCount {
		txn.Clear(r.keyForPublishedRoom(previousMemberCount, room.ID))
		txn.Set(r.keyForPublishedRoom(room.MemberCount, room.ID), nil)
	}
	txn.Set(r.KeyForRoom(room.ID), room.ToMsgpack())
}
