// The send events transactions are the engine of Babbleserv, this contains the
// logic to authorize and ingest events from local homeserver users and events
// coming from other homeservers via federation.

package rooms

import (
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SendEventsResult struct {
	versionFut fdb.FutureKey
	change     notifier.Change

	Allowed  []*types.Event
	Rejected []RejectedEvent
}

type RejectedEvent struct {
	Event *types.Event
	Error error
}

func newSendEventsResults(
	versionFut fdb.FutureKey,
	room *types.Room,
	allowedEvs []*types.Event,
	rejectedEvs []RejectedEvent,
	changedUsers map[id.UserID]struct{},
	changedServers map[string]struct{},
) *SendEventsResult {
	changedUserIDs := make([]id.UserID, 0, len(changedUsers))
	for uid := range changedUsers {
		changedUserIDs = append(changedUserIDs, uid)
	}
	changedServerNames := make([]string, 0, len(changedServers))
	for serverName := range changedServers {
		changedServerNames = append(changedServerNames, serverName)
	}

	var change notifier.Change

	if len(allowedEvs) > 0 {
		change = notifier.Change{
			RoomIDs: []id.RoomID{room.ID},
			UserIDs: changedUserIDs,
			Servers: changedServerNames,
			// Note: only pass the last event ID here to minimize notifier pubsub traffic
			EventIDs: []id.EventID{allowedEvs[len(allowedEvs)-1].ID},
		}
	}

	return &SendEventsResult{
		versionFut: versionFut,
		change:     change,

		Allowed:  allowedEvs,
		Rejected: rejectedEvs,
	}
}

func getUserIDList(evs []*types.PartialEvent) []id.UserID {
	userIDMap := make(map[id.UserID]struct{}, len(evs))
	for _, ev := range evs {
		userIDMap[ev.Sender] = struct{}{}
		if ev.Type == event.StateMember {
			userIDMap[id.UserID(*ev.StateKey)] = struct{}{}
		}
	}
	userIDs := make([]id.UserID, 0, len(userIDMap))
	for userID := range userIDMap {
		userIDs = append(userIDs, userID)
	}
	return userIDs
}

func (r *RoomsDatabase) handleSendEventsResults(res *SendEventsResult, log zerolog.Logger) (*SendEventsResult, error) {
	r.notifier.SendChange(res.change)

	rlog := log.Info().
		Object("change", res.change).
		Int("events_allowed", len(res.Allowed)).
		Int("events_rejected", len(res.Rejected))
	if len(res.Allowed) > 0 {
		rlog = rlog.Any("versionstamp", types.DecodeRawVersionstamp(res.versionFut.MustGet()))
	}
	rlog.Msg("Sent events")

	return res, nil
}

func (r *RoomsDatabase) txnGetOrCreateRoomForEvents(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	evs []*types.PartialEvent,
) (*types.Room, error) {
	if roomBytes := txn.Get(r.KeyForRoom(roomID)).MustGet(); roomBytes != nil {
		return types.MustNewRoomFromBytes(roomBytes, roomID), nil
	}

	// If roomBytes is nil we must be creating the room, which means the first input event
	// *must* be the create event.
	if evs[0].Type != event.StateCreate {
		return nil, fmt.Errorf("%w: %s", types.ErrRoomNotFound, roomID)
	}

	room := types.Room{ID: roomID}

	room.Version = gjson.GetBytes(evs[0].Content, "room_version").String()
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

// Run final internal checks on an event before we accept it for storage, this
// is where we prevent duplicate annotations currently.
func (r *RoomsDatabase) txnCheckEventBeforeStore(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	ev *types.Event,
) error {
	// Quick sanity checks - should never happen!
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

// Gets the room state at one or more event IDs. If multiple event IDs are provided then state
// resolution is applied to merge the combined states.
func (r *RoomsDatabase) txnResolveStateForEvents(
	txn fdb.ReadTransaction,
	eventMap map[id.EventID]struct{},
	roomVersion string,
	eventsProvider *events.TxnEventsProvider,
) (types.StateMap, error) {
	// Gather all the state events we have
	allStateEvents := make([]*types.Event, 0, len(eventMap))
	for evID := range eventMap {
		allStateEvents = append(allStateEvents, eventsProvider.MustGet(evID))
	}

	// Now get the auth chain for those events
	allAuthEvents, err := r.events.TxnGetAuthChainForEvents(txn, allStateEvents, eventsProvider)
	if err != nil {
		return nil, err
	}

	evMap := util.MergeEventsMap(allStateEvents, allAuthEvents)
	resolvedPDUs, err := gomatrixserverlib.ResolveConflicts(
		gomatrixserverlib.RoomVersion(roomVersion),
		util.EventsToPDUs(allStateEvents),
		util.EventsToPDUs(allAuthEvents),
		func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return senderID.ToUserID(), nil
		},
		func(eventID string) bool {
			ev := evMap[id.EventID(eventID)]
			return ev.Outlier || ev.Rejected
		},
	)
	if err != nil {
		return nil, err
	}

	// Turn the resolved state back into our stateMap/memberMap
	resolvedStateMap := make(types.StateMap, len(resolvedPDUs))
	for _, pdu := range resolvedPDUs {
		pduEv := pdu.(types.EventPDU).Event()
		resolvedStateMap[types.StateTup{
			Type:     pduEv.Type,
			StateKey: *pduEv.StateKey,
		}] = pduEv.ID
	}

	return resolvedStateMap, nil
}

// Pre-process the unsigned map for a given event before it gets stored, notably this just removes
// everything unexpected and injects prev_content for state events.
func (r *RoomsDatabase) txnPreProcessEventUnsigned(
	txn fdb.ReadTransaction,
	eventsProvider *events.TxnEventsProvider,
	ev *types.Event,
) {
	for k := range ev.Unsigned {
		switch k {
		case "invite_room_state", "knock_room_state":
			// OK!
		default:
			delete(ev.Unsigned, k)
		}
	}

	if ev.StateKey == nil {
		return
	}

	stateTup := types.StateTup{
		Type:     ev.Type,
		StateKey: *ev.StateKey,
	}
	currentEv := r.events.TxnGetCurrentRoomStateEvent(txn, ev.RoomID, stateTup, eventsProvider)
	if currentEv == nil {
		// If we're joining now and the joiner is local - lookup any current membership for the
		// room which may point to an outlier event to pull prev_content from.
		userID := id.UserID(*ev.StateKey)
		if ev.Membership() == event.MembershipJoin && userID.Homeserver() == r.config.ServerName {
			currentMembership := r.users.TxnGetMembership(txn, userID, ev.RoomID)
			if currentMembership != nil && currentMembership.Membership == event.MembershipInvite {
				currentEv = r.events.TxnGetEvent(txn, currentMembership.EventID)
			}
		}
	}
	if currentEv == nil {
		return
	}

	ev.SetUnsigned("prev_content", currentEv.Content)
	// Note: this is not referenced anywhere in the spec but synapse does it and complement tests it
	ev.SetUnsigned("prev_sender", currentEv.Sender)

	// Internal cache of the prev state event object, used when updating room below
	ev.PrevStateEvent = currentEv
}

func (r *RoomsDatabase) updateRoomForStateEvent(room *types.Room, ev *types.Event) bool {
	var changed bool
	switch ev.Type {
	case event.StateRoomName:
		room.Name = gjson.GetBytes(ev.Content, "name").String()
		changed = true
	case event.StateTopic:
		room.Topic = gjson.GetBytes(ev.Content, "topic").String()
		changed = true
	case event.StateRoomAvatar:
		room.AvatarURL = gjson.GetBytes(ev.Content, "url").String()
		changed = true
	case event.StateMember:
		if ev.Membership() == event.MembershipJoin {
			// We're joining new if no prev or prev wasn't join
			if ev.PrevStateEvent == nil || ev.PrevStateEvent.Membership() != event.MembershipJoin {
				room.MemberCount++
			}
		} else {
			// We're leaving if prev was join
			if ev.PrevStateEvent != nil && ev.PrevStateEvent.Membership() == event.MembershipJoin {
				room.MemberCount--
			}
		}
	}

	return changed
}
