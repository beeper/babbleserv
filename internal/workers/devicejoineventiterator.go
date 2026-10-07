package workers

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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
	deviceJoinIteratorPositionsKey = "DeviceJoinEventIteratorPositions"
	deviceJoinIteratorLockName     = "DeviceJoinEventIteratorLock"
	deviceJoinIteratorLockRetry    = time.Second * 5
	deviceJoinIteratorLockTimeout  = time.Second * 10
	deviceJoinIteratorBatchSize    = 10
)

// The DeviceJoinEventIterator watches for join events in the rooms database and generates relevant
// to-device messages in the transient database which are sent over federation or sync.
type DeviceJoinEventIterator struct {
	iteratorWorker
}

func NewDeviceJoinEventIterator(
	log zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
) *DeviceJoinEventIterator {
	e := &DeviceJoinEventIterator{NewWorker(
		"DeviceJoinEventIterator", log, cfg, db, notifiers,
		deviceJoinIteratorLockName,
		deviceJoinIteratorLockRetry,
		deviceJoinIteratorLockTimeout,
	)}
	e.handler = e.handleNewEventsLoop
	return e
}

func (e *DeviceJoinEventIterator) handleNewEventsLoop(lock lock.Lock) {
	newEventsCh := e.notifiers.Subscribe(notifier.Subscription{AllEvents: true})
	defer e.notifiers.Unsubscribe(newEventsCh)

	// Cold start case: handle anything waiting right away
	e.handleNewEvents(lock)

	for {
		select {
		case <-e.ctx.Done():
			lock.Release()
			return
		case <-newEventsCh:
			e.handleNewEvents(lock)
		case <-time.After(deviceJoinIteratorLockRetry):
			lock.Refresh()
		}
	}
}

func (e *DeviceJoinEventIterator) handleNewEvents(lock lock.Lock) {
	startVersion, err := e.db.System.GetIteratorPositions(e.ctx, deviceJoinIteratorPositionsKey)
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
			Limit: deviceJoinIteratorBatchSize,
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

		if err := e.sendLocalDeviceChanges(lock, newEventTups); err != nil {
			e.log.Err(err).Msg("Failed to send local device changes")
		}

		currentVersion = newEventTups[len(newEventTups)-1].Version

		if len(newEventTups) < deviceJoinIteratorBatchSize {
			break
		}
	}

	if currentVersion == startVersion {
		return
	}

	// Update the position - refreshing the lock as part of the transaction to
	// ensure the write is safe.
	err = e.db.System.UpdateIteratorPositions(e.ctx, deviceJoinIteratorPositionsKey, currentVersion, lock.TxnRefresh)
	if err != nil {
		e.log.Err(err).Msg("Failed to update current position")
		return
	}
}

// When a user joins or leaves an encrypted room we need to populate the device_lists part of sync:
// if joining - we send a change to all other members in the room
// if leaving - for every other member check if they share a room with the leaving member, if not
//
//	generate a leave for both the leaving and other member
func (e *DeviceJoinEventIterator) sendLocalDeviceChanges(lock lock.Lock, tups []types.EventTupWithVersion) error {
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
		} else if ev.IsBabbleProfileUpdate() && ev.Sender.Homeserver() == e.config.ServerName {
			// Ignore internal profile updates as these don't actually change membership - note this
			// doesn't cover profile updates over federation.
			continue
		}
		var tds []*types.ToDevice
		switch ev.Membership() {
		case event.MembershipJoin:
			tds, err = e.deviceChangesForJoin(ev, tup.Version)
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
		_, err := e.db.SendToDeviceEvents(e.ctx, allTds, transient.SendToDeviceOptions{
			// Ensure we hold the lock when comitting the events
			LockTxnRefresh: lock.TxnRefresh,
		})
		return err
	}
	return nil
}

// deviceChangesForJoin returns the device list changes of a join. A local user's remote join that made
// this server joined also handles the other local members joined with it, whose joins its response
// may have named: those were staged without state and never reach the iterator.
func (e *DeviceJoinEventIterator) deviceChangesForJoin(ev *types.Event, version tuple.Versionstamp) ([]*types.ToDevice, error) {
	isLocal := ev.Sender.Homeserver() == e.config.ServerName
	if isLocal && !ev.HasStateIn(ev.RoomID) {
		return nil, nil
	}
	roomMembers, err := e.db.Rooms.RoomMembers(e.ctx, ev.RoomID, event.MembershipJoin)
	if err != nil {
		return nil, err
	}
	joiners := []id.UserID{ev.Sender}
	if isLocal && !ev.Local {
		localMembers, err := e.db.Rooms.LocalMembersJoinedWith(e.ctx, ev, version)
		if err != nil {
			return nil, err
		}
		for memberID := range localMembers {
			if memberID != ev.Sender {
				joiners = append(joiners, memberID)
			}
		}
	}

	var tds []*types.ToDevice
	for _, joiner := range joiners {
		tds = append(tds, e.localDeviceChangesForJoin(joiner, roomMembers)...)
		// If this is our user we might need to send m.device_list_updates out
		if joiner.Homeserver() == e.config.ServerName {
			rtds, err := e.remoteDeviceChangesForJoin(ev.RoomID, joiner)
			if err != nil {
				return nil, err
			}
			tds = append(tds, rtds...)
		}
	}
	return tds, nil
}

func (e *DeviceJoinEventIterator) localDeviceChangesForJoin(joiner id.UserID, roomMembers types.RoomMemberships) []*types.ToDevice {
	tds := make([]*types.ToDevice, 0, len(roomMembers))

	for memberID := range roomMembers {
		if memberID.Homeserver() != e.config.ServerName {
			continue
		}
		tds = append(tds, &types.ToDevice{
			Type:     types.BabbleservLocalDeviceChange,
			UserID:   memberID,
			DeviceID: id.DeviceID("*"),
			Sender:   joiner,
		})
	}

	// A local joiner tracks the devices of every other member, as Synapse lists them all in
	// device_lists.changed for a newly joined room, a remote join's response members included, whose
	// events the iterator never sees.
	if joiner.Homeserver() == e.config.ServerName {
		for memberID := range roomMembers {
			if memberID == joiner {
				continue
			}
			tds = append(tds, &types.ToDevice{
				Type:     types.BabbleservLocalDeviceChange,
				UserID:   joiner,
				DeviceID: id.DeviceID("*"),
				Sender:   memberID,
			})
		}
	}

	return tds
}

// https://github.com/element-hq/synapse/issues/11374, m.device_list_edus are sent per spec:
// ... when that user joins a room which contains servers which are not already receiving updates for that user’s device list
func (e *DeviceJoinEventIterator) remoteDeviceChangesForJoin(roomID id.RoomID, joiner id.UserID) ([]*types.ToDevice, error) {
	roomServers, err := e.db.Rooms.RoomServers(e.ctx, roomID)
	if err != nil {
		return nil, err
	}
	userRooms, err := e.db.Rooms.GetUserJoinedMembershipsWithEncryption(e.ctx, joiner)
	if err != nil {
		return nil, err
	}
	serverRooms := make(map[string]types.Memberships, len(roomServers))
	for _, server := range roomServers {
		if server == e.config.ServerName {
			continue
		} else if serverRooms[server], err = e.db.Rooms.GetServerMemberships(e.ctx, server); err != nil {
			return nil, err
		}
	}
	servers := serversNewToUser(roomID, userRooms, serverRooms)
	if len(servers) == 0 {
		return nil, nil
	}

	snapshot, err := e.db.Accounts.GetLocalUserDevicesSnapshot(e.ctx, joiner)
	if err != nil || snapshot == nil {
		return nil, err
	}

	contents := make([]json.RawMessage, len(snapshot.Devices))
	for i, d := range snapshot.Devices {
		update := types.LocalDeviceListUpdate{Version: snapshot.Version, Device: &d.Device, Keys: d.Keys}
		contents[i] = exerrors.Must(json.Marshal(deviceListUpdateContent(joiner, d.Device.ID, update, nil)))
	}

	tds := make([]*types.ToDevice, 0, len(servers)*len(contents))
	for _, server := range servers {
		// Target is the server, not user, but we smuggle such updates through to-device
		// internally (see the DeviceChangeIterator).
		serverUserID := id.UserID("@:" + server)
		for _, content := range contents {
			tds = append(tds, &types.ToDevice{
				Type:    types.BabbleservRemoteDeviceListUpdate,
				UserID:  serverUserID,
				Content: content,
			})
		}
	}

	return tds, nil
}

// serversNewToUser returns the servers that share none of the user's encrypted rooms but the one
// they joined, so receive no updates of the user's device list yet
func serversNewToUser(joinedRoomID id.RoomID, userEncryptedRooms types.Memberships, serverRooms map[string]types.Memberships) []string {
	var servers []string
	for server, rooms := range serverRooms {
		shared := false
		for roomID := range rooms {
			if _, found := userEncryptedRooms[roomID]; found && roomID != joinedRoomID {
				shared = true
				break
			}
		}
		if !shared {
			servers = append(servers, server)
		}
	}
	slices.Sort(servers)
	return servers
}

// Leave events tell each local user, the leaver or another member of the room, of the other side
// once they no longer share an encrypted room (device_lists.left). Only local users are told, so
// every check is whether a local user still shares an encrypted room with another user, read from
// the local user's rows: a local leaver's for every member, otherwise each local member's for the
// leaver. A remote user's rooms are never read.
func (e *DeviceJoinEventIterator) localDeviceChangesForLeaveEvent(ev *types.Event) ([]*types.ToDevice, error) {
	roomMembers, err := e.db.Rooms.RoomMembers(e.ctx, ev.RoomID)
	if err != nil {
		return nil, err
	}
	leaver := ev.Sender
	var others []id.UserID
	for memberID := range roomMembers {
		if memberID != leaver && (e.isLocal(leaver) || e.isLocal(memberID)) {
			others = append(others, memberID)
		}
	}
	shared := make(map[id.UserID]bool, len(others))
	if e.isLocal(leaver) {
		if shared, err = e.db.Rooms.UsersSharingEncryptedRoom(e.ctx, leaver, others); err != nil {
			return nil, err
		}
	} else {
		for _, memberID := range others {
			sharing, err := e.db.Rooms.UsersSharingEncryptedRoom(e.ctx, memberID, []id.UserID{leaver})
			if err != nil {
				return nil, err
			}
			shared[memberID] = sharing[leaver]
		}
	}
	return leftDeviceChanges(e.isLocal, leaver, others, shared), nil
}

func (e *DeviceJoinEventIterator) isLocal(userID id.UserID) bool {
	return userID.Homeserver() == e.config.ServerName
}

// leftDeviceChanges tells the leaver and each other member, those of them that are local, of the other
// side when they no longer share an encrypted room, as shared reports for each member
func leftDeviceChanges(isLocal func(id.UserID) bool, leaver id.UserID, others []id.UserID, shared map[id.UserID]bool) []*types.ToDevice {
	var tds []*types.ToDevice
	for _, memberID := range others {
		if shared[memberID] {
			continue
		}
		if isLocal(leaver) {
			tds = append(tds, &types.ToDevice{
				Type:     types.BabbleservLocalDeviceLeft,
				Sender:   memberID,
				UserID:   leaver,
				DeviceID: id.DeviceID("*"),
			})
		}
		if isLocal(memberID) {
			tds = append(tds, &types.ToDevice{
				Type:     types.BabbleservLocalDeviceLeft,
				Sender:   leaver,
				UserID:   memberID,
				DeviceID: id.DeviceID("*"),
			})
		}
	}
	return tds
}
