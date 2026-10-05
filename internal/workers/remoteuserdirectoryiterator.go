package workers

import (
	"context"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	remoteUserDirectoryPositionsKey  = "RemoteUserDirectoryIteratorPositions"
	remoteUserDirectoryLockName      = "RemoteUserDirectoryIteratorLock"
	remoteUserDirectoryLockRetry     = 5 * time.Second
	remoteUserDirectoryLockTimeout   = 30 * time.Second
	remoteUserDirectoryPollInterval  = 5 * time.Second
	remoteUserDirectoryBatchSize     = 10
	remoteUserDirectoryRoomPage      = 100
	remoteUserDirectoryLookupTimeout = 10 * time.Second
	remoteUserDirectoryRetryDelay    = time.Minute
	remoteUserDirectoryMaxRetries    = 8
	// Matches Synapse: gives other state events time to arrive so that a burst
	// of membership changes results in a single profile request.
	remoteUserDirectoryLookupDelay = time.Minute
)

// RemoteUserDirectoryIterator independently walks the accepted event stream.
// It records remote current membership generations in Accounts, then performs
// signed profile requests outside FoundationDB transactions. Processing one
// profile job after every event batch bounds work and avoids starving lookups
// during historical catch-up.
type RemoteUserDirectoryIterator struct {
	iteratorWorker
	fclient fclient.FederationClient
}

func NewRemoteUserDirectoryIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	federationClient fclient.FederationClient,
) *RemoteUserDirectoryIterator {
	w := &RemoteUserDirectoryIterator{
		iteratorWorker: NewWorker(
			"RemoteUserDirectoryIterator", log, cfg, db, notifiers,
			remoteUserDirectoryLockName,
			remoteUserDirectoryLockRetry,
			remoteUserDirectoryLockTimeout,
		),
		fclient: federationClient,
	}
	w.handler = w.run
	return w
}

func (w *RemoteUserDirectoryIterator) run(workerLock lock.Lock) {
	newEvents := w.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer w.notifiers.Unsubscribe(newEvents)
	ticker := time.NewTicker(remoteUserDirectoryPollInterval)
	defer ticker.Stop()

	w.drain(workerLock)
	for {
		select {
		case <-w.ctx.Done():
			workerLock.Release()
			return
		case <-newEvents:
			w.drain(workerLock)
		case <-ticker.C:
			w.drain(workerLock)
		}
	}
}

func (w *RemoteUserDirectoryIterator) drain(workerLock lock.Lock) {
	for w.ctx.Err() == nil {
		workerLock.Refresh()
		hadEvents := w.processEventBatch(workerLock)
		hadProfile := w.processProfileJob(workerLock)
		if !hadEvents && !hadProfile {
			return
		}
	}
}

func (w *RemoteUserDirectoryIterator) processEventBatch(workerLock lock.Lock) bool {
	position, err := w.db.System.GetIteratorPositions(w.ctx, remoteUserDirectoryPositionsKey)
	if err != nil {
		w.log.Err(err).Msg("Failed to read remote user-directory event position")
		return false
	}
	eventTups, err := w.db.Rooms.PaginateAllEventTups(w.ctx, types.PaginationOptions{
		From: position, Limit: remoteUserDirectoryBatchSize,
	})
	if err != nil {
		w.log.Err(err).Msg("Failed to paginate remote user-directory events")
		return false
	} else if len(eventTups) == 0 {
		return false
	}
	sources, joinedRooms, err := w.db.Rooms.DiscoverRemoteDirectoryUsersForEvents(w.ctx, eventTups)
	if err != nil {
		w.log.Err(err).Msg("Failed to validate remote directory memberships")
		return false
	}
	if len(sources) > 0 {
		now := time.Now().UTC()
		if err = w.db.Accounts.EnsureRemoteDirectoryUsers(
			w.ctx, sources, now, now.Add(remoteUserDirectoryLookupDelay),
		); err != nil {
			w.log.Err(err).Msg("Failed to index remote directory memberships")
			return false
		}
	}
	for _, roomID := range joinedRooms {
		if err := w.indexRoomMembers(workerLock, roomID); err != nil {
			w.log.Err(err).Stringer("room_id", roomID).Msg("Failed to index the remote members of a joined room")
			return false
		}
	}
	position = eventTups[len(eventTups)-1].Version
	if err = w.db.System.UpdateIteratorPositions(
		w.ctx, remoteUserDirectoryPositionsKey, position, workerLock.TxnRefresh,
	); err != nil {
		w.log.Err(err).Msg("Failed to update remote user-directory event position")
		return false
	}
	return true
}

// indexRoomMembers indexes the remote members of a room this server joined that are not indexed
// already, from its current state a page of members at a time, as the join's response events are
// staged without entering the index of all events the iterator walks.
func (w *RemoteUserDirectoryIterator) indexRoomMembers(workerLock lock.Lock, roomID id.RoomID) error {
	room, err := w.db.Rooms.GetRoom(w.ctx, roomID)
	if err != nil || room == nil {
		return err
	}
	for from, more := uint64(0), true; more; {
		workerLock.Refresh()
		var sources []types.RemoteUserDirectorySource
		if sources, from, more, err = w.db.Rooms.RoomRemoteDirectorySources(w.ctx, room, from, remoteUserDirectoryRoomPage); err != nil {
			return err
		} else if len(sources) == 0 {
			continue
		}
		now := time.Now().UTC()
		if err := w.db.Accounts.EnsureUnindexedRemoteDirectoryUsers(
			w.ctx, sources, now, now.Add(remoteUserDirectoryLookupDelay),
		); err != nil {
			return err
		}
	}
	return nil
}

func (w *RemoteUserDirectoryIterator) processProfileJob(workerLock lock.Lock) bool {
	now := time.Now().UTC()
	job, err := w.db.Accounts.NextRemoteDirectoryProfileJob(w.ctx, now)
	if err != nil {
		w.log.Err(err).Msg("Failed to get remote user-directory profile job")
		return false
	} else if job == nil {
		return false
	}

	workerLock.Refresh()
	lookupCtx, cancel := context.WithTimeout(w.ctx, remoteUserDirectoryLookupTimeout)
	response, lookupErr := w.fclient.LookupProfile(
		lookupCtx,
		spec.ServerName(w.config.ServerName),
		spec.ServerName(job.UserID.Homeserver()),
		job.UserID.String(),
		"",
	)
	cancel()
	workerLock.Refresh()

	var profile *types.UserProfile
	var retryAt *time.Time
	if lookupErr != nil && job.Attempts < remoteUserDirectoryMaxRetries {
		retry := time.Now().UTC().Add(remoteUserDirectoryRetryDelay << job.Attempts)
		retryAt = &retry
		w.log.Warn().Err(lookupErr).Stringer("user_id", job.UserID).Int("attempts", job.Attempts+1).
			Msg("Remote user-directory profile lookup failed, will retry")
	} else if lookupErr != nil {
		w.log.Warn().Err(lookupErr).Stringer("user_id", job.UserID).Int("attempts", job.Attempts+1).
			Msg("Remote user-directory profile lookup failed, giving up")
	} else {
		profile = &types.UserProfile{
			DisplayName: response.DisplayName,
			AvatarURL:   response.AvatarURL,
		}
	}
	applied, err := w.db.Accounts.FinishRemoteDirectoryProfileJob(
		w.ctx, *job, profile, retryAt,
	)
	if err != nil {
		w.log.Err(err).Stringer("user_id", job.UserID).
			Msg("Failed to finish remote user-directory profile job")
		return false
	} else if !applied {
		w.log.Debug().Stringer("user_id", job.UserID).
			Msg("Ignored superseded remote user-directory profile job")
	}
	return true
}
