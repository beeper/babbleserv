package accounts

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	remoteDeviceCacheReadConcurrency = 8
	remoteDeviceEnsureChunk          = 100
)

// EnsureRemoteDeviceCaches creates the caches of remote users a local client queried, returning
// those created. Callers check the querier shares an encrypted room with each user.
func (a *AccountsDatabase) EnsureRemoteDeviceCaches(ctx context.Context, userIDs []id.UserID) ([]id.UserID, error) {
	var created []id.UserID
	for chunk := range slices.Chunk(userIDs, remoteDeviceEnsureChunk) {
		chunkCreated, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) ([]id.UserID, error) {
			return a.remotedevices.TxnEnsureCaches(txn, chunk, time.Now().UTC())
		})
		if err != nil {
			return nil, err
		}
		created = append(created, chunkCreated...)
	}
	return created, nil
}

func (a *AccountsDatabase) EvictStaleRemoteDeviceCaches(
	ctx context.Context,
	after id.UserID,
	limit int,
) (evicted []id.UserID, next id.UserID, done bool, err error) {
	type evictedPage struct {
		evicted []id.UserID
		next    id.UserID
		done    bool
	}
	page, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (evictedPage, error) {
		evicted, next, done, err := a.remotedevices.TxnEvictStale(txn, after, time.Now().UTC(), limit)
		return evictedPage{evicted: evicted, next: next, done: done}, err
	})
	return page.evicted, page.next, page.done, err
}

func (a *AccountsDatabase) IngestRemoteDeviceUpdates(
	ctx context.Context,
	userID id.UserID,
	edus []types.RemoteDeviceEDU,
) (bool, error) {
	notified, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		notified, err := a.remotedevices.TxnIngest(txn, userID, edus, time.Now().UTC())
		if err == nil && notified {
			a.devices.TxnStoreDeviceChange(txn, userID, id.DeviceID("*"), tuple.IncompleteVersionstamp(0))
		}
		return notified, err
	})
	if err == nil && notified {
		a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{userID}})
	}
	return notified, err
}

func (a *AccountsDatabase) NextRemoteDeviceJobs(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]types.RemoteDeviceJob, error) {
	return util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) ([]types.RemoteDeviceJob, error) {
		return a.remotedevices.TxnNextJobs(txn, now, limit)
	})
}

func (a *AccountsDatabase) RescheduleRemoteDeviceJob(
	ctx context.Context,
	userID id.UserID,
	dueAt time.Time,
	checkWorkerLock func(fdb.Transaction),
) (bool, error) {
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		checkWorkerLock(txn)
		return a.remotedevices.TxnRescheduleJob(txn, userID, dueAt)
	})
}

func (a *AccountsDatabase) PublishRemoteDeviceSnapshot(
	ctx context.Context,
	job types.RemoteDeviceJob,
	snapshot *types.RemoteDeviceSnapshot,
	checkWorkerLock func(fdb.Transaction),
) (bool, error) {
	published, err := util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		checkWorkerLock(txn)
		published, err := a.remotedevices.TxnPublishSnapshot(txn, job, snapshot, time.Now().UTC())
		if err == nil && published {
			a.devices.TxnStoreDeviceChange(txn, job.UserID, id.DeviceID("*"), tuple.IncompleteVersionstamp(0))
		}
		return published, err
	})
	if err == nil && published {
		a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{job.UserID}})
	}
	return published, err
}

// GetRemoteDeviceCaches reads each user's cache in a transaction of its own, since a snapshot may
// hold up to MaxRemoteDevicesPerUser devices. Users without a valid cache are absent.
func (a *AccountsDatabase) GetRemoteDeviceCaches(
	ctx context.Context,
	userIDs []id.UserID,
) (map[id.UserID]*types.RemoteDeviceCache, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	caches := make(map[id.UserID]*types.RemoteDeviceCache, len(userIDs))
	running := make(chan struct{}, remoteDeviceCacheReadConcurrency)
	for _, userID := range userIDs {
		running <- struct{}{}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-running }()
			cache, err := util.DoReadTransaction(ctx, a.db, func(txn fdb.ReadTransaction) (*types.RemoteDeviceCache, error) {
				return a.remotedevices.TxnGetCache(txn, userID)
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
				cancel()
			} else if cache != nil {
				caches[userID] = cache
			}
		})
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return caches, nil
}
