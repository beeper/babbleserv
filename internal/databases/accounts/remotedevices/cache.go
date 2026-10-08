package remotedevices

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
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

	signingKeys := signingKeysFuture.mustGet()
	cache := &types.RemoteDeviceCache{
		UserID: userID,
		RemoteDeviceSnapshot: types.RemoteDeviceSnapshot{
			StreamID:       *m.StreamID,
			Devices:        make(map[id.DeviceID]types.RemoteDevice, len(kvs)),
			MasterKey:      json.RawMessage(signingKeys.MasterKey),
			SelfSigningKey: json.RawMessage(signingKeys.SelfSigningKey),
		},
	}
	for _, kv := range kvs {
		key, err := userDevices.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		cache.Devices[id.DeviceID(key[0].(string))] = *decodeDevice(kv.Value)
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
		m.WrittenAt = now.UnixMilli()
		m.Generation++
		r.txnEnsureJob(txn, userID, decodeJob(userID, jobFutures[i].MustGet()), now)
		r.txnWriteMeta(txn, userID, m)
		created = append(created, userID)
	}
	return created, nil
}

// TxnEvictStale evicts the caches nothing has written for RemoteDeviceCacheRetention, paging through
// users after the cursor, from the start when it is empty. Clients are not told: the remote user's
// keys did not change.
func (r *RemoteDevicesDirectory) TxnEvictStale(
	txn fdb.Transaction,
	after id.UserID,
	now time.Time,
	limit int,
) (evicted []id.UserID, next id.UserID, done bool, err error) {
	if limit < 1 {
		return nil, "", false, fmt.Errorf("remote device cache eviction needs a positive limit, got %d", limit)
	}
	begin, end := r.meta.FDBRangeKeys()
	page := fdb.KeyRange{Begin: begin, End: end}
	if after != "" {
		page.Begin = append(r.keyForMeta(after), 0x00)
	}
	// A snapshot read so ingest on the caches this page keeps does not conflict with it; each
	// eviction adds a conflict on its own row
	kvs, err := txn.Snapshot().GetRange(page, fdb.RangeOptions{
		Limit: limit,
		Mode:  fdb.StreamingModeWantAll,
	}).GetSliceWithError()
	if err != nil {
		return nil, "", false, err
	}

	type staleCache struct {
		userID id.UserID
		meta   meta
		job    fdb.FutureByteSlice
	}
	var stale []staleCache
	cutoff := now.Add(-types.RemoteDeviceCacheRetention).UnixMilli()
	next = after
	for _, kv := range kvs {
		key, err := r.meta.Unpack(kv.Key)
		if err != nil {
			return nil, "", false, err
		}
		userID := id.UserID(key[0].(string))
		next = userID
		m := decodeMeta(kv.Value)
		if !m.cached() || m.WrittenAt >= cutoff {
			continue
		}
		if err := txn.AddReadConflictKey(kv.Key); err != nil {
			return nil, "", false, err
		}
		stale = append(stale, staleCache{userID: userID, meta: m, job: txn.Get(r.keyForJob(userID))})
	}

	for _, cache := range stale {
		r.txnClearData(txn, cache.userID, &cache.meta)
		r.txnCancelJob(txn, decodeJob(cache.userID, cache.job.MustGet()))
		cache.meta.WrittenAt = 0
		cache.meta.Generation++
		r.txnWriteMeta(txn, cache.userID, cache.meta)
		evicted = append(evicted, cache.userID)
	}
	return evicted, next, len(kvs) < limit, nil
}
