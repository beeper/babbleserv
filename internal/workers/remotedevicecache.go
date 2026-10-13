package workers

import (
	"context"
	"fmt"
	"math/bits"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	remoteDeviceCacheLockName      = "RemoteDeviceCacheWorkerLock"
	remoteDeviceCacheLockRetry     = 5 * time.Second
	remoteDeviceCacheLockTimeout   = time.Minute
	remoteDeviceCacheLockRefresh   = 15 * time.Second
	remoteDeviceCachePollInterval  = 2 * time.Second
	remoteDeviceCacheEvictInterval = time.Hour
	remoteDeviceCacheEvictPageSize = 500
	remoteDeviceCacheConcurrency   = 8
	// Due jobs of users in flight are read too, so twice the slots always yields enough to fill them
	remoteDeviceCacheBatchSize     = 2 * remoteDeviceCacheConcurrency
	remoteDeviceCacheLookupTimeout = 20 * time.Second
	remoteDeviceCacheRetryBase     = 30 * time.Second
	remoteDeviceCacheRetryCap      = 6 * time.Hour
)

// RemoteDeviceCacheWorker fetches a device list snapshot for each remote user whose cache is waiting
// for one and publishes it to the Accounts cache, retrying failed fetches with backoff. It also evicts
// the caches of users no longer sharing an encrypted room instead of fetching, and those nothing has
// written to for types.RemoteDeviceCacheRetention.
type RemoteDeviceCacheWorker struct {
	iteratorWorker
	client *federation.Client
}

type remoteDeviceJobResult struct {
	userID   id.UserID
	panicked bool
}

func NewRemoteDeviceCacheWorker(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	fedClient *federation.Client,
) *RemoteDeviceCacheWorker {
	// Shares the transport and its resolution cache, with a size limit of its own
	client := *fedClient
	client.ResponseSizeLimit = types.MaxRemoteDeviceSnapshotBytes

	w := &RemoteDeviceCacheWorker{
		iteratorWorker: NewWorker(
			"RemoteDeviceCacheWorker", log, cfg, db, notifiers,
			remoteDeviceCacheLockName,
			remoteDeviceCacheLockRetry,
			remoteDeviceCacheLockTimeout,
		),
		client: &client,
	}
	w.handler = w.run
	return w
}

// Jobs are created by transactions that send no change notification, so the worker polls for due
// jobs
func (w *RemoteDeviceCacheWorker) run(workerLock lock.Lock) {
	jobsCtx, cancelJobs := context.WithCancel(w.ctx)
	var jobs sync.WaitGroup
	stopJobs := func() {
		cancelJobs()
		jobs.Wait()
	}
	defer stopJobs()

	inFlight := make(map[id.UserID]struct{}, remoteDeviceCacheConcurrency)
	finished := make(chan remoteDeviceJobResult, remoteDeviceCacheConcurrency)
	dispatch := func() {
		for _, job := range w.nextJobs(inFlight) {
			inFlight[job.UserID] = struct{}{}
			jobs.Go(func() {
				var panicErr error
				defer func() { finished <- remoteDeviceJobResult{userID: job.UserID, panicked: panicErr != nil} }()
				// Publishing and rescheduling panic once the lock is lost. Nothing above this
				// goroutine recovers, so the panic stops here and the run loop surfaces the lost lock.
				defer util.RecoverPanic(&w.log, &panicErr)
				w.processJob(jobsCtx, workerLock, job)
			})
		}
	}

	poll := time.NewTicker(remoteDeviceCachePollInterval)
	defer poll.Stop()
	refresh := time.NewTicker(remoteDeviceCacheLockRefresh)
	defer refresh.Stop()
	evict := time.NewTicker(remoteDeviceCacheEvictInterval)
	defer evict.Stop()

	dispatch()
	w.evictStaleCaches(workerLock)
	for {
		select {
		case <-w.ctx.Done():
			stopJobs()
			workerLock.Release()
			return
		case result := <-finished:
			delete(inFlight, result.userID)
			if result.panicked {
				// Panics here at once when the lock was lost, rather than refetching due jobs until
				// the next refresh. Any other panic leaves the job to the next poll.
				workerLock.Refresh()
				continue
			}
			dispatch()
		case <-poll.C:
			dispatch()
		case <-refresh.C:
			workerLock.Refresh()
		case <-evict.C:
			w.evictStaleCaches(workerLock)
		}
	}
}

func (w *RemoteDeviceCacheWorker) nextJobs(inFlight map[id.UserID]struct{}) []types.RemoteDeviceJob {
	free := remoteDeviceCacheConcurrency - len(inFlight)
	if free <= 0 {
		return nil
	}
	due, err := w.db.Accounts.NextRemoteDeviceJobs(w.ctx, time.Now().UTC(), remoteDeviceCacheBatchSize)
	if err != nil {
		w.log.Err(err).Msg("Failed to get due remote device cache jobs")
		return nil
	}
	// A skipped job stays due and is read again by the next poll, so skipping needs no write
	jobs := make([]types.RemoteDeviceJob, 0, free)
	for _, job := range due {
		if _, running := inFlight[job.UserID]; running {
			continue
		}
		if jobs = append(jobs, job); len(jobs) == free {
			break
		}
	}
	return jobs
}

func (w *RemoteDeviceCacheWorker) processJob(ctx context.Context, workerLock lock.Lock, job types.RemoteDeviceJob) {
	log := w.log.With().Stringer("user_id", job.UserID).Logger()

	err := w.refreshCache(ctx, log, workerLock, job)
	if err == nil || ctx.Err() != nil {
		return
	}

	delay := remoteDeviceRetryDelay(job.Attempts)
	if rescheduled, rescheduleErr := w.db.Accounts.RescheduleRemoteDeviceJob(
		ctx, job.UserID, time.Now().UTC().Add(delay), workerLock.Refresh,
	); rescheduleErr != nil {
		log.Err(rescheduleErr).AnErr("cause", err).Msg("Failed to reschedule remote device snapshot fetch")
	} else if !rescheduled {
		log.Debug().Err(err).Msg("Remote device snapshot fetch failed for an evicted cache")
	} else {
		log.Info().Err(err).
			Int("attempts", job.Attempts+1).
			Dur("retry_in", delay).
			Msg("Remote device snapshot fetch failed, will retry")
	}
}

// refreshCache publishes a fetched snapshot, or evicts the cache once the user shares no encrypted
// room with a local user. A leave evicted nothing if it was handled before a key query created the
// cache, but rooms read after creation show it; a later leave evicts the cache itself and moves the
// generation, so the publish is refused.
func (w *RemoteDeviceCacheWorker) refreshCache(
	ctx context.Context,
	log zerolog.Logger,
	workerLock lock.Lock,
	job types.RemoteDeviceJob,
) error {
	rooms, err := w.db.Rooms.GetUserJoinedMembershipsWithEncryption(ctx, job.UserID)
	if err != nil {
		return fmt.Errorf("failed to read shared encrypted rooms: %w", err)
	} else if len(rooms) == 0 {
		if _, err := w.db.Accounts.EvictRemoteDeviceCaches(ctx, []id.UserID{job.UserID}); err != nil {
			return fmt.Errorf("failed to evict unshared cache: %w", err)
		}
		log.Debug().Msg("Evicted remote device cache no longer shared")
		return nil
	}

	snapshot, err := w.fetchSnapshot(ctx, job.UserID)
	if err != nil {
		return err
	}
	published, err := w.db.Accounts.PublishRemoteDeviceSnapshot(ctx, job, snapshot, workerLock.Refresh)
	if err != nil {
		return fmt.Errorf("failed to publish snapshot: %w", err)
	}
	log.Debug().
		Int64("stream_id", snapshot.StreamID).
		Int("devices", len(snapshot.Devices)).
		Bool("published", published).
		Msg("Processed remote device snapshot")
	return nil
}

func (w *RemoteDeviceCacheWorker) fetchSnapshot(ctx context.Context, userID id.UserID) (*types.RemoteDeviceSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteDeviceCacheLookupTimeout)
	defer cancel()
	body, _, err := w.client.MakeFullRequest(ctx, federation.RequestParams{
		ServerName:   userID.Homeserver(),
		Method:       http.MethodGet,
		Path:         federation.URLPath{"v1", "user", "devices", userID.String()},
		Authenticate: true,
	})
	if err != nil {
		return nil, err
	}
	return util.ParseRemoteDeviceSnapshot(userID, body)
}

// evictStaleCaches refreshes the lock per page since a sweep can outlast a refresh interval, but
// passes no lock check to the evictions: each conflicts on the cache row it read as stale, so even a
// sweep that outlives the lock never evicts a cache written meanwhile
func (w *RemoteDeviceCacheWorker) evictStaleCaches(workerLock lock.Lock) {
	var total int
	for {
		workerLock.Refresh()
		evicted, done, err := w.db.Accounts.EvictStaleRemoteDeviceCaches(w.ctx, remoteDeviceCacheEvictPageSize)
		if err != nil {
			w.log.Err(err).Msg("Failed to evict stale remote device caches")
			break
		}
		total += len(evicted)
		if done {
			break
		}
	}
	if total > 0 {
		w.log.Info().Int("evicted", total).Msg("Evicted stale remote device caches")
	}
}

func remoteDeviceRetryDelay(attempts int) time.Duration {
	if attempts >= bits.Len64(uint64(remoteDeviceCacheRetryCap/remoteDeviceCacheRetryBase)) {
		return remoteDeviceCacheRetryCap
	}
	return min(remoteDeviceCacheRetryBase<<max(attempts, 0), remoteDeviceCacheRetryCap)
}
