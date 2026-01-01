package workers

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

const (
	deviceChangeIteratorPositionsKey = "DeviceChangeIteratorPositions"
	deviceChangeIteratorLockName     = "DeviceChangeIteratorLock"
	deviceChangeIteratorLockRetry    = time.Second * 5
	deviceChangeIteratorLockTimeout  = time.Second * 10
	deviceChangeIteratorBatchSize    = 10
)

// The DeviceChangeIterator watches for device changes in the accounts database and turns them into
// to-device messages in the transient database which are sent over federation or sync. Handles both
// local and remote user device changes.
//
// For remote user device changes:
//   - find relevant (shared encrypted rooms) local users
//   - send babbleserv.local_device_change to-device events -> local users
//   - these then populate device_lists in sync api
//
// For local user device changes:
//   - same as above for local users
//   - find relevant remote servers
//   - send babbleserv.remote_[device_list_update|signing_key_update] to-device events -> servers
type DeviceChangeIterator struct {
	iteratorWorker
}

func NewDeviceChangeIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *DeviceChangeIterator {
	d := &DeviceChangeIterator{NewWorker(
		"DeviceChangeIterator", log, cfg, db, notifiers,
		deviceChangeIteratorLockName,
		deviceChangeIteratorLockRetry,
		deviceChangeIteratorLockTimeout,
	)}
	d.handler = d.handleDeviceChangesLoop
	return d
}

func (d *DeviceChangeIterator) handleDeviceChangesLoop(lock lock.Lock) {
	// Subscribe to user account changes
	newProfilesCh := d.notifiers.Subscribe(notifier.Subscription{AllUsers: true})
	defer d.notifiers.Unsubscribe(newProfilesCh)

	// Cold start: process any pending changes
	d.handleDeviceChanges(lock)

	for {
		select {
		case <-d.ctx.Done():
			lock.Release()
			return
		case <-newProfilesCh:
			d.handleDeviceChanges(lock)
		case <-time.After(deviceChangeIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (d *DeviceChangeIterator) handleDeviceChanges(lock lock.Lock) {
	startVersion, err := d.db.System.GetIteratorPositions(d.ctx, deviceChangeIteratorPositionsKey)
	if err != nil {
		d.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh lock before each batch
		lock.Refresh()

		deviceChanges, err := d.db.Accounts.PaginateDeviceChanges(d.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: deviceChangeIteratorBatchSize,
		})
		if err != nil {
			d.log.Err(err).Msg("Failed to paginate device changes")
			return
		} else if len(deviceChanges) == 0 {
			d.log.Trace().Any("fromVersion", currentVersion).Msg("No device changes found")
			break
		}

		d.log.Info().
			Int("changes", len(deviceChanges)).
			Any("fromVersion", currentVersion).
			Msg("Handling device changes batch")

		for _, change := range deviceChanges {
			if change.UserID.Homeserver() == d.config.ServerName {
				if err := d.processLocalDeviceChange(lock, change); err != nil {
					d.log.Err(err).
						Str("user_id", change.UserID.String()).
						Msg("Failed to process local device change")
				}
			} else {
				if err := d.processRemoteDeviceChange(lock, change); err != nil {
					d.log.Err(err).
						Str("user_id", change.UserID.String()).
						Msg("Failed to process remote device change")
				}
			}
		}

		currentVersion = deviceChanges[len(deviceChanges)-1].Version

		if len(deviceChanges) < deviceChangeIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update position atomically with lock refresh
	err = d.db.System.UpdateIteratorPositions(
		d.ctx,
		deviceChangeIteratorPositionsKey,
		currentVersion,
		lock.TxnRefresh,
	)
	if err != nil {
		d.log.Err(err).Msg("Failed to update current position")
		return
	}

	// TODO: clear device changes > X old
}

// Remote device changes are simple we just need to find the relevant local users sharing encrypted
// rooms and send dummy to-device events to populate device_lists during sync.
func (d *DeviceChangeIterator) processRemoteDeviceChange(lock lock.Lock, change types.UserDeviceChange) error {
	// Find all encrypted rooms the user is in
	memberships, err := d.db.Rooms.GetUserJoinedMembershipsWithEncryption(d.ctx, change.UserID)
	if err != nil {
		return fmt.Errorf("failed to lookup memberships: %w", err)
	}

	// Now find distinct local userIDs joined in those rooms
	localUserIDs := make(map[id.UserID]struct{}, len(memberships))

	for roomID := range memberships {
		memberships, err := d.db.Rooms.GetCurrentRoomMemberships(d.ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to get room member IDs: %s: %w", roomID, err)
		}
		for memberID, membershipTup := range memberships {
			if membershipTup.Membership == event.MembershipJoin && memberID.Homeserver() == d.config.ServerName {
				localUserIDs[memberID] = struct{}{}
			}
		}
	}

	// Generate list of to-device events to send
	tds := make([]*types.ToDevice, 0, len(localUserIDs))

	for userID := range localUserIDs {
		userDevices, err := d.db.Accounts.GetUserDevices(d.ctx, userID)
		if err != nil {
			return err
		}
		for _, d := range userDevices {
			tds = append(tds, &types.ToDevice{
				UserID:   userID,
				DeviceID: d.ID,
				Sender:   change.UserID,
				Type:     types.BabbleservLocalDeviceChange,
			})
		}
	}

	_, err = d.db.Transient.SendToDeviceEvents(d.ctx, tds, transient.SendToDeviceOptions{
		// Ensure we hold the lock when comitting the events
		LockTxnRefresh: lock.TxnRefresh,
	})
	return err
}

// Local device changes need to be turned into both local user device_lists sync updates as well as
// EDUs to send over federation which vary depending on the change (XS keys or single device).
func (d *DeviceChangeIterator) processLocalDeviceChange(lock lock.Lock, change types.UserDeviceChange) error {
	// Find all encrypted rooms the user is in
	memberships, err := d.db.Rooms.GetUserJoinedMembershipsWithEncryption(d.ctx, change.UserID)
	if err != nil {
		return fmt.Errorf("failed to lookup memberships: %w", err)
	}

	// Now find distinct local userIDs & remote serverNames for those rooms
	localUserIDs := make(map[id.UserID]struct{}, len(memberships))
	remoteServerNames := make(map[string]struct{}, len(memberships))

	// Always notify ourselves
	localUserIDs[change.UserID] = struct{}{}

	for roomID := range memberships {
		roomMemberships, err := d.db.Rooms.GetCurrentRoomMemberships(d.ctx, roomID)
		if err != nil {
			return fmt.Errorf("failed to get room member IDs: %s: %w", roomID, err)
		}
		for memberID, membershipTup := range roomMemberships {
			if membershipTup.Membership != event.MembershipJoin {
				// We only care about joined members
				continue
			}
			if memberID.Homeserver() == d.config.ServerName {
				localUserIDs[memberID] = struct{}{}
			} else {
				remoteServerNames[memberID.Homeserver()] = struct{}{}
			}
		}
	}

	d.log.Debug().
		Any("change", change).
		Int("local_user_ids", len(localUserIDs)).
		Int("remote_servers", len(remoteServerNames)).
		Msg("Processing local device change")

	// Generate list of to-device events to send
	tds := make([]*types.ToDevice, 0, len(localUserIDs))

	for userID := range localUserIDs {
		// If user is local - we just need to populate device_lists in sync, so send dummy to-device
		// events to be expanded later, no content needed as clients will query it.
		if userID.Homeserver() == d.config.ServerName {
			userDevices, err := d.db.Accounts.GetUserDevices(d.ctx, userID)
			if err != nil {
				return err
			}
			for _, d := range userDevices {
				tds = append(tds, &types.ToDevice{
					Type:     types.BabbleservLocalDeviceChange,
					UserID:   userID,
					DeviceID: d.ID,
					Sender:   change.UserID,
				})
			}
			continue
		}
	}

	// Lazy getters for user XS + device keys, needed if we're notifying servers not local users
	getCrossSigningKeys := util.Memoize(func() (*types.UserCrossSigningKeys, error) {
		// Note requestUserID="" here such that we only see/push the "public" view
		return d.db.Accounts.GetUserCrossSigningKeys(d.ctx, change.UserID, "")
	})
	getDeviceKeys := util.Memoize(func() (*mautrix.DeviceKeys, error) {
		// As above, requestUserID=""
		return d.db.Accounts.GetDeviceKeys(d.ctx, change.UserID, change.DeviceID, "")
	})
	getDevice := util.Memoize(func() (*types.Device, error) {
		return d.db.Accounts.GetUserDevice(d.ctx, change.UserID, change.DeviceID)
	})

	for serverName := range remoteServerNames {
		// Target is the server, not user, but we smuggle such updates through to-device
		// internally (see the DeviceChangeIterator).
		serverUserID := id.UserID("@:" + serverName)

		if change.DeviceID == "*" {
			// If change.DeviceID == "*" cross signing keys have been updated, we must send them in
			// a m.signing_key_update EDU to the server.
			keys, err := getCrossSigningKeys()
			if err != nil {
				return err
			} else if keys == nil {
				d.log.Warn().Msg("Got empty cross signing keys for device change, ignoring")
				continue
			}
			content := types.SigningKeyUpdateEDUContent{
				UserID:      change.UserID,
				MasterKey:   keys.Master.CrossSigningKeys,
				SelfSigning: keys.SelfSigning.CrossSigningKeys,
			}
			tds = append(tds, &types.ToDevice{
				Type:    types.BabbleservRemoteSigningKeyUpdate,
				UserID:  serverUserID,
				Content: exerrors.Must(json.Marshal(content)),
			})
		} else {
			// Device list update, we must send the device and keys in a m.device_list_update EDU to
			// the server.
			keys, err := getDeviceKeys()
			if err != nil {
				return err
			}
			content := types.DeviceListUpdateEDUContent{
				UserID:     change.UserID,
				DeviceID:   change.DeviceID,
				DeviceKeys: keys,
			}

			// Check the device exists, if not this is a deleted notification
			device, err := getDevice()
			if err != nil {
				return err
			} else if device == nil {
				content.Deleted = true
			}

			tds = append(tds, &types.ToDevice{
				Type:    types.BabbleservRemoteDeviceListUpdate,
				UserID:  serverUserID,
				Content: exerrors.Must(json.Marshal(content)),
			})
		}
	}

	_, err = d.db.Transient.SendToDeviceEvents(d.ctx, tds, transient.SendToDeviceOptions{
		// Ensure we hold the lock when comitting the events
		LockTxnRefresh: lock.TxnRefresh,
	})
	return err
}
