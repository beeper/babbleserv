package presence

import (
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type PresenceDirectory struct {
	log zerolog.Logger

	// key: (id.UserID)
	// value: types.Presence
	userToPresence subspace.Subspace

	// key: (id.UserID)
	// value: time.Time
	userToLastActive subspace.Subspace

	// key: tuple.Versionstamp
	// value: (id.UserID, types.Presence)
	presenceChanges subspace.Subspace

	// key: (timeoutMS, id.UseriD)
	// value: []byte{}
	presenceTimeouts subspace.Subspace
}

func NewPresenceDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *PresenceDirectory {
	presenceDir, err := parentDir.CreateOrOpen(db, []string{"presence"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "presence").Logger()
	log.Debug().
		Bytes("prefix", presenceDir.Bytes()).
		Msg("Init transient/presence directory")

	return &PresenceDirectory{
		log: log,

		userToPresence:   presenceDir.Sub("utp"),
		userToLastActive: presenceDir.Sub("ula"),
		presenceChanges:  presenceDir.Sub("pch"),
		presenceTimeouts: presenceDir.Sub("ptm"),
	}
}

func (p *PresenceDirectory) keyForUserPresence(userID id.UserID) fdb.Key {
	return p.userToPresence.Pack(tuple.Tuple{userID.String()})
}

func (p *PresenceDirectory) keyForUserLastActive(userID id.UserID) fdb.Key {
	return p.userToLastActive.Pack(tuple.Tuple{userID.String()})
}

func (p *PresenceDirectory) keyForPresenceChange(version tuple.Versionstamp) fdb.Key {
	key, err := p.presenceChanges.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (p *PresenceDirectory) keyForPresenceTimeout(timeout time.Time, userID id.UserID) fdb.Key {
	return p.presenceTimeouts.Pack(tuple.Tuple{timeout.UnixMilli(), userID.String()})
}

func (p *PresenceDirectory) TxnGetPresence(txn fdb.ReadTransaction, userID id.UserID) *types.Presence {
	key := p.keyForUserPresence(userID)
	b := txn.Get(key).MustGet()
	if b == nil {
		return nil
	}
	return types.MustNewPresenceFromBytes(b, userID)
}

func (p *PresenceDirectory) TxnGetLastActive(txn fdb.ReadTransaction, userID id.UserID) *time.Time {
	key := p.keyForUserLastActive(userID)
	kv := txn.Get(key).MustGet()
	if kv == nil {
		return nil
	}

	tup, err := tuple.Unpack(kv)
	if err != nil {
		panic(err)
	}

	lastActive := time.Unix(0, tup[0].(int64)*int64(time.Millisecond))
	return &lastActive
}

func (p *PresenceDirectory) TxnStorePresence(
	txn fdb.Transaction,
	userID id.UserID,
	presence *types.Presence,
	timeout time.Time,
	checkMessage bool,
) bool {
	// Lookup any current presence
	currentPresence := p.TxnGetPresence(txn, userID)

	// Get current last active time
	now := time.Now().UTC()

	// Check if presence has changed
	hasChanged := currentPresence == nil || currentPresence.Presence != presence.Presence
	if checkMessage && currentPresence != nil && currentPresence.Message != presence.Message {
		hasChanged = true
	}

	if hasChanged {
		presence.LastActive = now
		txn.SetVersionstampedKey(
			p.keyForPresenceChange(tuple.IncompleteVersionstamp(0)),
			tuple.Tuple{userID.String(), presence.ToBytes()}.Pack(),
		)

		txn.Set(p.keyForUserPresence(userID), presence.ToBytes())

		if !timeout.Equal(time.Time{}) {
			p.TxnStorePresenceTimeout(txn, timeout, userID)
		}
	}

	// Always bump userToLastActive
	txn.Set(p.keyForUserLastActive(userID), tuple.Tuple{now.UnixMilli()}.Pack())
	return hasChanged
}

// TxnClearPresenceChanges removes presence changes from zero through to and including toVersion
func (p *PresenceDirectory) TxnClearPresenceChanges(txn fdb.Transaction, toVersion tuple.Versionstamp) {
	txn.ClearRange(types.GetVersionRange(p.presenceChanges, types.ZeroVersionstamp, toVersion))
}

func (p *PresenceDirectory) TxnPaginatePresenceChanges(
	txn fdb.ReadTransaction,
	options types.PaginationOptions,
) []*types.PresenceChange {
	iter := txn.GetRange(
		types.GetVersionRange(p.presenceChanges, options.From, options.To),
		options.RangeOptions(),
	).Iterator()

	changes := make([]*types.PresenceChange, 0, options.Limit)

	for iter.Advance() {
		kv := iter.MustGet()

		// Extract version from key
		keyTup, err := p.presenceChanges.Unpack(kv.Key)
		if err != nil {
			panic(err)
		}
		version := keyTup[0].(tuple.Versionstamp)

		// Extract presence from value
		tup, err := tuple.Unpack(kv.Value)
		if err != nil {
			panic(err)
		}
		userID := id.UserID(tup[0].(string))
		presence := types.MustNewPresenceFromBytes(tup[1].([]byte), userID)

		changes = append(changes, &types.PresenceChange{
			Presence: *presence,
			Version:  version,
		})
	}

	return changes
}

func (p *PresenceDirectory) TxnStorePresenceTimeout(txn fdb.Transaction, timeout time.Time, userID id.UserID) {
	txn.Set(p.keyForPresenceTimeout(timeout, userID), []byte{})
}

func (p *PresenceDirectory) rangeForPresenceTimeouts(toTimeout time.Time) fdb.KeyRange {
	return fdb.KeyRange{
		Begin: fdb.Key(append(p.presenceTimeouts.Bytes(), byte(0x00))),
		End:   fdb.Key(append(p.presenceTimeouts.Pack(tuple.Tuple{toTimeout.UnixMilli()}), byte(0xff))),
	}
}

func (p *PresenceDirectory) TxnClearPresenceTimeouts(txn fdb.Transaction, toTimeout time.Time) {
	txn.ClearRange(p.rangeForPresenceTimeouts(toTimeout))
}

// Paginates presence timeout checkers, returning a list of userIDs to check. Expected that these
// will be cleared via TxnClearPresenceTimeouts once handled.
func (p *PresenceDirectory) TxnGetPresenceTimeouts(txn fdb.ReadTransaction, toTimeout time.Time) []id.UserID {
	kvs := txn.GetRange(p.rangeForPresenceTimeouts(toTimeout), fdb.RangeOptions{
		Mode: fdb.StreamingModeWantAll,
	}).GetSliceOrPanic()

	// Collect unique UserIDs to check
	userIDs := make([]id.UserID, len(kvs))
	for i, kv := range kvs {
		tup, _ := p.presenceTimeouts.Unpack(kv.Key)
		userIDs[i] = id.UserID(tup[1].(string))
	}

	return userIDs
}
