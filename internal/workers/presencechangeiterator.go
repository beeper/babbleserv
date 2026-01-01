package workers

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	presenceChangeIteratorPositionsKey = "PresenceChangeIteratorPositions"
	presenceChangeIteratorLockName     = "PresenceChangeIteratorLock"
	presenceChangeIteratorLockRetry    = time.Second * 5
	presenceChangeIteratorLockTimeout  = time.Second * 10
	presenceChangeIteratorBatchSize    = 10
)

// The PresenceChangeIterator watches for presence changes in the transient database and turns them
// into to-device messages (also in the transient database) which are sent over federation or sync.
// Handles both local and remote presence changes.
//
// For remote presence changes:
// - find relevant (shared joined room) local users
// - send babbleserv.local_presence_change to-device events -> local users
//
// For local presence changes:
// - same as above for local users
// - find relevant remote servers
// - send babbleserv.remote_presence_change to-device events -> servers
type PresenceChangeIterator struct {
	iteratorWorker
}

func NewPresenceChangeIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *PresenceChangeIterator {
	p := &PresenceChangeIterator{NewWorker(
		"PresenceChangeIterator", log, cfg, db, notifiers,
		presenceChangeIteratorLockName,
		presenceChangeIteratorLockRetry,
		presenceChangeIteratorLockTimeout,
	)}
	p.handler = p.handlePresenceChangesLoop
	return p
}

func (p *PresenceChangeIterator) handlePresenceChangesLoop(lock lock.Lock) {
	// Subscribe to presence changes
	newPresenceCh := p.notifiers.Subscribe(notifier.Subscription{AllUsers: true})
	defer p.notifiers.Unsubscribe(newPresenceCh)

	// Cold start: process any pending changes
	p.handlePresenceChanges(lock)

	for {
		select {
		case <-p.ctx.Done():
			lock.Release()
			return
		case <-newPresenceCh:
			p.handlePresenceChanges(lock)
		case <-time.After(presenceChangeIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (p *PresenceChangeIterator) handlePresenceChanges(lock lock.Lock) {
	startVersion, err := p.db.System.GetIteratorPositions(p.ctx, presenceChangeIteratorPositionsKey)
	if err != nil {
		p.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh lock before each batch
		lock.Refresh()

		presenceChanges, err := p.db.Transient.PaginatePresenceChanges(p.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: presenceChangeIteratorBatchSize,
		})
		if err != nil {
			p.log.Err(err).Msg("Failed to paginate presence changes")
			return
		} else if len(presenceChanges) == 0 {
			p.log.Trace().Any("fromVersion", currentVersion).Msg("No presence changes found")
			break
		}

		p.log.Info().
			Int("changes", len(presenceChanges)).
			Any("fromVersion", currentVersion).
			Msg("Handling presence changes batch")

		for _, change := range presenceChanges {
			if change.UserID.Homeserver() == p.config.ServerName {
				if err := p.processLocalPresenceChange(lock, change); err != nil {
					p.log.Err(err).
						Str("user_id", change.UserID.String()).
						Msg("Failed to process local presence change")
				}
			} else {
				if err := p.processRemotePresenceChange(lock, change); err != nil {
					p.log.Err(err).
						Str("user_id", change.UserID.String()).
						Msg("Failed to process remote presence change")
				}
			}
		}

		currentVersion = presenceChanges[len(presenceChanges)-1].Version

		if len(presenceChanges) < presenceChangeIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update position atomically with lock refresh
	err = p.db.System.UpdateIteratorPositions(
		p.ctx,
		presenceChangeIteratorPositionsKey,
		currentVersion,
		lock.TxnRefresh,
	)
	if err != nil {
		p.log.Err(err).Msg("Failed to update current position")
		return
	}

	// TODO: clear presence changes > X old or all, not needed
}

// Remote presence changes are simple we just need to find the relevant local users sharing
// rooms and send to-device events to populate presence during sync.
func (p *PresenceChangeIterator) processRemotePresenceChange(lock lock.Lock, change *types.PresenceChange) error {
	// Find all rooms the user is in
	memberships, err := p.db.Rooms.GetUserJoinedMemberships(p.ctx, change.Presence.UserID)
	if err != nil {
		return fmt.Errorf("failed to lookup memberships: %w", err)
	}

	// Now find distinct local userIDs joined in those rooms
	localUserIDs := make(map[id.UserID]struct{}, len(memberships))

	for roomID := range memberships {
		roomMemberships, err := p.db.Rooms.GetCurrentRoomMemberships(p.ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to get room member IDs: %s: %w", roomID, err)
		}
		for memberID, membershipTup := range roomMemberships {
			if membershipTup.Membership == event.MembershipJoin && memberID.Homeserver() == p.config.ServerName {
				localUserIDs[memberID] = struct{}{}
			}
		}
	}

	// Generate list of to-device events to send
	tds := make([]*types.ToDevice, 0, len(localUserIDs))

	for userID := range localUserIDs {
		userDevices, err := p.db.Accounts.GetUserDevices(p.ctx, userID)
		if err != nil {
			return err
		}
		for _, d := range userDevices {
			tds = append(tds, &types.ToDevice{
				UserID:   userID,
				DeviceID: d.ID,
				Type:     types.BabbleservLocalPresenceChange,
				Sender:   change.UserID,
				Content:  makePresenceLocalContent(change),
			})
		}
	}

	if len(tds) > 0 {
		_, err = p.db.Transient.SendToDeviceEvents(p.ctx, tds, transient.SendToDeviceOptions{
			// Ensure we hold the lock when comitting the events
			LockTxnRefresh: lock.TxnRefresh,
		})
		return err
	}
	return nil
}

// Local presence changes need to be turned into both local user presence sync updates as well as
// EDUs to send over federation.
func (p *PresenceChangeIterator) processLocalPresenceChange(lock lock.Lock, change *types.PresenceChange) error {
	// Find all rooms the user is in
	memberships, err := p.db.Rooms.GetUserJoinedMemberships(p.ctx, change.Presence.UserID)
	if err != nil {
		return fmt.Errorf("failed to lookup memberships: %w", err)
	}

	// Now find distinct local userIDs & remote serverNames for those rooms
	localUserIDs := make(map[id.UserID]struct{}, len(memberships))
	remoteServerNames := make(map[string]struct{}, len(memberships))

	// Always notify ourselves
	localUserIDs[change.Presence.UserID] = struct{}{}

	for roomID := range memberships {
		roomMemberships, err := p.db.Rooms.GetCurrentRoomMemberships(p.ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to get room member IDs: %s: %w", roomID, err)
		}
		for memberID, membershipTup := range roomMemberships {
			if membershipTup.Membership != event.MembershipJoin {
				// We only care about joined members
				continue
			}
			if memberID.Homeserver() == p.config.ServerName {
				localUserIDs[memberID] = struct{}{}
			} else {
				remoteServerNames[memberID.Homeserver()] = struct{}{}
			}
		}
	}

	p.log.Debug().
		Str("user_id", change.Presence.UserID.String()).
		Int("local_user_ids", len(localUserIDs)).
		Int("remote_servers", len(remoteServerNames)).
		Msg("Processing local presence change")

	// Generate list of to-device events to send
	tds := make([]*types.ToDevice, 0, len(localUserIDs)+len(remoteServerNames))

	for userID := range localUserIDs {
		// If user is local - we just need to populate presence in sync, so send to-device
		// events to be expanded later.
		if userID.Homeserver() == p.config.ServerName {
			userDevices, err := p.db.Accounts.GetUserDevices(p.ctx, userID)
			if err != nil {
				return err
			}

			for _, d := range userDevices {
				tds = append(tds, &types.ToDevice{
					Type:     types.BabbleservLocalPresenceChange,
					UserID:   userID,
					DeviceID: d.ID,
					Sender:   change.UserID,
					Content:  makePresenceLocalContent(change),
				})
			}
		}
	}

	for serverName := range remoteServerNames {
		// Target is the server, not user, but we smuggle such updates through to-device
		// internally (see the PresenceChangeIterator).
		serverUserID := id.UserID("@:" + serverName)

		tds = append(tds, &types.ToDevice{
			Type:    types.BabbleservRemotePresenceChange,
			UserID:  serverUserID,
			Content: makePresenceEDUContent(change),
		})
	}

	if len(tds) > 0 {
		_, err = p.db.Transient.SendToDeviceEvents(p.ctx, tds, transient.SendToDeviceOptions{
			// Ensure we hold the lock when comitting the events
			LockTxnRefresh: lock.TxnRefresh,
		})
		return err
	}
	return nil
}

func makePresenceLocalItem(change *types.PresenceChange) types.PresenceEDUItem {
	lastActiveAgo := time.Since(change.LastActive).Milliseconds()
	if lastActiveAgo < 0 {
		lastActiveAgo = 0
	}

	return types.PresenceEDUItem{
		UserID:        change.UserID,
		Presence:      change.Presence.Presence,
		StatusMsg:     change.Message,
		LastActiveAgo: lastActiveAgo,
		// Online is always considered active, after X minutes configured we'll automatically change
		// it to unavailable.
		CurrentlyActive: change.Presence.Presence == "online",
	}
}

func makePresenceLocalContent(change *types.PresenceChange) []byte {
	return exerrors.Must(json.Marshal(makePresenceLocalItem(change)))
}

func makePresenceEDUContent(change *types.PresenceChange) []byte {
	content := types.PresenceEDUContent{
		Push: []types.PresenceEDUItem{makePresenceLocalItem(change)},
	}
	return exerrors.Must(json.Marshal(content))
}
