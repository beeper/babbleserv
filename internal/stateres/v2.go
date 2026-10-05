// Adapted from gomatrixserverlib's state resolution v2 implementation.
// TODO: Reconsider events rejected against their state, as the spec requires.
// Like Synapse, we currently exclude them; accepting them also requires repairing
// their stored rejection status, state contexts and dependent events.

package stateres

import (
	"cmp"
	"container/heap"
	"fmt"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const creatorPowerLevel = 100

var (
	createTup      = types.StateTup{Type: event.StateCreate}
	powerLevelsTup = types.StateTup{Type: event.StatePowerLevels}
)

type resolver struct {
	roomVersion        string
	events             *eventCache
	partial            types.StateMap
	common             func(types.StateTup) (id.EventID, error)
	powerLevelContents map[id.EventID]*gomatrixserverlib.PowerLevelContent
}

func newResolver(roomVersion string, getEvent GetEventFunc) *resolver {
	return &resolver{
		roomVersion: roomVersion,
		events: &eventCache{
			getEvent: getEvent,
			events:   make(map[id.EventID]*types.Event),
		},
		powerLevelContents: make(map[id.EventID]*gomatrixserverlib.PowerLevelContent),
	}
}

func (r *resolver) lookupPartial(tup types.StateTup) (id.EventID, error) {
	if eventID, ok := r.partial[tup]; ok {
		return eventID, nil
	}
	if r.common != nil {
		return r.common(tup)
	}
	return "", nil
}

func (r *resolver) run(fullConflictedSet map[id.EventID]*types.Event) error {

	// Spec steps 1 and 2: power events and their auth ancestors, authorized on the unconflicted
	// state.
	powerEvents, err := r.reverseTopologicalPowerOrder(fullConflictedSet)
	if err != nil {
		return err
	}
	if err := r.iterativeAuthChecks(powerEvents); err != nil {
		return err
	}

	ordered := make(map[id.EventID]struct{}, len(powerEvents))
	for _, ev := range powerEvents {
		ordered[ev.ID] = struct{}{}
	}
	// Spec steps 3 and 4: everything else, in mainline order of the resolved power levels.
	others := make([]*types.Event, 0, len(fullConflictedSet)-len(powerEvents))
	for eventID, ev := range fullConflictedSet {
		if _, ok := ordered[eventID]; !ok {
			others = append(others, ev)
		}
	}
	if err := r.mainlineOrder(others); err != nil {
		return err
	}
	if err := r.iterativeAuthChecks(others); err != nil {
		return err
	}

	return nil
}

// isPowerEvent follows the spec's definition of a power event, one that might remove someone's
// ability to do something in the room, and adds the create event as Synapse does.
func isPowerEvent(ev *types.Event) bool {
	switch ev.Type {
	case event.StateCreate, event.StatePowerLevels, event.StateJoinRules:
		return *ev.StateKey == ""
	case event.StateMember:
		if *ev.StateKey == "" || *ev.StateKey == ev.Sender.String() {
			return false
		}
		membership := ev.Membership()
		return membership == event.MembershipLeave || membership == event.MembershipBan
	}
	return false
}

type powerSortKey struct {
	powerLevel int64
	timestamp  int64
	eventID    id.EventID
}

func (k powerSortKey) less(other powerSortKey) bool {
	if k.powerLevel != other.powerLevel {
		return k.powerLevel > other.powerLevel
	}
	if k.timestamp != other.timestamp {
		return k.timestamp < other.timestamp
	}
	return k.eventID < other.eventID
}

type powerSortHeap []powerSortKey

func (h powerSortHeap) Len() int           { return len(h) }
func (h powerSortHeap) Less(i, j int) bool { return h[i].less(h[j]) }
func (h powerSortHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *powerSortHeap) Push(x any)        { *h = append(*h, x.(powerSortKey)) }
func (h *powerSortHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

// reverseTopologicalPowerOrder takes the power events of the full conflicted set together with
// their auth ancestors inside the full conflicted set, and returns them in the reverse topological
// power ordering: auth events before the events citing them, ties broken by greater sender power
// level, then earlier origin_server_ts, then smaller event ID.
func (r *resolver) reverseTopologicalPowerOrder(fullConflictedSet map[id.EventID]*types.Event) ([]*types.Event, error) {
	authEdges := make(map[id.EventID][]id.EventID)
	var pending []*types.Event
	for _, ev := range fullConflictedSet {
		if isPowerEvent(ev) {
			pending = append(pending, ev)
		}
	}
	for len(pending) > 0 {
		ev := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, seen := authEdges[ev.ID]; seen {
			continue
		}
		edges := make([]id.EventID, 0, len(ev.AuthEventIDs))
		for _, authEventID := range ev.AuthEventIDs {
			authEv, ok := fullConflictedSet[authEventID]
			if !ok || slices.Contains(edges, authEventID) {
				continue
			}
			edges = append(edges, authEventID)
			pending = append(pending, authEv)
		}
		authEdges[ev.ID] = edges
	}

	keys := make(map[id.EventID]powerSortKey, len(authEdges))
	remainingAuth := make(map[id.EventID]int, len(authEdges))
	citedBy := make(map[id.EventID][]id.EventID, len(authEdges))
	ready := make(powerSortHeap, 0, len(authEdges))
	for eventID, edges := range authEdges {
		ev := fullConflictedSet[eventID]
		powerLevel, err := r.senderPowerLevel(ev)
		if err != nil {
			return nil, err
		}
		keys[eventID] = powerSortKey{powerLevel: powerLevel, timestamp: ev.Timestamp, eventID: eventID}
		remainingAuth[eventID] = len(edges)
		for _, authEventID := range edges {
			citedBy[authEventID] = append(citedBy[authEventID], eventID)
		}
		if len(edges) == 0 {
			ready = append(ready, keys[eventID])
		}
	}
	heap.Init(&ready)

	ordered := make([]*types.Event, 0, len(authEdges))
	for ready.Len() > 0 {
		next := heap.Pop(&ready).(powerSortKey)
		ordered = append(ordered, fullConflictedSet[next.eventID])
		for _, eventID := range citedBy[next.eventID] {
			remainingAuth[eventID]--
			if remainingAuth[eventID] == 0 {
				heap.Push(&ready, keys[eventID])
			}
		}
	}
	return ordered, nil
}

// senderPowerLevel is the sender's power level according to the power levels event among the
// event's auth events, or the creator level if there is none and the sender created the room.
func (r *resolver) senderPowerLevel(ev *types.Event) (int64, error) {
	var createEv *types.Event
	if util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.PrivilegedCreators) {
		var err error
		createEv, err = r.events.get(ev.ImplicitCreateEventID())
		if err != nil {
			return 0, err
		}
		if createEv != nil && slices.Contains(gomatrixserverlib.CreatorsFromCreateEvent(createEv.PDU()), ev.Sender.String()) {
			return gomatrixserverlib.CreatorPowerLevel, nil
		}
	}
	powerLevelsEv, err := r.citedAuthEvent(ev, powerLevelsTup, false)
	if err != nil {
		return 0, err
	} else if powerLevelsEv != nil {
		if content := r.powerLevelContent(powerLevelsEv); content != nil {
			return content.UserLevel(spec.SenderID(ev.Sender)), nil
		}
		return 0, nil
	}
	if citedCreateEv, err := r.citedAuthEvent(ev, createTup, false); err != nil {
		return 0, err
	} else if citedCreateEv != nil {
		createEv = citedCreateEv
	}
	if createEv != nil && r.isCreator(createEv, ev.Sender) {
		return creatorPowerLevel, nil
	}
	return 0, nil
}

func (r *resolver) isCreator(createEv *types.Event, userID id.UserID) bool {
	if util.RoomVersionHas(r.roomVersion, gomatrixserverlib.IRoomVersion.CreatorInCreateEvent) {
		return gjson.GetBytes(createEv.Content, "creator").String() == userID.String()
	}
	return createEv.Sender == userID
}

// powerLevelContent returns nil for unparseable content.
func (r *resolver) powerLevelContent(ev *types.Event) *gomatrixserverlib.PowerLevelContent {
	if content, ok := r.powerLevelContents[ev.ID]; ok {
		return content
	}
	var content *gomatrixserverlib.PowerLevelContent
	if parsed, err := gomatrixserverlib.NewPowerLevelContentFromEvent(ev.PDU()); err == nil {
		content = &parsed
	}
	r.powerLevelContents[ev.ID] = content
	return content
}

type mainlineSortKey struct {
	position  int
	timestamp int64
	eventID   id.EventID
}

func (k mainlineSortKey) compare(other mainlineSortKey) int {
	return cmp.Or(
		cmp.Compare(k.position, other.position),
		cmp.Compare(k.timestamp, other.timestamp),
		cmp.Compare(k.eventID, other.eventID),
	)
}

// mainlineOrder sorts events by the mainline of the resolved power levels event: the chain of
// power levels events reached by following each one's auth events back to the start of the room.
// An event's position is that of the first mainline event on its own power levels chain, with the
// oldest mainline event first and events that never reach the mainline before all of them, then
// ties broken by earlier origin_server_ts and smaller event ID.
func (r *resolver) mainlineOrder(evs []*types.Event) error {
	if len(evs) == 0 {
		return nil
	}

	var mainline []id.EventID
	eventID, err := r.lookupPartial(powerLevelsTup)
	if err != nil {
		return err
	}
	if eventID != "" {
		current, err := r.events.get(eventID)
		if err != nil {
			return err
		}
		for current != nil {
			mainline = append(mainline, current.ID)
			if current, err = r.citedAuthEvent(current, powerLevelsTup, false); err != nil {
				return err
			}
		}
	}
	positions := make(map[id.EventID]int, len(mainline))
	for i, eventID := range mainline {
		positions[eventID] = len(mainline) - i
	}

	keys := make(map[id.EventID]mainlineSortKey, len(evs))
	for _, ev := range evs {
		position, err := r.mainlinePosition(ev, positions)
		if err != nil {
			return err
		}
		keys[ev.ID] = mainlineSortKey{position: position, timestamp: ev.Timestamp, eventID: ev.ID}
	}
	slices.SortFunc(evs, func(a, b *types.Event) int {
		return keys[a.ID].compare(keys[b.ID])
	})
	return nil
}

func (r *resolver) mainlinePosition(ev *types.Event, positions map[id.EventID]int) (int, error) {
	for current := ev; current != nil; {
		if position, ok := positions[current.ID]; ok {
			return position, nil
		}
		var err error
		if current, err = r.citedAuthEvent(current, powerLevelsTup, false); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func (r *resolver) iterativeAuthChecks(evs []*types.Event) error {
	for _, ev := range evs {
		if ev.Rejected {
			continue
		}
		authErr, err := r.authorize(ev)
		if err != nil {
			return err
		}
		if authErr == nil {
			r.partial[ev.StateTup()] = ev.ID
		}
	}
	return nil
}

func (r *resolver) authorize(ev *types.Event) (authErr, err error) {
	return util.Authorize(ev, func(tup types.StateTup) (*types.Event, error) {
		return r.authEvent(ev, tup)
	})
}

// authEvent serves the auth events for one event from the partial state. Per the spec's iterative
// auth checks, a tuple the partial state lacks, or holds a rejected event for, falls back to the
// event's own auth event for it, unless that auth event was rejected too.
func (r *resolver) authEvent(ev *types.Event, tup types.StateTup) (*types.Event, error) {
	eventID, err := r.lookupPartial(tup)
	if err != nil {
		return nil, err
	}
	if tup == createTup && util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) {
		eventID = ev.ImplicitCreateEventID()
	}
	if eventID != "" {
		stateEv, err := r.events.get(eventID)
		if err != nil {
			return nil, err
		} else if stateEv == nil {
			return nil, fmt.Errorf("state event %s for (%s, %q) not found", eventID, tup.Type.Type, tup.StateKey)
		} else if !stateEv.Rejected {
			return stateEv, nil
		}
	}
	return r.citedAuthEvent(ev, tup, true)
}

// citedAuthEvent returns the first of the event's auth events that is the state event for tup.
func (r *resolver) citedAuthEvent(ev *types.Event, tup types.StateTup, skipRejected bool) (*types.Event, error) {
	for _, authEventID := range ev.AuthEventIDs {
		authEv, err := r.events.get(authEventID)
		if err != nil {
			return nil, err
		}
		if authEv != nil && !(skipRejected && authEv.Rejected) && authEv.StateKey != nil && authEv.StateTup() == tup {
			return authEv, nil
		}
	}
	return nil, nil
}
