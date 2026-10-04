// The send events transactions are the engine of Babbleserv, this contains the
// logic to authorize and ingest events from local homeserver users and events
// coming from other homeservers via federation.

package rooms

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/servers"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/databases/rooms/users"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SendEventsResult struct {
	transactionDuplicate bool
	versionFut           fdb.FutureKey
	change               notifier.Change

	Allowed  []*types.Event
	Rejected []RejectedEvent
}

type RejectedEvent struct {
	Event *types.Event
	Error error
}

// Store events handles writing out all the relevant event data into FoundationDB, assuming that all
// events passed in are already authenticated and carry their state contexts. Each accepted event
// clears the extremities it replaces. The caller then stores the room's current state with
// txnStoreRoomState.
func (r *RoomsDatabase) txnStoreEvents(
	ctx context.Context,
	txn fdb.Transaction,
	room *types.Room,
	evs []*types.Event,
	replacedExtremities map[id.EventID][]id.EventID,
	eventNotifications map[id.EventID]map[id.UserID]types.Notifications,
) (map[id.EventID]tuple.Versionstamp, error) {
	if len(evs) > types.MaxVersionstampUserVersion {
		return nil, fmt.Errorf("cannot store %d events in one transaction", len(evs))
	} else if len(evs) == 0 {
		return nil, nil
	}

	notifs := 0
	for _, u := range eventNotifications {
		notifs += len(u)
	}
	zerolog.Ctx(ctx).Debug().
		Int("events", len(evs)).
		Int("event_notifications", notifs).
		Msg("Storing batch of events")

	var version tuple.Versionstamp
	depthKey := r.KeyForRoomDepth(room.ID)
	depth := types.BytesToRoomDepth(txn.Get(depthKey).MustGet())

	newEvs := make([]*types.Event, 0, len(evs))
	for _, ev := range evs {
		if ev.Outlier {
			panic("cannot pass outliers to txnStoreEvents")
		} else if !ev.IsDuplicate {
			newEvs = append(newEvs, ev)
		}
	}
	// The versions this transaction gives its events. They cannot be read back before it commits,
	// FDB fails the read of a versionstamped value with error 1036, so an index keyed by the
	// version of an event in the batch takes it from here.
	batchVersions := r.events.TxnStoreEventRows(txn, true, newEvs...)
	batchThreadRoots := make(map[id.EventID]struct{})

	for _, ev := range newEvs {
		zerolog.Ctx(ctx).Trace().Any("event", ev).Msg("Storing event")

		eventTupBytes := types.EventTupToBytes(ev.EventTup())

		// This is the magic FDB version which is globally ordered, we index events by this
		version = batchVersions[ev.ID]

		// Rejected and soft failed events never appear to clients, so neither enters the room indices
		if ev.Rejected || ev.SoftFailed {
			continue
		}

		if ev.Depth > depth {
			// Update room depth
			txn.Set(depthKey, types.RoomDepthToBytes(ev.Depth))
			depth = ev.Depth
		}

		// Room indices
		// room/version -> EventTup, used to sync room events to clients
		txn.SetVersionstampedKey(r.events.KeyForRoomVersion(room.ID, version), eventTupBytes)
		if ev.Local {
			// local events room/version -> EventTup
			txn.SetVersionstampedKey(r.events.KeyForLocalRoomVersion(room.ID, version), eventTupBytes)
		}

		// Relation events indices
		relEvID, relType := ev.RelatesTo()
		if relEvID != "" {
			txn.SetVersionstampedKey(
				r.events.KeyForRoomRelation(room.ID, relEvID, version),
				tuple.Tuple{ev.ID.String(), []byte(relType)}.Pack(),
			)

			if relType == event.RelThread {
				// room-threads/root-ev-version -> root event ID - only if this
				// doesn't already exist (so the first reply in a thread creates).
				if rootVersion, found := batchVersions[relEvID]; found {
					if _, written := batchThreadRoots[relEvID]; !written {
						txn.SetVersionstampedKey(r.events.KeyForRoomThread(room.ID, rootVersion), []byte(relEvID))
						batchThreadRoots[relEvID] = struct{}{}
					}
				} else {
					relEvVersion := r.events.TxnLookupVersionForEventID(txn, relEvID)
					threadKey := r.events.KeyForRoomThread(room.ID, relEvVersion)
					if txn.Get(threadKey).MustGet() == nil {
						txn.Set(threadKey, []byte(relEvID))
					}
				}
			}

			if relType == event.RelAnnotation {
				// room-ev-reactions/rel-ev/uid/key
				// Note: dupe check is handled before we call storeEvents
				txn.Set(r.events.KeyForRoomReaction(room.ID, relEvID, ev.Sender, ev.ReactionKey()), []byte(ev.ID))
			}
		}

		// Store notification counts for local users
		if userNotifs, ok := eventNotifications[ev.ID]; ok {
			for userID, notif := range userNotifs {
				r.users.TxnStoreNotification(txn, userID, room.ID, version, notif)
			}
		}

		// Update room extremeties
		// This is where we handle the partial DAG ordering via prev_events
		// For each new event:
		//     store room/last/event_id empty key
		//     for each extremity it replaces, clear room/last/extremity_id
		// The contents of room/last/ are used at event creation time to populate
		// prev_events, thus any DAG split can be corrected by sending an event.
		for _, extremityID := range replacedExtremities[ev.ID] {
			r.events.TxnDeleteRoomExtremEventID(txn, room.ID, extremityID)
		}
		// Set this last, so rooms always have a last event
		r.events.TxnSetRoomExtremEventID(txn, room.ID, ev.ID)
	}

	if len(batchVersions) > 0 {
		// Bump the room version to the max
		txn.SetVersionstampedValue(r.KeyForRoomVersion(room.ID), types.MustVersionstampToBytes(version))
	}

	return batchVersions, nil
}

// txnStoreRoomState moves the room's current state through the prepared steps once the transaction
// stored their events: the contexts, a history row per step, the room record's fields, the server
// rows and local users' rows. The publish then holding more than publishMandatoryMaxBytes fails with
// types.ErrRoomTooLarge, otherwise the room record is written.
func (r *RoomsDatabase) txnStoreRoomState(
	ctx context.Context,
	txn fdb.Transaction,
	room *types.Room,
	prepared *preparedEvents,
	versions map[id.EventID]tuple.Versionstamp,
	changed *membershipChanges,
) error {
	stateBatch, steps := prepared.stateBatch, prepared.stateSteps
	previous := *room
	if len(steps) > 0 {
		room.CurrentState = steps[len(steps)-1].state
	}

	stateBatch.TxnWrite(txn, prepared.stateContexts()...)
	prepared.authGraph.Write(txn)
	diffedSteps, err := prepared.txnDiffStateSteps(txn, previous.CurrentState)
	if err != nil {
		return err
	}
	published := make([]publishStep, len(diffedSteps))
	for i, step := range diffedSteps {
		version, found := versions[step.eventID]
		if !found {
			return fmt.Errorf("current state moved by %s, which is not stored with it", step.eventID)
		}
		r.events.TxnStoreRoomStateStep(txn, room.ID, version, step.state)
		published[i] = publishStep{version: version, eventID: step.eventID, changes: step.changes}
	}

	room.StateRevision++
	if err := r.txnStoreRoomFields(prepared.eventsProvider, room, published); err != nil {
		return err
	}
	madeJoined := previous.LocalMembers == 0 && room.LocalMembers > 0
	if err := r.txnStoreServerRows(ctx, txn, room.ID, madeJoined, published, changed); err != nil {
		return err
	}
	if err := r.txnStoreLocalMemberRows(txn, room.ID, published, changed); err != nil {
		return err
	}
	if size, err := txn.GetApproximateSize().Get(); err != nil {
		return err
	} else if err := r.checkPublishSize(size); err != nil {
		return err
	}
	r.txnStoreRoom(txn, room, previous.MemberCount)
	return nil
}

// checkPublishSize fails a publish holding more than config.PublishMandatoryMaxBytes before the room
// record with types.ErrRoomTooLarge, leaving room under FoundationDB's 10 MB for the record and a
// send's extra writes.
func (r *RoomsDatabase) checkPublishSize(size int64) error {
	if size > config.PublishMandatoryMaxBytes {
		return fmt.Errorf("%w: %d bytes before the room record", types.ErrRoomTooLarge, size)
	}
	return nil
}

// publishStep is what one step of a publish changed in current state, written at its event's version
type publishStep struct {
	version tuple.Versionstamp
	eventID id.EventID
	changes []types.StateChange
}

func isJoinEvent(ev *types.Event) bool {
	return ev.Type == event.StateMember && ev.Membership() == event.MembershipJoin
}

// joinedDelta is what a member change adds to the room's joined members
func (r *RoomsDatabase) joinedDelta(change types.StateChange) int {
	switch was, is := change.OldMembership == event.MembershipJoin, change.NewMembership == event.MembershipJoin; {
	case is && !was:
		return 1
	case was && !is:
		return -1
	}
	return 0
}

var encryptionTup = types.StateTup{Type: event.StateEncryption}

// roomSummaryField is a room summary field, held by the state event of its type with an empty state
// key, under contentKey of its content
type roomSummaryField struct {
	contentKey string
	field      func(room *types.Room) *string
}

var roomSummaryFields = map[string]roomSummaryField{
	event.StateRoomName.Type:          {"name", func(room *types.Room) *string { return &room.Name }},
	event.StateTopic.Type:             {"topic", func(room *types.Room) *string { return &room.Topic }},
	event.StateRoomAvatar.Type:        {"url", func(room *types.Room) *string { return &room.AvatarURL }},
	event.StateCanonicalAlias.Type:    {"alias", func(room *types.Room) *string { return &room.CanonicalAlias }},
	event.StateJoinRules.Type:         {"join_rule", func(room *types.Room) *string { return &room.JoinRule }},
	event.StateHistoryVisibility.Type: {"history_visibility", func(room *types.Room) *string { return &room.HistoryVisibility }},
	event.StateGuestAccess.Type:       {"guest_access", func(room *types.Room) *string { return &room.GuestAccess }},
}

func (r *RoomsDatabase) roomSummaryFieldOf(tup types.StateTup) (roomSummaryField, bool) {
	if tup.StateKey != "" {
		return roomSummaryField{}, false
	}
	summary, found := roomSummaryFields[tup.Type.Type]
	return summary, found
}

// txnStoreRoomFields moves the room record's member counts and summary fields by the steps'
// changes, reading only the events that set a summary field.
func (r *RoomsDatabase) txnStoreRoomFields(
	eventsProvider *events.TxnEventsProvider,
	room *types.Room,
	steps []publishStep,
) error {
	for _, step := range steps {
		step.willGetChangedEvents(eventsProvider)
	}
	for _, step := range steps {
		for _, change := range step.changes {
			if change.Type == event.StateMember {
				delta := r.joinedDelta(change)
				room.MemberCount += delta
				if r.isLocalUser(id.UserID(change.StateKey)) {
					room.LocalMembers += delta
				}
				continue
			}
			if change.StateTup == encryptionTup {
				room.Encrypted = change.NewEventID != ""
			}
			summary, found := r.roomSummaryFieldOf(change.StateTup)
			if !found {
				continue
			}
			value := ""
			if change.NewEventID != "" {
				ev, err := eventsProvider.GetRequired(change.NewEventID)
				if err != nil {
					return err
				}
				value = gjson.GetBytes(ev.Content, summary.contentKey).String()
			}
			*summary.field(room) = value
		}
	}
	return nil
}

// willGetChangedEvents starts reading the events the changes set room summary fields to
func (step publishStep) willGetChangedEvents(eventsProvider *events.TxnEventsProvider) {
	for _, change := range step.changes {
		if _, found := roomSummaryFields[change.Type.Type]; found && change.StateKey == "" && change.NewEventID != "" {
			eventsProvider.WillGet(change.NewEventID)
		}
	}
}

// serverCrossing is a server's joined member count of a room crossing zero in a step, with the
// membership its server rows record
type serverCrossing struct {
	server     string
	membership types.MembershipTup
}

// countJoinedMembers moves each server's joined member count of the room by each step's member
// changes, and returns the servers whose count crosses zero in each step: joined with the step's
// first join of their members, left with its last loss, or with the step's event when that loss
// removed the member key.
func (r *RoomsDatabase) countJoinedMembers(roomID id.RoomID, counts map[string]int, steps []publishStep) [][]serverCrossing {
	crossings := make([][]serverCrossing, len(steps))
	for i, step := range steps {
		before := make(map[string]int)
		joins := make(map[string]types.StateChange)
		losses := make(map[string]types.StateChange)
		var order []string
		for _, change := range step.changes {
			delta := 0
			if change.Type == event.StateMember {
				delta = r.joinedDelta(change)
			}
			if delta == 0 {
				continue
			}
			server := id.UserID(change.StateKey).Homeserver()
			if _, found := before[server]; !found {
				before[server] = counts[server]
				order = append(order, server)
			}
			counts[server] = max(counts[server]+delta, 0)
			if _, found := joins[server]; !found && delta > 0 {
				joins[server] = change
			} else if delta < 0 {
				losses[server] = change
			}
		}
		for _, server := range order {
			switch {
			case before[server] == 0 && counts[server] > 0:
				join := joins[server]
				crossings[i] = append(crossings[i], serverCrossing{server, types.MembershipTup{
					EventID: join.NewEventID, RoomID: roomID, Membership: event.MembershipJoin,
				}})
			case before[server] > 0 && counts[server] == 0:
				eventID := losses[server].NewEventID
				if eventID == "" {
					eventID = step.eventID
				}

				crossings[i] = append(crossings[i], serverCrossing{server, types.MembershipTup{
					EventID: eventID, RoomID: roomID, Membership: event.MembershipLeave,
				}})
			}
		}
	}
	return crossings
}

// recordsServerChange reports whether a server crossing zero in a publish gets a membership change
// row: every crossing but other servers' joins in a publish making this server joined, as the
// federation sender starts every server's stream of the room at this server's join.
func (crossing serverCrossing) recordsServerChange(localServer string, madeJoined bool) bool {
	return !madeJoined || crossing.server == localServer || crossing.membership.Membership != event.MembershipJoin
}

// txnStoreServerRows writes each changed server's joined member count, and for a server whose count
// crosses zero its membership and membership change, see recordsServerChange.
func (r *RoomsDatabase) txnStoreServerRows(
	ctx context.Context,
	txn fdb.Transaction,
	roomID id.RoomID,
	madeJoined bool,
	steps []publishStep,
	changed *membershipChanges,
) error {
	var serverNames []string
	for _, step := range steps {
		for _, change := range step.changes {
			if change.Type == event.StateMember && r.joinedDelta(change) != 0 {
				serverNames = append(serverNames, id.UserID(change.StateKey).Homeserver())
			}
		}
	}
	slices.Sort(serverNames)
	serverNames = slices.Compact(serverNames)
	futures := r.servers.TxnReadJoinedCounts(txn, roomID, serverNames)
	counts := make(map[string]int, len(futures))
	for serverName, future := range futures {
		b, err := future.Get()
		if err != nil {
			return err
		}
		counts[serverName] = servers.JoinedCountOf(b)
	}
	before := maps.Clone(counts)

	crossings := r.countJoinedMembers(roomID, counts, steps)
	for _, serverName := range serverNames {
		if counts[serverName] != before[serverName] {
			r.servers.TxnSetJoinedCount(txn, roomID, serverName, counts[serverName])
		}
	}
	for i, step := range steps {
		for _, crossing := range crossings[i] {
			zerolog.Ctx(ctx).Debug().
				Str("server", crossing.server).
				Str("membership", string(crossing.membership.Membership)).
				Msg("Server membership of room changed")
			changed.servers[crossing.server] = struct{}{}
			r.servers.TxnStoreServerMembership(txn, roomID, crossing.server, crossing.membership)
			if crossing.recordsServerChange(r.config.ServerName, madeJoined) {
				r.servers.TxnStoreServerMembershipChange(txn, crossing.server, step.version, crossing.membership)
			}
		}
	}
	return nil
}

// txnStoreLocalMemberRows writes local users' rows of the steps' member changes, see
// localMemberWrites, reading the rows they replace first.
func (r *RoomsDatabase) txnStoreLocalMemberRows(
	txn fdb.Transaction,
	roomID id.RoomID,
	steps []publishStep,
	changed *membershipChanges,
) error {
	futures := make(map[id.UserID]fdb.FutureByteSlice)
	for _, step := range steps {
		for _, change := range step.changes {
			if userID := id.UserID(change.StateKey); change.Type == event.StateMember && r.isLocalUser(userID) {
				if _, found := futures[userID]; !found {
					futures[userID] = txn.Get(r.users.KeyForMembership(userID, roomID))
				}
			}
		}
	}
	stored, err := users.MembershipRowsOf(futures)
	if err != nil {
		return err
	}
	for _, write := range r.localMemberWrites(roomID, steps, stored) {
		changed.users[write.userID] = struct{}{}
		localKey := r.keyForLocalMember(roomID, write.userID)
		if write.row == nil {
			r.users.TxnDeleteMembership(txn, write.userID, roomID)
			txn.Clear(localKey)
			continue
		}
		r.users.TxnStoreMembership(txn, write.userID, roomID, *write.row)
		if write.row.Membership == event.MembershipJoin {
			txn.Set(localKey, types.MembershipTupToBytes(write.row.MembershipTup))
		} else {
			txn.Clear(localKey)
		}
		if write.changed {
			r.users.TxnStoreMembershipChange(txn, write.userID, write.version, write.row.MembershipTup)
		}
	}
	return nil
}

// localMemberWrite is a local user's member change as a publish writes it: the user's membership row
// and the room's local joined member, both cleared for a member key that left the room's state when
// row is nil, and when changed a membership change at version.
type localMemberWrite struct {
	userID  id.UserID
	row     *types.MembershipRow
	changed bool
	version tuple.Versionstamp
}

// localMemberWrites returns the writes of local users' member changes in step order, each at its
// step's version, given the rows stored before the publish, see writesMembershipChange. Remote users
// have no rows.
func (r *RoomsDatabase) localMemberWrites(
	roomID id.RoomID,
	steps []publishStep,
	stored map[id.UserID]*types.MembershipRow,
) []localMemberWrite {
	stored = maps.Clone(stored)
	var writes []localMemberWrite
	for _, step := range steps {
		for _, change := range step.changes {
			userID := id.UserID(change.StateKey)
			if change.Type != event.StateMember || !r.isLocalUser(userID) {
				continue
			}
			write := localMemberWrite{userID: userID, version: step.version}
			if change.NewEventID != "" {
				write.row = &types.MembershipRow{
					MembershipTup: types.MembershipTup{EventID: change.NewEventID, RoomID: roomID, Membership: change.NewMembership},
				}
			}
			write.changed = write.writesMembershipChange(stored[userID])
			stored[userID] = write.row
			writes = append(writes, write)
		}
	}
	return writes
}

// writesMembershipChange reports whether a local user's change gets a membership change row for
// sync, given the row it replaces: only when that held another event, so each change is delivered
// once, and never for a member key that left the room's state, which sync cannot express.
func (w localMemberWrite) writesMembershipChange(stored *types.MembershipRow) bool {
	return w.row != nil && (stored == nil || stored.EventID != w.row.EventID)
}

// txnGetOrCreateRoomForEvents returns the room events are sent to: the stored room, or the one the
// batch's create event creates, which must come first.
func (r *RoomsDatabase) txnGetOrCreateRoomForEvents(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	evs []*types.PartialEvent,
) (*types.Room, error) {
	room, err := r.txnGetRoom(txn, roomID)
	if errors.Is(err, types.ErrRoomNotFound) {
		return r.newRoomFromEvents(roomID, evs)
	}
	return room, err
}

func (r *RoomsDatabase) newRoomFromEvents(roomID id.RoomID, evs []*types.PartialEvent) (*types.Room, error) {
	if len(evs) == 0 || evs[0].Type != event.StateCreate {
		return nil, fmt.Errorf("%w: %s", types.ErrRoomNotFound, roomID)
	}

	room := types.Room{ID: roomID, CurrentState: state.EmptyContext}

	room.Version = createRoomVersion(evs[0])
	room.Type = gjson.GetBytes(evs[0].Content, "type").String()

	// https://spec.matrix.org/v1.14/client-server-api/#mroomcreate
	// Whether users on other servers can join this room. Defaults to true if key does not exist.
	res := gjson.GetBytes(evs[0].Content, "m\\.federate")
	var canFederate bool
	if res.Exists() {
		canFederate = res.Bool()
	} else {
		canFederate = true
	}
	room.Federated = canFederate

	return &room, nil
}

func createRoomVersion(createEv *types.PartialEvent) string {
	return gjson.GetBytes(createEv.Content, "room_version").String()
}

var errRoomNotFederated = errors.New("this room is not federated")

// Run final internal checks on an event before we accept it for storage, this
// is where we prevent duplicate annotations currently.
func (r *RoomsDatabase) txnCheckEventBeforeStore(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	ev *types.Event,
) error {
	// Quick sanity checks - should never happen!
	if ev.Type == event.StateCreate && util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) && ev.ID != ev.ImplicitCreateEventID() {
		return fmt.Errorf("v12 room ID does not match its create event")
	}
	if ev.RoomID != roomID {
		panic("event room ID does not match input room ID")
	} else if ev.RoomVersion == "" {
		panic("event room version is missing")
	}

	relEvID, relType := ev.RelatesTo()
	if relType == event.RelAnnotation {
		existng := txn.Get(r.events.KeyForRoomReaction(ev.RoomID, relEvID, ev.Sender, ev.ReactionKey())).MustGet()
		if existng != nil {
			return errors.New("duplicate reaction for this event/user")
		}
	}

	return nil
}

// Strip everything unexpected from an event's unsigned map before it is stored.
func (r *RoomsDatabase) stripEventUnsigned(ev *types.Event) {
	for k := range ev.Unsigned {
		switch k {
		case "invite_room_state", "knock_room_state":
			// OK!
		default:
			delete(ev.Unsigned, k)
		}
	}
}

func (r *RoomsDatabase) txnPreProcessEventUnsigned(
	txn fdb.ReadTransaction,
	eventsProvider *events.TxnEventsProvider,
	ev *types.Event,
	prevStateEventID id.EventID,
) error {
	r.stripEventUnsigned(ev)
	if ev.StateKey == nil {
		return nil
	}
	var prevStateEv *types.Event
	if prevStateEventID != "" {
		var err error
		if prevStateEv, err = eventsProvider.Get(prevStateEventID); err != nil {
			return fmt.Errorf("failed to get previous state event of %s: %w", ev.ID, err)
		}
	}
	if prevStateEv == nil {
		var err error
		if prevStateEv, err = r.txnGetLocalJoinerInvite(txn, ev); err != nil {
			return err
		}
	}
	if prevStateEv != nil {
		ev.SetUnsigned("prev_content", prevStateEv.Content)
		// Note: this is not referenced anywhere in the spec but synapse does it and complement tests it
		ev.SetUnsigned("prev_sender", prevStateEv.Sender)
	}
	return nil
}

// txnGetLocalJoinerInvite returns the invite a local user's join follows when the state before the
// join holds no member event of theirs: an outlier invite received while this server was not joined.
func (r *RoomsDatabase) txnGetLocalJoinerInvite(txn fdb.ReadTransaction, ev *types.Event) (*types.Event, error) {
	if !isJoinEvent(ev) {
		return nil, nil
	}
	userID := id.UserID(*ev.StateKey)
	if !r.isLocalUser(userID) {
		return nil, nil
	}
	memberships, err := r.txnMemberships(txn, ev.RoomID, []id.UserID{userID})
	if err != nil {
		return nil, err
	} else if membership, found := memberships[userID]; !found || membership.Membership != event.MembershipInvite {
		return nil, nil
	} else {
		return r.events.TxnGetEvent(txn, membership.EventID), nil
	}
}

// stateStep is the room's current state after an event stored in the same transaction moved it.
type stateStep struct {
	eventID id.EventID
	state   types.StateHash
}

// stateSteps records each move of a room's current state during one send.
type stateSteps struct {
	current types.StateHash
	steps   []stateStep
}

func (s *stateSteps) advance(eventID id.EventID, next types.StateHash) {
	if next == s.current {
		return
	}
	s.steps = append(s.steps, stateStep{eventID: eventID, state: next})
	s.current = next
}

// diffedStep is a state step with the changes it made to current state
type diffedStep struct {
	stateStep
	changes []types.StateChange
}

// membershipChanges are the users and servers whose rows of the room a send wrote
type membershipChanges struct {
	users   map[id.UserID]struct{}
	servers map[string]struct{}
}

func newMembershipChanges() *membershipChanges {
	return &membershipChanges{
		users:   make(map[id.UserID]struct{}, 1),
		servers: make(map[string]struct{}, 1),
	}
}

func (c *membershipChanges) userIDs() []id.UserID {
	return slices.Collect(maps.Keys(c.users))
}

func (c *membershipChanges) serverNames() []string {
	return slices.Collect(maps.Keys(c.servers))
}
