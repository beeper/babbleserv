package keybackup

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type KeyBackupDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// Backup version storage
	// key: (id.UserID, tuple.Versionstamp)
	// value: types.KeyBackupVersion msgpack bytes
	userVersions subspace.Subspace

	// Backup version metadata (count and etag tracking)
	// key: (id.UserID, backup-versionstamp)
	// value: tuple{count, updatedVersionstamp}
	userVersionsMeta subspace.Subspace

	// Key storage
	// key: (id.UserID, backup-versionstamp, roomID, sessionID)
	// value: types.KeyBackupData msgpack bytes
	keys subspace.Subspace
}

func NewKeyBackupDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *KeyBackupDirectory {
	keyBackupDir, err := parentDir.CreateOrOpen(db, []string{"keybackup"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "keybackup").Logger()
	log.Debug().
		Bytes("prefix", keyBackupDir.Bytes()).
		Msg("Init accounts/keybackup directory")

	return &KeyBackupDirectory{
		log: log,
		db:  db,

		userVersions:     keyBackupDir.Sub("uv"),
		userVersionsMeta: keyBackupDir.Sub("uvm"),
		keys:             keyBackupDir.Sub("k"),
	}
}

func (d *KeyBackupDirectory) keyForVersion(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	return types.MustPackVersionKey(d.userVersions, tuple.Tuple{userID.String(), version})
}

func (d *KeyBackupDirectory) keyForVersionMeta(userID id.UserID, version tuple.Versionstamp) fdb.Key {
	return d.userVersionsMeta.Pack(tuple.Tuple{userID.String(), version})
}

func (d *KeyBackupDirectory) keyForKey(userID id.UserID, backupVersion tuple.Versionstamp, roomID, sessionID string) fdb.Key {
	return d.keys.Pack(tuple.Tuple{userID.String(), backupVersion, roomID, sessionID})
}

func (d *KeyBackupDirectory) rangeForUserVersions(userID id.UserID) fdb.ExactRange {
	return d.userVersions.Sub(userID.String())
}

func (d *KeyBackupDirectory) rangeForBackupKeys(userID id.UserID, backupVersion tuple.Versionstamp) fdb.ExactRange {
	return d.keys.Sub(userID.String(), backupVersion)
}

func (d *KeyBackupDirectory) rangeForRoomKeys(userID id.UserID, backupVersion tuple.Versionstamp, roomID string) fdb.ExactRange {
	return d.keys.Sub(userID.String(), backupVersion, roomID)
}

func (d *KeyBackupDirectory) TxnStoreVersion(txn fdb.Transaction, userID id.UserID, version *types.KeyBackupVersion) tuple.Versionstamp {
	vstamp := tuple.IncompleteVersionstamp(0)
	key := d.keyForVersion(userID, vstamp)
	txn.SetVersionstampedKey(key, version.ToBytes())

	// Initialize the metadata with count 0 and new etag
	d.txnInitVersionMeta(txn, userID, vstamp)

	return vstamp
}

func (d *KeyBackupDirectory) TxnGetLatestVersion(txn fdb.ReadTransaction, userID id.UserID) (*types.KeyBackupVersion, tuple.Versionstamp, error) {
	rng := d.rangeForUserVersions(userID)
	// Reverse iteration to get the latest version
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll, Reverse: true, Limit: 1}).Iterator()

	if !iter.Advance() {
		return nil, types.ZeroVersionstamp, nil
	}

	kv, err := iter.Get()
	if err != nil {
		return nil, types.ZeroVersionstamp, err
	}

	// Unpack the key to get the versionstamp
	tup, err := d.userVersions.Unpack(kv.Key)
	if err != nil {
		return nil, types.ZeroVersionstamp, err
	}
	vstamp := tup[1].(tuple.Versionstamp)

	version, err := types.NewKeyBackupVersionFromBytes(kv.Value)
	if err != nil {
		return nil, types.ZeroVersionstamp, err
	}

	return version, vstamp, nil
}

func (d *KeyBackupDirectory) TxnGetVersion(txn fdb.ReadTransaction, userID id.UserID, vstamp tuple.Versionstamp) (*types.KeyBackupVersion, error) {
	key := d.keyForVersion(userID, vstamp)
	value := txn.Get(key).MustGet()
	if value == nil {
		return nil, nil
	}
	return types.NewKeyBackupVersionFromBytes(value)
}

func (d *KeyBackupDirectory) TxnGetVersionWithMeta(txn fdb.ReadTransaction, userID id.UserID, vstamp tuple.Versionstamp) (*types.KeyBackupVersionWithMeta, error) {
	version, err := d.TxnGetVersion(txn, userID, vstamp)
	if err != nil || version == nil {
		return nil, err
	}

	count, etag := d.TxnGetVersionMeta(txn, userID, vstamp)

	return &types.KeyBackupVersionWithMeta{
		Version:   types.MustVersionstampToString(vstamp),
		Algorithm: version.Algorithm,
		AuthData:  version.AuthData,
		Count:     count,
		ETag:      etag,
	}, nil
}

func (d *KeyBackupDirectory) TxnUpdateVersionAuthData(txn fdb.Transaction, userID id.UserID, vstamp tuple.Versionstamp, version *types.KeyBackupVersion) error {
	key := d.keyForVersion(userID, vstamp)

	// Check if version exists
	existing := txn.Get(key).MustGet()
	if existing == nil {
		return types.ErrKeyBackupNotFound
	}

	metadata, err := types.NewKeyBackupVersionFromBytes(existing)
	if err != nil {
		return err
	}
	if metadata.Algorithm != version.Algorithm {
		return types.ErrKeyBackupAlgorithmMismatch
	}
	txn.Set(key, version.ToBytes())
	return nil
}

func (d *KeyBackupDirectory) TxnDeleteVersion(txn fdb.Transaction, userID id.UserID, vstamp tuple.Versionstamp) {
	// Delete the version itself
	txn.Clear(d.keyForVersion(userID, vstamp))
	// Delete the metadata
	txn.Clear(d.keyForVersionMeta(userID, vstamp))
	// Delete all keys for this version
	txn.ClearRange(d.rangeForBackupKeys(userID, vstamp))
}

func (d *KeyBackupDirectory) TxnStoreKey(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp, roomID, sessionID string, data *types.KeyBackupData) (bool, int) {
	key := d.keyForKey(userID, backupVersion, roomID, sessionID)

	// Check if key exists and compare first_message_index
	existing := txn.Get(key).MustGet()
	if existing != nil {
		existingData := types.MustNewKeyBackupDataFromBytes(existing)
		if !shouldReplaceKey(existingData, data) {
			return false, 0
		}
	}

	txn.Set(key, data.ToBytes())
	if existing == nil {
		return true, 1
	}
	return true, 0
}

func (d *KeyBackupDirectory) TxnGetKey(txn fdb.ReadTransaction, userID id.UserID, backupVersion tuple.Versionstamp, roomID, sessionID string) (*types.KeyBackupData, error) {
	key := d.keyForKey(userID, backupVersion, roomID, sessionID)
	value := txn.Get(key).MustGet()
	if value == nil {
		return nil, nil
	}
	return types.NewKeyBackupDataFromBytes(value)
}

func (d *KeyBackupDirectory) TxnGetKeysForRoom(txn fdb.ReadTransaction, userID id.UserID, backupVersion tuple.Versionstamp, roomID string) (map[string]*types.KeyBackupData, error) {
	rng := d.rangeForRoomKeys(userID, backupVersion, roomID)
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()

	sessions := make(map[string]*types.KeyBackupData)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// Unpack key to get sessionID
		tup, err := d.keys.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		sessionID := tup[3].(string)

		data, err := types.NewKeyBackupDataFromBytes(kv.Value)
		if err != nil {
			return nil, err
		}
		sessions[sessionID] = data
	}

	return sessions, nil
}

// TxnGetAllKeys returns all session keys for a backup version
func (d *KeyBackupDirectory) TxnGetAllKeys(txn fdb.ReadTransaction, userID id.UserID, backupVersion tuple.Versionstamp) (map[string]map[string]*types.KeyBackupData, error) {
	rng := d.rangeForBackupKeys(userID, backupVersion)
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()

	rooms := make(map[string]map[string]*types.KeyBackupData)
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return nil, err
		}

		// Unpack key to get roomID and sessionID
		tup, err := d.keys.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		roomID := tup[2].(string)
		sessionID := tup[3].(string)

		data, err := types.NewKeyBackupDataFromBytes(kv.Value)
		if err != nil {
			return nil, err
		}

		if rooms[roomID] == nil {
			rooms[roomID] = make(map[string]*types.KeyBackupData)
		}
		rooms[roomID][sessionID] = data
	}

	return rooms, nil
}

func (d *KeyBackupDirectory) TxnDeleteKey(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp, roomID, sessionID string) bool {
	key := d.keyForKey(userID, backupVersion, roomID, sessionID)

	// Check if key exists
	existing := txn.Get(key).MustGet()
	if existing == nil {
		return false
	}

	txn.Clear(key)
	return true
}

func (d *KeyBackupDirectory) TxnDeleteKeysForRoom(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp, roomID string) int {
	rng := d.rangeForRoomKeys(userID, backupVersion, roomID)

	// Count keys before deleting
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	count := 0
	for iter.Advance() {
		if _, err := iter.Get(); err == nil {
			count++
		}
	}

	txn.ClearRange(rng)
	return count
}

func (d *KeyBackupDirectory) TxnDeleteAllKeys(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp) int {
	rng := d.rangeForBackupKeys(userID, backupVersion)

	// Count keys before deleting
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	count := 0
	for iter.Advance() {
		if _, err := iter.Get(); err == nil {
			count++
		}
	}

	txn.ClearRange(rng)
	return count
}

func (d *KeyBackupDirectory) txnInitVersionMeta(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp) {
	// We use the backup versionstamp as the key, so we need to use SetVersionstampedKey
	key := types.MustPackVersionKey(d.userVersionsMeta, tuple.Tuple{userID.String(), backupVersion})

	// Store count as 0 and a new etag versionstamp
	etag := tuple.IncompleteVersionstamp(1)
	value, err := tuple.Tuple{0, etag}.PackWithVersionstamp(nil)
	if err != nil {
		panic(err)
	}
	txn.SetVersionstampedKey(key, value)
}

func (d *KeyBackupDirectory) TxnGetVersionMeta(txn fdb.ReadTransaction, userID id.UserID, backupVersion tuple.Versionstamp) (int, string) {
	key := d.keyForVersionMeta(userID, backupVersion)
	value := txn.Get(key).MustGet()
	if value == nil {
		return 0, ""
	}

	tup, err := tuple.Unpack(value)
	if err != nil {
		return 0, ""
	}

	count := int(tup[0].(int64))
	etag := types.MustVersionstampToString(tup[1].(tuple.Versionstamp))
	return count, etag
}

func (d *KeyBackupDirectory) TxnBumpVersionMeta(txn fdb.Transaction, userID id.UserID, backupVersion tuple.Versionstamp, countDelta int) {
	key := d.keyForVersionMeta(userID, backupVersion)

	// Read current count
	currentValue := txn.Get(key).MustGet()
	currentCount := 0
	if currentValue != nil {
		tup, err := tuple.Unpack(currentValue)
		if err == nil && len(tup) > 0 {
			currentCount = int(tup[0].(int64))
		}
	}

	newCount := max(currentCount+countDelta, 0)

	// Store new count with new etag versionstamp
	etag := tuple.IncompleteVersionstamp(0)
	value, err := tuple.Tuple{newCount, etag}.PackWithVersionstamp(nil)
	if err != nil {
		panic(err)
	}
	txn.SetVersionstampedValue(key, value)
}

func (d *KeyBackupDirectory) TxnGetVersionCount(txn fdb.ReadTransaction, userID id.UserID, backupVersion tuple.Versionstamp) int {
	count, _ := d.TxnGetVersionMeta(txn, userID, backupVersion)
	return count
}

func shouldReplaceKey(existing, incoming *types.KeyBackupData) bool {
	if existing.IsVerified != incoming.IsVerified {
		return incoming.IsVerified
	}
	if existing.FirstMessageIndex != incoming.FirstMessageIndex {
		return incoming.FirstMessageIndex < existing.FirstMessageIndex
	}
	return incoming.ForwardedCount < existing.ForwardedCount
}
