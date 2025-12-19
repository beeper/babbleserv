package workers

import (
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	profileChangeIteratorPositionsKey = "ProfileChangeIteratorPositions"
	profileChangeIteratorLockName     = "ProfileChangeIteratorLock"
	profileChangeIteratorLockRetry    = time.Second * 5
	profileChangeIteratorLockTimeout  = time.Second * 10
	profileChangeIteratorBatchSize    = 10
)

// ProfileChangeIterator iterates over profile changes from the accounts database and propagates
// them to all rooms where the user has joined membership.
type ProfileChangeIterator struct {
	iteratorWorker
}

func NewProfileChangeIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *ProfileChangeIterator {
	p := &ProfileChangeIterator{NewWorker(
		"ProfileChangeIterator", log, cfg, db, notifiers,
		profileChangeIteratorLockName,
		profileChangeIteratorLockRetry,
		profileChangeIteratorLockTimeout,
	)}
	p.handler = p.handleProfileChangesLoop
	return p
}

func (p *ProfileChangeIterator) handleProfileChangesLoop(lock lock.Lock) {
	// Subscribe to user account changes
	newProfilesCh := p.notifiers.Subscribe(notifier.Subscription{AllUsers: true})
	defer p.notifiers.Accounts.Unsubscribe(newProfilesCh)

	// Cold start: process any pending changes
	p.handleProfilesChanges(lock)

	for {
		select {
		case <-p.ctx.Done():
			lock.Release()
			return
		case <-newProfilesCh:
			p.handleProfilesChanges(lock)
		case <-time.After(profileChangeIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (p *ProfileChangeIterator) handleProfilesChanges(lock lock.Lock) {
	startVersion, err := p.db.System.GetIteratorPositions(p.ctx, profileChangeIteratorPositionsKey)
	if err != nil {
		p.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh lock before each batch
		lock.Refresh()

		profileChanges, err := p.db.Accounts.PaginateProfileChanges(p.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: profileChangeIteratorBatchSize,
		})
		if err != nil {
			p.log.Err(err).Msg("Failed to paginate profile changes")
			return
		} else if len(profileChanges) == 0 {
			p.log.Trace().Any("fromVersion", currentVersion).Msg("No profile changes found")
			break
		}

		p.log.Info().
			Int("changes", len(profileChanges)).
			Any("fromVersion", currentVersion).
			Msg("Handling profile changes batch")

		// Process each profile change
		for _, change := range profileChanges {
			if err := p.processProfileChange(lock, change); err != nil {
				p.log.Err(err).
					Str("user_id", change.UserID.String()).
					Msg("Failed to process profile change")
				// Continue processing other changes
			}
		}

		currentVersion = profileChanges[len(profileChanges)-1].Version

		if len(profileChanges) < profileChangeIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update position atomically with lock refresh
	err = p.db.System.UpdateIteratorPositions(
		p.ctx,
		profileChangeIteratorPositionsKey,
		currentVersion,
		lock.TxnRefresh,
	)
	if err != nil {
		p.log.Err(err).Msg("Failed to update current position")
		return
	}

	// Clear processed changes
	err = p.db.Accounts.ClearProfileChanges(p.ctx, currentVersion, lock.TxnRefresh)
	if err != nil {
		p.log.Err(err).Msg("Failed to clear processed profile changes")
		return
	}
}

func (p *ProfileChangeIterator) processProfileChange(lock lock.Lock, change types.UserProfileChange) error {
	// Get all room memberships for this user
	memberships, err := p.db.Rooms.GetUserMemberships(p.ctx, change.UserID)
	if err != nil {
		return fmt.Errorf("failed to lookup memberships: %w", err)
	}

	var updated int
	var failed int

	// For each room where user has joined
	for roomID, membershipTup := range memberships {
		if membershipTup.Membership != event.MembershipJoin {
			continue
		}

		// Build updated membership content with new profile
		content := change.Profile.ToMembershipContent()
		content["membership"] = string(membershipTup.Membership)
		content["babbleserv.is_profile_update"] = true
		sKey := string(change.UserID)

		// Create partial event for updated membership
		partialEv := types.NewPartialEvent(
			roomID,
			event.StateMember,
			&sKey,
			change.UserID,
			content,
		)

		// Send event to room
		results, err := p.db.Rooms.SendLocalEvents(
			p.ctx,
			roomID,
			[]*types.PartialEvent{partialEv},
			rooms.SendLocalEventsOptions{
				// Ensure we hold the lock when comitting the events
				LockTxnRefresh: lock.TxnRefresh,
			},
		)

		if err != nil {
			p.log.Err(err).
				Str("user_id", change.UserID.String()).
				Str("room_id", roomID.String()).
				Msg("Failed to send updated member event")
			failed++
		} else if len(results.Rejected) > 0 {
			err := results.Rejected[0].Error
			p.log.Warn().Err(err).
				Str("user_id", change.UserID.String()).
				Str("room_id", roomID.String()).
				Msg("Updated member event not allowed")
			failed++
		} else {
			updated++
		}
	}

	p.log.Info().
		Str("user_id", change.UserID.String()).
		Int("updated", updated).
		Int("failed", failed).
		Int("total_rooms", len(memberships)).
		Msg("Processed profile change")

	return nil
}
