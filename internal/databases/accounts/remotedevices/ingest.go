package remotedevices

import (
	"bytes"
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type acceptance struct {
	at  time.Time
	seq int64
}

// TxnPublishSnapshot stores a fetched snapshot unless the generation moved since the job was read
func (r *RemoteDevicesDirectory) TxnPublishSnapshot(
	txn fdb.Transaction,
	job types.RemoteDeviceJob,
	snapshot *types.RemoteDeviceSnapshot,
	now time.Time,
) (bool, error) {
	userID := job.UserID
	if err := r.checkRemote(userID); err != nil {
		return false, err
	}
	metaFuture := txn.Get(r.keyForMeta(userID))
	jobFuture := txn.Get(r.keyForJob(userID))
	m := decodeMeta(metaFuture.MustGet())
	if m.Generation != job.Generation {
		return false, nil
	}

	txn.ClearRange(r.rangeForDevices(userID))
	txn.ClearRange(r.rangeForSigningKeys(userID))
	txn.ClearRange(r.rangeForStreamIDs(userID))

	for deviceID, device := range snapshot.Devices {
		r.txnStoreDevice(txn, userID, deviceID, device)
	}
	r.txnStoreSigningKeys(txn, userID, storedSigningKeys{
		MasterKey:      snapshot.MasterKey,
		SelfSigningKey: snapshot.SelfSigningKey,
	})
	r.txnStoreStreamID(txn, userID, &m, snapshot.StreamID, now)
	r.txnCancelJob(txn, decodeJob(userID, jobFuture.MustGet()))

	streamID := snapshot.StreamID
	m.StreamID = &streamID
	m.DeviceCount = len(snapshot.Devices)
	m.WrittenAt = now.UnixMilli()
	m.Generation++
	r.txnWriteMeta(txn, userID, m)
	return true, nil
}

// TxnIngest applies a user's device-list and signing-key EDUs in received order
func (r *RemoteDevicesDirectory) TxnIngest(
	txn fdb.Transaction,
	userID id.UserID,
	edus []types.RemoteDeviceEDU,
	now time.Time,
) (bool, error) {
	if err := r.checkRemote(userID); err != nil {
		return false, err
	}

	metaFuture := txn.Get(r.keyForMeta(userID))
	jobFuture := txn.Get(r.keyForJob(userID))
	signingKeysFuture := r.getSigningKeys(txn, userID)
	userStreamIDs := r.rangeForStreamIDs(userID)
	historyRange := txn.GetRange(userStreamIDs, fdb.RangeOptions{
		Limit: types.MaxRemoteDeviceStreamHistory + types.MaxRemoteDeviceEDUsPerIngest,
		Mode:  fdb.StreamingModeWantAll,
	})
	deviceFutures := make(map[id.DeviceID]fdb.FutureByteSlice)
	if len(edus) <= types.MaxRemoteDeviceEDUsPerIngest {
		for _, edu := range edus {
			// An over-long ID is decided invalid without its row, whose key could exceed the key limit
			if edu.DeviceList == nil || len(edu.DeviceList.DeviceID) > types.MaxRemoteDeviceIDBytes {
				continue
			}
			if _, ok := deviceFutures[edu.DeviceList.DeviceID]; !ok {
				deviceFutures[edu.DeviceList.DeviceID] = txn.Get(r.keyForDevice(userID, edu.DeviceList.DeviceID))
			}
		}
	}

	kvs, err := historyRange.GetSliceWithError()
	if err != nil {
		return false, err
	}
	history := make(map[int64]acceptance, len(kvs))
	for _, kv := range kvs {
		key, err := userStreamIDs.Unpack(kv.Key)
		if err != nil {
			return false, err
		}
		value, err := tuple.Unpack(kv.Value)
		if err != nil {
			return false, err
		}
		history[key[0].(int64)] = acceptance{
			at:  time.UnixMilli(value[0].(int64)).UTC(),
			seq: value[1].(int64),
		}
	}
	state := ingestState{
		meta:        decodeMeta(metaFuture.MustGet()),
		history:     history,
		devices:     make(map[id.DeviceID]*types.RemoteDevice, len(deviceFutures)),
		signingKeys: signingKeysFuture.mustGet(),
	}
	for deviceID, future := range deviceFutures {
		state.devices[deviceID] = decodeDevice(future.MustGet())
	}

	plan := planIngest(state, edus, now)
	notified := len(plan.devices) > 0 || plan.signingKeys != nil || (!state.meta.cached() && reportsUncachedChange(edus))

	m := state.meta
	if plan.wipe {
		r.txnClearData(txn, userID, &m)
	}
	if plan.refetch {
		m.Generation++
		r.txnEnsureJob(txn, userID, decodeJob(userID, jobFuture.MustGet()), now)
		r.txnWriteMeta(txn, userID, m)
		return notified, nil
	}

	for deviceID, device := range plan.devices {
		if device == nil {
			txn.Clear(r.keyForDevice(userID, deviceID))
		} else {
			r.txnStoreDevice(txn, userID, deviceID, *device)
		}
	}
	if plan.signingKeys != nil {
		r.txnStoreSigningKeys(txn, userID, *plan.signingKeys)
	}
	if len(plan.accepted) == 0 {
		return notified, nil
	}
	for _, streamID := range plan.accepted {
		history[streamID] = r.txnStoreStreamID(txn, userID, &m, streamID, now)
	}
	for _, streamID := range historyToPrune(history, now) {
		txn.Clear(r.keyForStreamID(userID, streamID))
	}
	lastID := plan.accepted[len(plan.accepted)-1]
	m.StreamID = &lastID
	m.DeviceCount = plan.deviceCount
	m.WrittenAt = now.UnixMilli()
	r.txnWriteMeta(txn, userID, m)
	return notified, nil
}

type ingestState struct {
	meta meta
	// Accepted stream IDs as stored, including those due to be pruned
	history map[int64]acceptance
	// Stored device per ID the batch names, nil when absent
	devices     map[id.DeviceID]*types.RemoteDevice
	signingKeys storedSigningKeys
}

type ingestPlan struct {
	// Clear the cache: the batch's accepted updates are discarded with it and a refetch follows
	wipe bool
	// Move the generation and ensure a fetch: deltas cannot apply without a snapshot, so a fetch in
	// flight is retried
	refetch bool
	// In order, the last becomes the current stream ID
	accepted []int64
	// Device writes, nil deletes
	devices     map[id.DeviceID]*types.RemoteDevice
	deviceCount int
	signingKeys *storedSigningKeys
}

func planIngest(state ingestState, edus []types.RemoteDeviceEDU, now time.Time) ingestPlan {
	wipeAndRefetch := ingestPlan{wipe: true, refetch: true}
	switch {
	case len(edus) == 0, !state.meta.cached():
		return ingestPlan{}
	case !state.meta.hasSnapshot():
		return ingestPlan{refetch: true}
	case len(edus) > types.MaxRemoteDeviceEDUsPerIngest:
		return wipeAndRefetch
	}

	// An accepted stream ID past retention or the count cap is unknown even before it is pruned
	known := make(map[int64]struct{}, len(state.history)+1)
	for streamID := range state.history {
		known[streamID] = struct{}{}
	}
	for _, streamID := range historyToPrune(state.history, now) {
		delete(known, streamID)
	}
	known[*state.meta.StreamID] = struct{}{}
	plan := ingestPlan{
		devices:     make(map[id.DeviceID]*types.RemoteDevice),
		deviceCount: state.meta.DeviceCount,
	}
	signingKeys := state.signingKeys

	for _, edu := range edus {
		if keys := edu.SigningKeys; keys != nil {
			if keys.Invalid {
				return wipeAndRefetch
			}
			// Only the supplied keys are replaced: an omitted key is never a deletion
			if keys.MasterKey != nil && !bytes.Equal(keys.MasterKey, signingKeys.MasterKey) {
				signingKeys.MasterKey = keys.MasterKey
				plan.signingKeys = &signingKeys
			}
			if keys.SelfSigningKey != nil && !bytes.Equal(keys.SelfSigningKey, signingKeys.SelfSigningKey) {
				signingKeys.SelfSigningKey = keys.SelfSigningKey
				plan.signingKeys = &signingKeys
			}
			continue
		}
		update := edu.DeviceList
		if update == nil {
			continue
		}

		// Continuity is decided before content: identical content never excuses an unknown predecessor
		if len(update.PrevIDs) == 0 {
			return wipeAndRefetch
		}
		if _, replay := known[update.StreamID]; replay {
			continue
		}
		if len(update.PrevIDs) > types.MaxRemoteDevicePrevIDs {
			return wipeAndRefetch
		}
		for _, prevID := range update.PrevIDs {
			if _, ok := known[prevID]; !ok {
				return wipeAndRefetch
			}
		}
		if update.Invalid || len(update.DeviceID) > types.MaxRemoteDeviceIDBytes {
			return wipeAndRefetch
		}

		current, written := plan.devices[update.DeviceID]
		if !written {
			current = state.devices[update.DeviceID]
		}
		switch {
		case update.Deleted && current == nil:
		case update.Deleted:
			plan.devices[update.DeviceID] = nil
			plan.deviceCount--
		case current == nil && plan.deviceCount >= types.MaxRemoteDevicesPerUser:
			return wipeAndRefetch
		case current != nil && current.DisplayName == update.Device.DisplayName && bytes.Equal(current.Keys, update.Device.Keys):
		default:
			if current == nil {
				plan.deviceCount++
			}
			plan.devices[update.DeviceID] = &types.RemoteDevice{DisplayName: update.Device.DisplayName, Keys: update.Device.Keys}
		}
		known[update.StreamID] = struct{}{}
		plan.accepted = append(plan.accepted, update.StreamID)
	}
	return plan
}

// reportsUncachedChange is whether EDUs for a user without a cache tell local clients of a change. A
// device-list update without prev IDs starts the stream to this server, as when the user joins a
// room shared with it, and local members already learn of a joiner from the join itself.
func reportsUncachedChange(edus []types.RemoteDeviceEDU) bool {
	for _, edu := range edus {
		if edu.SigningKeys != nil || (edu.DeviceList != nil && len(edu.DeviceList.PrevIDs) > 0) {
			return true
		}
	}
	return false
}

// historyToPrune returns the stream IDs accepted up to the last expired one, then the oldest beyond
// the per-user cap, in acceptance order. Keeping a suffix of acceptance order means a pruned ID's
// earlier-accepted predecessors go with it, so a replay of it can never connect and roll back.
// Pruning can only cause an extra resync, never acceptance of an unknown dependency.
func historyToPrune(history map[int64]acceptance, now time.Time) []int64 {
	cutoff := now.Add(-types.RemoteDeviceStreamHistoryRetention)
	oldestFirst := slices.SortedFunc(maps.Keys(history), func(a, b int64) int {
		return cmp.Compare(history[a].seq, history[b].seq)
	})
	prune := max(len(oldestFirst)-types.MaxRemoteDeviceStreamHistory, 0)
	for i, streamID := range oldestFirst {
		if history[streamID].at.Before(cutoff) {
			prune = max(prune, i+1)
		}
	}
	return oldestFirst[:prune]
}
