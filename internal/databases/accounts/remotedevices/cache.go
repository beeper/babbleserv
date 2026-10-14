package remotedevices

import (
	"fmt"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// TxnGetCache returns the user's cache, nil unless it is cached with a valid snapshot
func (r *RemoteDevicesDirectory) TxnGetCache(txn fdb.ReadTransaction, userID id.UserID) (*types.RemoteDeviceCache, error) {
	if err := r.checkRemote(userID); err != nil {
		return nil, err
	}

	metaFuture := txn.Get(r.keyForMeta(userID))
	userDevices := r.rangeForDevices(userID)
	devicesRange := txn.GetRange(userDevices, fdb.RangeOptions{
		Limit: types.MaxRemoteDevicesPerUser + 1,
		Mode:  fdb.StreamingModeWantAll,
	})
	signingKeysFuture := r.getSigningKeys(txn, userID)

	m := decodeMeta(metaFuture.MustGet())
	if !m.hasSnapshot() {
		return nil, nil
	}
	kvs, err := devicesRange.GetSliceWithError()
	if err != nil {
		return nil, err
	} else if len(kvs) > types.MaxRemoteDevicesPerUser {
		r.log.Warn().
			Str("user_id", userID.String()).
			Msg("Remote device cache exceeds the device limit, treating as a miss")
		return nil, nil
	}

	cache, err := decodeCache(userID, *m.StreamID, userDevices, kvs, signingKeysFuture.mustGet())
	if err != nil {
		// Keys were validated before they were stored, so this cache is corrupt: the user is queried live
		r.log.Warn().
			Err(err).
			Str("user_id", userID.String()).
			Msg("Remote device cache keys do not parse, treating as a miss")
		return nil, nil
	}
	return cache, nil
}

func decodeCache(
	userID id.UserID,
	streamID int64,
	userDevices subspace.Subspace,
	kvs []fdb.KeyValue,
	signingKeys storedSigningKeys,
) (*types.RemoteDeviceCache, error) {
	cache := &types.RemoteDeviceCache{
		UserID: userID,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{
			StreamID: streamID,
			Devices:  make(map[id.DeviceID]types.RemoteDevice, len(kvs)),
		},
	}
	var err error
	if cache.MasterKey, err = types.ParseSignedJSON[mautrix.CrossSigningKeys](signingKeys.MasterKey); err != nil {
		return nil, err
	}
	if cache.SelfSigningKey, err = types.ParseSignedJSON[mautrix.CrossSigningKeys](signingKeys.SelfSigningKey); err != nil {
		return nil, err
	}
	for _, kv := range kvs {
		key, err := userDevices.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		device, err := decodeDevice(kv.Value)
		if err != nil {
			return nil, err
		}
		cache.Devices[id.DeviceID(key[0].(string))] = *device
	}
	return cache, nil
}

// TxnEnsureCaches creates the caches of remote users a local client queried, scheduling their first
// fetch
func (r *RemoteDevicesDirectory) TxnEnsureCaches(
	txn fdb.Transaction,
	userIDs []id.UserID,
	now time.Time,
) (created []id.UserID, err error) {
	userIDs = slices.Compact(slices.Sorted(slices.Values(userIDs)))
	// Snapshot reads so a query of many users conflicts only with writes to the users it changes
	snapshot := txn.Snapshot()
	metaFutures := make([]fdb.FutureByteSlice, len(userIDs))
	jobFutures := make([]fdb.FutureByteSlice, len(userIDs))
	for i, userID := range userIDs {
		if err := r.checkRemote(userID); err != nil {
			return nil, err
		}
		metaFutures[i] = snapshot.Get(r.keyForMeta(userID))
		jobFutures[i] = snapshot.Get(r.keyForJob(userID))
	}

	for i, userID := range userIDs {
		m := decodeMeta(metaFutures[i].MustGet())
		if m.cached() {
			continue
		}
		if err := txn.AddReadConflictKey(r.keyForMeta(userID)); err != nil {
			return nil, err
		} else if err := txn.AddReadConflictKey(r.keyForJob(userID)); err != nil {
			return nil, err
		}
		// The generation bump keeps a fetch from an evicted cache from publishing into this one
		r.txnClearData(txn, userID, &m)
		r.txnSetWrittenAt(txn, userID, &m, now.UnixMilli())
		m.Generation++
		r.txnEnsureJob(txn, userID, decodeJob(userID, jobFutures[i].MustGet()), now)
		r.txnWriteMeta(txn, userID, m)
		created = append(created, userID)
	}
	return created, nil
}

// TxnEvictStale evicts a page of the caches nothing has written for RemoteDeviceCacheRetention,
// least recently written first, and reports whether the page came back short. Clients are not told:
// the remote user's keys did not change.
func (r *RemoteDevicesDirectory) TxnEvictStale(
	txn fdb.Transaction,
	now time.Time,
	limit int,
) (evicted []id.UserID, done bool, err error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("remote device cache eviction needs a positive limit, got %d", limit)
	}
	cutoff := now.Add(-types.RemoteDeviceCacheRetention).UnixMilli()
	begin, _ := r.cachesByWrittenAt.FDBRangeKeys()
	end := r.cachesByWrittenAt.Pack(tuple.Tuple{cutoff})
	// A snapshot read since each eviction conflicts on the meta row it reads
	kvs, err := txn.Snapshot().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{
		Limit: limit,
		Mode:  fdb.StreamingModeWantAll,
	}).GetSliceWithError()
	if err != nil {
		return nil, false, err
	}

	userIDs := make([]id.UserID, 0, len(kvs))
	metaFutures := make([]fdb.FutureByteSlice, 0, len(kvs))
	jobFutures := make([]fdb.FutureByteSlice, 0, len(kvs))
	for _, kv := range kvs {
		key, err := r.cachesByWrittenAt.Unpack(kv.Key)
		if err != nil {
			return nil, false, err
		}
		userID := id.UserID(key[1].(string))
		userIDs = append(userIDs, userID)
		metaFutures = append(metaFutures, txn.Get(r.keyForMeta(userID)))
		jobFutures = append(jobFutures, txn.Get(r.keyForJob(userID)))
	}

	for i, userID := range userIDs {
		m := decodeMeta(metaFutures[i].MustGet())
		if !m.cached() || m.WrittenAt >= cutoff {
			// An orphaned entry would otherwise fill every page and the sweep never end
			txn.Clear(kvs[i].Key)
			continue
		}
		r.txnEvict(txn, userID, m, decodeJob(userID, jobFutures[i].MustGet()))
		evicted = append(evicted, userID)
	}
	return evicted, len(kvs) < limit, nil
}

// TxnEvict evicts the caches of those of the users that are cached, returning them
func (r *RemoteDevicesDirectory) TxnEvict(txn fdb.Transaction, userIDs []id.UserID) ([]id.UserID, error) {
	metaFutures := make([]fdb.FutureByteSlice, len(userIDs))
	jobFutures := make([]fdb.FutureByteSlice, len(userIDs))
	for i, userID := range userIDs {
		if err := r.checkRemote(userID); err != nil {
			return nil, err
		}
		metaFutures[i] = txn.Get(r.keyForMeta(userID))
		jobFutures[i] = txn.Get(r.keyForJob(userID))
	}
	var evicted []id.UserID
	for i, userID := range userIDs {
		m := decodeMeta(metaFutures[i].MustGet())
		if !m.cached() {
			continue
		}
		r.txnEvict(txn, userID, m, decodeJob(userID, jobFutures[i].MustGet()))
		evicted = append(evicted, userID)
	}
	return evicted, nil
}

func (r *RemoteDevicesDirectory) txnEvict(txn fdb.Transaction, userID id.UserID, m meta, job *types.RemoteDeviceJob) {
	r.txnClearData(txn, userID, &m)
	r.txnCancelJob(txn, job)
	r.txnSetWrittenAt(txn, userID, &m, 0)
	m.Generation++
	r.txnWriteMeta(txn, userID, m)
}
