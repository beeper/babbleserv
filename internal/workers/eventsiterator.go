package workers

import (
	"encoding/json"
	"time"

	"github.com/rs/zerolog"
	"github.com/samber/lo"
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
	eventsIteratorPositionsKey = "EventsIteratorPositions"
	eventsIteratorLockName     = "EventsIteratorLock"
	eventsIteratorLockRetry    = time.Second * 5
	eventsIteratorLockTimeout  = time.Second * 10
	eventsIteratorBatchSize    = 10
)

// The events iterator is a singleton background worker that iterates over all events ever stored
// by Babbleserv and triggers other things:
// - starts federation senders for servers in rooms with new events
// - sends device change notifications for join/leave events in encrypted rooms
type EventsIterator struct {
	iteratorWorker
}

func NewEventsIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *EventsIterator {
	e := &EventsIterator{NewWorker(
		"EventsIterator", log, cfg, db, notifiers,
		eventsIteratorLockName,
		eventsIteratorLockRetry,
		eventsIteratorLockTimeout,
	)}
	e.handler = e.handleNewEventsLoop
	return e
}

func (e *EventsIterator) handleNewEventsLoop(lock lock.Lock) {
	newEventsCh := e.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer e.notifiers.Rooms.Unsubscribe(newEventsCh)

	// Cold start case: handle anything waiting right away
	e.handleNewEvents(lock)

	for {
		select {
		case <-e.ctx.Done():
			lock.Release()
			return
		case <-newEventsCh:
			e.handleNewEvents(lock)
		case <-time.After(eventsIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (e *EventsIterator) handleNewEvents(lock lock.Lock) {
	startVersion, err := e.db.System.GetIteratorPositions(e.ctx, eventsIteratorPositionsKey)
	if err != nil {
		e.log.Err(err).Msg("Failed to get current position")
		return
	}

	currentVersion := startVersion
	for {
		// Refresh the lock before we process each batch
		lock.Refresh()

		newEventTups, err := e.db.Rooms.PaginateAllEventTups(e.ctx, types.PaginationOptions{
			From:  currentVersion,
			Limit: eventsIteratorBatchSize,
		})
		if err != nil {
			e.log.Err(err).Msg("Failed to paginate new events")
			return
		} else if len(newEventTups) == 0 {
			e.log.Trace().Any("fromVersion", currentVersion).Msg("No events found")
			break
		}

		e.log.Info().
			Int("events", len(newEventTups)).
			Any("fromVersion", currentVersion).
			Msg("Handling new events batch")

		if err := e.notifyFederationSenders(newEventTups); err != nil {
			e.log.Err(err).Msg("Failed to notify federation senders")
		}
		if err := e.sendLocalDeviceChanges(lock, newEventTups); err != nil {
			e.log.Err(err).Msg("Failed to send local device changes")
		}

		currentVersion = newEventTups[len(newEventTups)-1].Version

		if len(newEventTups) < eventsIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = e.db.System.UpdateIteratorPositions(e.ctx, eventsIteratorPositionsKey, currentVersion, lock.TxnRefresh)
	if err != nil {
		e.log.Err(err).Msg("Failed to update current position")
		return
	}
}

func (e *EventsIterator) notifyFederationSenders(tups []types.EventTupWithVersion) error {
	// Get unique room IDs
	roomIDs := make(map[id.RoomID]struct{})
	for _, tup := range tups {
		roomIDs[tup.RoomID] = struct{}{}
	}

	// Now get unique server names from those rooms
	serverNames := make(map[string]struct{})
	for roomID := range roomIDs {
		servers, err := e.db.Rooms.GetCurrentRoomServers(e.ctx, roomID)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if server != e.config.ServerName {
				// We only care about non-local servers
				serverNames[server] = struct{}{}
			}
		}
	}

	if len(serverNames) == 0 {
		return nil
	}

	// Finally send the change
	servers := make([]string, 0, len(serverNames))
	for server := range serverNames {
		servers = append(servers, server)
	}

	e.notifiers.Rooms.SendChange(notifier.Change{
		Servers: servers,
	})

	e.log.Debug().Strs("servers", servers).Msg("Notified federation senders")
	return nil
}

// When a user joins or leaves an encrypted room we need to populate the device_lists part of sync:
// if joining - we send a change to all other members in the room
// if leaving - for every other member check if they share a room with the leaving member, if not
//
//	generate a leave for both the leaving and other member
func (e *EventsIterator) sendLocalDeviceChanges(lock lock.Lock, tups []types.EventTupWithVersion) error {
	allTds := make([]*types.ToDevice, 0, len(tups))

	for _, tup := range tups {
		// We only care about member events
		if tup.Type != event.StateMember {
			continue
		}
		// We only care about encrypted rooms
		enc, err := e.db.Rooms.IsRoomEncrypted(e.ctx, tup.RoomID)
		if err != nil {
			return err
		} else if !enc {
			continue
		}
		ev, err := e.db.Rooms.GetEvent(e.ctx, tup.EventID)
		if err != nil {
			return err
		} else if ev.IsProfileUpdate() {
			// Ignore internal profile updates as these don't actually change membership - note this
			// doesn't cover profile updates over federation.
			continue
		}
		var tds []*types.ToDevice
		switch ev.Membership() {
		case event.MembershipJoin:
			tds, err = e.deviceChangesForJoinEvent(ev)
		case event.MembershipLeave:
			tds, err = e.localDeviceChangesForLeaveEvent(ev)
		default:
		}
		if err != nil {
			return err
		}
		allTds = append(allTds, tds...)
	}

	if len(allTds) > 0 {
		_, err := e.db.Transient.SendToDeviceEvents(e.ctx, allTds, transient.SendToDeviceOptions{
			// Ensure we hold the lock when comitting the events
			LockTxnRefresh: lock.TxnRefresh,
		})
		return err
	}
	return nil
}

func (e *EventsIterator) deviceChangesForJoinEvent(ev *types.Event) ([]*types.ToDevice, error) {
	tds, err := e.localDeviceChangesForJoinEvent(ev)
	if err != nil {
		return nil, err
	}
	// If this is our user we might need to send m.device_list_updates out
	if ev.Sender.Homeserver() == e.config.ServerName {
		if rtds, err := e.remoteDeviceChangesForJoinEvent(ev); err != nil {
			return nil, err
		} else {
			tds = append(tds, rtds...)
		}
	}
	return tds, nil
}

func (e *EventsIterator) localDeviceChangesForJoinEvent(ev *types.Event) ([]*types.ToDevice, error) {
	roomMembers, err := e.db.Rooms.GetCurrentRoomMemberships(e.ctx, ev.RoomID)
	if err != nil {
		return nil, err
	}
	// We only care about members currently joined in the room
	roomMembers = lo.PickBy(roomMembers, func(uid id.UserID, mtup types.MembershipTup) bool {
		return mtup.Membership == event.MembershipJoin
	})

	tds := make([]*types.ToDevice, 0, len(roomMembers))

	for memberID := range roomMembers {
		if memberID.Homeserver() != e.config.ServerName {
			continue
		}
		devices, err := e.db.Accounts.GetUserDevices(e.ctx, memberID)
		if err != nil {
			return nil, err
		}
		for _, d := range devices {
			tds = append(tds, &types.ToDevice{
				Type:     types.BabbleservLocalDeviceChange,
				UserID:   memberID,
				DeviceID: d.ID,
				Sender:   ev.Sender,
			})
		}
	}

	// If the joining user is local, also notify them about changes to all other members
	if ev.Sender.Homeserver() == e.config.ServerName {
		devices, err := e.db.Accounts.GetUserDevices(e.ctx, ev.Sender)
		if err != nil {
			return nil, err
		}
		for memberID := range roomMembers {
			if memberID == ev.Sender {
				continue
			}
			for _, d := range devices {
				tds = append(tds, &types.ToDevice{
					Type:     types.BabbleservLocalDeviceChange,
					UserID:   ev.Sender,
					DeviceID: d.ID,
					Sender:   memberID,
				})
			}
		}
	}

	return tds, nil
}

// https://github.com/element-hq/synapse/issues/11374, m.device_list_edus are sent per spec:
// ... when that user joins a room which contains servers which are not already receiving updates for that user’s device list
func (e *EventsIterator) remoteDeviceChangesForJoinEvent(ev *types.Event) ([]*types.ToDevice, error) {
	// TODO: this is inefficient and inconsistent - we should pull memberships shared between us
	// and each other server inside a single txn alongside the encryption filtering.

	// Note: servers are always joined
	localServerMemberships, err := e.db.Rooms.GetServerMemberships(e.ctx, e.config.ServerName)
	if err != nil {
		return nil, err
	}

	roomServers, err := e.db.Rooms.GetCurrentRoomServers(e.ctx, ev.RoomID)
	if err != nil {
		return nil, err
	}

	devices, err := e.db.Accounts.GetUserDevices(e.ctx, ev.Sender)
	if err != nil {
		return nil, err
	}

	getDeviceKeys := util.MemoizeMap(func(k id.DeviceID) (*mautrix.DeviceKeys, error) {
		// As above, requestUserID=""
		return e.db.Accounts.GetDeviceKeys(e.ctx, ev.Sender, k, "")
	}, 10)

	tds := make([]*types.ToDevice, 0, len(roomServers))

	for _, server := range roomServers {
		// Find any other rooms shared with this server that also contain the joining user, if none
		// we need to initialize the device updates we send to the server for this user. We do this
		// by generating a m.device_list_update for each device of the joining user.
		memberships, err := e.db.Rooms.GetServerMemberships(e.ctx, server)
		if err != nil {
			return nil, err
		}
		var match bool
		for roomID := range memberships {
			if roomID == ev.RoomID {
				continue
			} else if _, ok := localServerMemberships[roomID]; !ok {
				continue
			} else if isEncrypted, err := e.db.Rooms.IsRoomEncrypted(e.ctx, ev.RoomID); err != nil {
				return nil, err
			} else if !isEncrypted {
				continue
			}
			// Both us and the other server share this other room, check if our joining user is in it
			rmMemberships, err := e.db.Rooms.GetCurrentRoomMemberships(e.ctx, ev.RoomID)
			if err != nil {
				return nil, err
			}
			if mtup, ok := rmMemberships[ev.Sender]; ok && mtup.Membership == event.MembershipJoin {
				match = true
				break
			}
		}
		if !match {
			// Target is the server, not user, but we smuggle such updates through to-device
			// internally (see the DeviceChangeIterator).
			serverUserID := id.UserID("@:" + server)

			// We have no matching rooms, generate an update for each joining users device
			for _, d := range devices {
				keys, err := getDeviceKeys(d.ID)
				if err != nil {
					return nil, err
				}
				content := types.DeviceListUpdateEDUContent{
					UserID:     ev.Sender,
					DeviceID:   d.ID,
					DeviceKeys: keys,
				}
				b, _ := json.Marshal(content)
				tds = append(tds, &types.ToDevice{
					Type:    types.BabbleservRemoteDeviceListUpdate,
					UserID:  serverUserID,
					Content: b,
				})
			}
		}
	}

	return tds, nil
}

// Leave events are complicated - for every user in the room we need to determine if they still
// share any encrypted memberships with the leaving user. If not we tell both them and the leaving
// user they no longer share any encrypted rooms (device_lists.left).
func (e *EventsIterator) localDeviceChangesForLeaveEvent(ev *types.Event) ([]*types.ToDevice, error) {
	leaverMemberships, err := e.db.Rooms.GetUserMemberships(e.ctx, ev.Sender)
	if err != nil {
		return nil, err
	}
	// We only care about rooms the leaving user is joined in
	leaverMemberships = lo.PickBy(leaverMemberships, func(rid id.RoomID, mtup types.MembershipTup) bool {
		return mtup.Membership == event.MembershipJoin
	})

	var leaverDevices []*types.Device
	if ev.Sender.Homeserver() == e.config.ServerName {
		leaverDevices, err = e.db.Accounts.GetUserDevices(e.ctx, ev.Sender)
		if err != nil {
			return nil, err
		}
	}

	roomMembers, err := e.db.Rooms.GetCurrentRoomMemberships(e.ctx, ev.RoomID)
	if err != nil {
		return nil, err
	}

	tds := make([]*types.ToDevice, 0, len(roomMembers))

	for memberID := range roomMembers {
		if memberID == ev.Sender {
			// Ignore our own leaves
			continue
		} else if ev.Sender.Homeserver() != e.config.ServerName && memberID.Homeserver() != e.config.ServerName {
			// If neither the leaver or other member are local, nothing to do here
			continue
		}
		memberships, err := e.db.Rooms.GetUserMemberships(e.ctx, memberID)
		if err != nil {
			return nil, err
		}
		var match bool
		for roomID, membershipTup := range memberships {
			if membershipTup.Membership != event.MembershipJoin {
				continue
			}
			if _, ok := leaverMemberships[roomID]; ok {
				match = true
				break
			}
		}
		// The leaving user and the still joined user no longer share any room memberships, notify
		// them both.
		if !match {
			if ev.Sender.Homeserver() == e.config.ServerName {
				for _, d := range leaverDevices {
					tds = append(tds, &types.ToDevice{
						Type:     types.BabbleservLocalDeviceLeft,
						Sender:   memberID,
						UserID:   ev.Sender,
						DeviceID: d.ID,
					})
				}
			}
			if memberID.Homeserver() == e.config.ServerName {
				devices, err := e.db.Accounts.GetUserDevices(e.ctx, memberID)
				if err != nil {
					return nil, err
				}
				for _, d := range devices {
					tds = append(tds, &types.ToDevice{
						Type:     types.BabbleservLocalDeviceLeft,
						Sender:   ev.Sender,
						UserID:   memberID,
						DeviceID: d.ID,
					})
				}
			}
		}
	}

	return tds, nil
}
