package receipts

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type ReceiptsDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// ReceiptTup (user, room, thread, type) to version, used to clear previous keys when new ones are appended.
	//
	// key: (UserID, RoomID, Type, ThreadID)
	// value: tuple.Versionstamp
	receiptTupToVersion subspace.Subspace

	// Sparse stream of public receipts stored by room/version for pagination
	//
	// key: (RoomID, Versionstamp)
	// value: types.Receipt
	roomVersionToReceipt subspace.Subspace

	// Sparse stream of public receipts sent by local users, stored by room/version
	//
	// key: (RoomID, Versionstamp)
	// value: types.Receipt
	localRoomVersionToReceipt subspace.Subspace

	// Sparse stream of private receipts send by local users, stored by user/version
	//
	// key: (UserID, Versionstamp)
	// value: types.Receipt
	userVersionToReceipt subspace.Subspace
}

func NewReceiptsDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ReceiptsDirectory {
	receiptsDir, err := parentDir.CreateOrOpen(db, []string{"receipts"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "receipts").Logger()
	log.Debug().
		Bytes("prefix", receiptsDir.Bytes()).
		Msg("Init rooms/receipts directory")

	return &ReceiptsDirectory{
		log: log,
		db:  db,

		receiptTupToVersion:       receiptsDir.Sub("rtv"),
		roomVersionToReceipt:      receiptsDir.Sub("prv"),
		localRoomVersionToReceipt: receiptsDir.Sub("lrv"),
		userVersionToReceipt:      receiptsDir.Sub("puv"),
	}
}

func (r *ReceiptsDirectory) KeyForReceiptVersion(rc *types.Receipt) fdb.Key {
	return r.receiptTupToVersion.Pack(tuple.Tuple{
		rc.UserID.String(),
		rc.RoomID.String(),
		string(rc.Type),
		rc.ThreadID,
	})
}

// Room version

func (r *ReceiptsDirectory) KeyToRoomVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := r.roomVersionToReceipt.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (r *ReceiptsDirectory) KeyForRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	tup := tuple.Tuple{roomID.String(), version}
	if types.IsIncompleteVersionstamp(version) {
		key, err := r.roomVersionToReceipt.PackWithVersionstamp(tup)
		if err != nil {
			panic(err)
		}
		return key
	}
	return r.roomVersionToReceipt.Pack(tup)
}

func (r *ReceiptsDirectory) RangeForRoomVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.roomVersionToReceipt, fromVersion, toVersion, roomID.String())
}

// Local room version

func (r *ReceiptsDirectory) KeyToLocalRoomVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := r.localRoomVersionToReceipt.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (r *ReceiptsDirectory) KeyForLocalRoomVersion(roomID id.RoomID, version tuple.Versionstamp) fdb.Key {
	tup := tuple.Tuple{roomID.String(), version}
	if types.IsIncompleteVersionstamp(version) {
		key, err := r.localRoomVersionToReceipt.PackWithVersionstamp(tuple.Tuple{roomID.String(), version})
		if err != nil {
			panic(err)
		}
		return key
	}
	return r.localRoomVersionToReceipt.Pack(tup)
}

func (r *ReceiptsDirectory) RangeForLocalRoomVersion(
	roomID id.RoomID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.localRoomVersionToReceipt, fromVersion, toVersion, roomID.String())
}

// User version

func (r *ReceiptsDirectory) KeyToUserVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := r.userVersionToReceipt.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (r *ReceiptsDirectory) KeyForUserVersion(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	tup := tuple.Tuple{userID.String(), version}
	if types.IsIncompleteVersionstamp(version) {
		key, err := r.userVersionToReceipt.PackWithVersionstamp(tuple.Tuple{userID.String(), version})
		if err != nil {
			panic(err)
		}
		return key
	}
	return r.userVersionToReceipt.Pack(tup)
}

func (r *ReceiptsDirectory) RangeForUserVersion(
	userID id.UserID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.Range {
	return types.GetVersionRange(r.userVersionToReceipt, fromVersion, toVersion, userID.String())
}
