package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// stateDAGStrategy runs jobs of state DAG rooms (MSC4242): each join replays here the state DAG the
// resident serves with it, oldest first, and the PDUs publish as federated sends in chunks, those
// citing state events without state here, or prev events not stored, after their gap fill
type stateDAGStrategy struct{}

func (stateDAGStrategy) collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error) {
	return collectWorks(ctx, run, targets, stateDAGWorkKinds(run))
}

func stateDAGWorkKinds(run *jobRun) workKinds {
	return workKinds{
		join: func(target *types.IngestTarget) ingestWork {
			return &stateDAGJoin{joinWork: joinWork{run: run, target: target}}
		},
		pdus: func(pdus []*types.IngestTarget, gap bool) ingestWork {
			return &stateDAGPDUs{run: run, pdus: pdus, gap: gap}
		},
	}
}

func sortTargetsTopologically(targets []*types.IngestTarget) []*types.IngestTarget {
	byID := make(map[id.EventID]*types.IngestTarget, len(targets))
	evs := make([]*types.Event, len(targets))
	for i, target := range targets {
		byID[target.Event.ID] = target
		evs[i] = target.Event
	}
	return targetsOf(rooms.SortEventsTopologically(evs), byID)
}

// Most tips of a served state DAG a join publishes as state extremities beside it, each a row its
// publish writes: past it the room is not joined
const maxStateDAGJoinTips = 1000

// stateDAGJoin is a join to a state DAG room. Phase A sends the stored join event in a send_join and
// stages the state DAG its response streams, until a server's connects the join to the create event.
// Phase B replays it, giving its events state here, then builds the state before the join. A join to
// a room this server got into meanwhile is a member's: the events its prev state events reach that
// have no state here fill a gap, replayed and published ahead of it, while the tips of the state DAG
// are left to arrive as PDUs.
type stateDAGJoin struct {
	joinWork
	// Of the last response: its state DAG's events, and whether any of them cites each, which
	// leaves the tips uncited; the events it gave, their bytes, and those verified; its name and
	// alias events; the events not staged, the join and those citing it or another not staged
	served        map[id.EventID]bool
	received      int
	receivedBytes int
	seen          map[id.EventID]struct{}
	naming        []*types.Event
	skipped       map[id.EventID]struct{}
	// Whether this server was in the room as the join's state was built
	member bool
}

func (w *stateDAGJoin) fetch(ctx context.Context) error {
	return w.sendJoin(ctx)
}

func (w *stateDAGJoin) build(ctx context.Context) error {
	joinEv := w.target.Event
	var err error
	if w.member, err = w.run.jobs.db.Rooms.IsServerJoined(ctx, w.run.roomID, w.run.jobs.config.ServerName); err != nil {
		return err
	}
	tips := w.tips()
	from := joinEv.AuthDependencyIDs()
	if !w.member {
		if len(tips) > maxStateDAGJoinTips {
			return w.run.fail(ctx, w.target, tooManyTipsFailure(len(tips), maxStateDAGJoinTips))
		}
		from = append(from, tips...)
	}
	if err := w.run.jobs.db.Rooms.ReplayStateDAG(ctx, w.run.roomID, rooms.StateDAGReplay{
		RoomVersion: joinEv.RoomVersion,
		From:        from,
		Pending:     []id.EventID{joinEv.ID},
		Publish:     w.member,
	}); err != nil {
		return err
	}
	if w.join, err = w.run.jobs.db.NewStateDAGRemoteJoin(ctx, joinEv, tips); err != nil {
		return err
	}
	return w.joinWork.build(ctx)
}

// publish publishes the join, after, for a member's, the events it reaches that its replay gave
// state, which no published event may cite before they publish
func (w *stateDAGJoin) publish(ctx context.Context) error {
	if w.member {
		if err := w.run.publishStateDAG(ctx, nil, w.target.Event); err != nil {
			return err
		}
	}
	return w.joinWork.publish(ctx)
}

// sendJoin sends the stored join event to the target's servers until one serves a state DAG that
// connects the join to the create event, staging what each serves, as the MSC requires. A send that
// may have landed is sent again, across the servers, until the join times out, and a server refusing
// one that may have landed has the target replaced with a fresh join, unless a server answered it,
// so accepted it. A server whose state DAG does not connect the join, or that serves none, is not
// asked again: once no server is left the join fails.
func (w *stateDAGJoin) sendJoin(ctx context.Context) error {
	log := zerolog.Ctx(ctx).With().Stringer("event_id", w.target.Event.ID).Logger()
	incomplete := make(map[string]struct{})
	for attempt := 0; ; attempt++ {
		servers := slices.DeleteFunc(slices.Clone(w.target.Servers), func(server string) bool {
			_, found := incomplete[server]
			return found
		})
		if len(servers) == 0 {
			return w.run.fail(ctx, w.target, incompleteStateDAGFailure())
		}
		server := servers[attempt%len(servers)]
		before, err := w.mayLand(ctx)
		if err != nil {
			return err
		}
		w.served, w.received, w.receivedBytes, w.seen, w.naming = make(map[id.EventID]bool), 0, 0, make(map[id.EventID]struct{}), nil
		w.skipped = map[id.EventID]struct{}{w.target.Event.ID: {}}
		var stageErr error
		err = w.run.jobs.fetcher.SendJoinStateDAG(ctx, server, w.target.Event, w.timesOut(), func(raws []json.RawMessage) error {
			stageErr = w.stage(ctx, raws)
			return stageErr
		})
		var failure *types.IngestFailure
		switch {
		case errors.As(stageErr, &failure):
			return w.run.fail(ctx, w.target, failure)
		case stageErr != nil:
			return stageErr
		case errors.Is(err, errBadSendJoinResponse):
			log.Warn().Err(err).Str("server", server).Msg("Server answered the join with no state DAG")
			w.answered = true
			incomplete[server] = struct{}{}
			continue
		case err != nil:
			if err := w.sendFailed(ctx, before, server, attempt, err); errors.Is(err, errResendRefused) {
				return w.replaceJoin(ctx, err)
			} else if err != nil {
				return err
			}
			continue
		}
		w.answered = true
		util.ReportProgress(ctx)
		missing, err := w.run.jobs.db.Rooms.MissingStateDAGEvents(ctx, w.target.Event)
		if err != nil {
			return err
		} else if len(missing) == 0 {
			log.Info().Str("server", server).Int("state_dag", w.received).Msg("Staged the state DAG served with the join")
			return nil
		}
		log.Warn().
			Str("server", server).
			Int("missing", len(missing)).
			Stringer("missing_event_id", missing[0]).
			Msg("Server's state DAG does not connect the join to the create event")
		incomplete[server] = struct{}{}
	}
}

// stage verifies a chunk of the state DAG a send_join response serves and stages its state events,
// skipping, before their signatures are checked, those stored already. Events citing the join, or
// one skipped so, are newer than it: they cannot have state before it publishes, so arrive as PDUs
// rather than being staged. It records what the state DAG cites, for its tips, and names the room
// for BabbleBot's notices. Only a chunk holding events of the room reports progress, so that a
// server serving junk cannot keep the job going.
func (w *stateDAGJoin) stage(ctx context.Context, raws []json.RawMessage) error {
	limit := w.run.jobs.config.Rooms.Ingest.MaxJoinEvents
	w.received += len(raws)
	for _, raw := range raws {
		w.receivedBytes += len(raw)
	}
	if w.received > limit {
		return tooLargeFailure("state DAG has over", limit, limit)
	} else if w.receivedBytes > limit*util.MaxPDUBytes {
		return tooLargeFailure("state DAG has over the bytes of", limit, limit)
	}
	roomEvs := parseStateDAGEvents(ctx, raws, w.target.Event.RoomVersion, w.run.roomID)
	if len(roomEvs) > 0 {
		defer util.ReportProgress(ctx)
	}
	parsed := w.skipNewerThanJoin(roomEvs)
	staged, stored, err := w.run.stageUnstored(ctx, parsed, w.seen, 0)
	if err != nil {
		return err
	}
	kept := make(map[id.EventID]*types.Event, len(parsed))
	for _, ev := range staged {
		kept[ev.ID] = ev
	}
	for _, ev := range parsed {
		if _, found := stored[ev.ID]; found && ev.StateKey != nil {
			kept[ev.ID] = ev
		}
		if keptEv := kept[ev.ID]; keptEv != nil {
			w.cite(keptEv)
			if (keptEv.Type == event.StateRoomName || keptEv.Type == event.StateCanonicalAlias) && *keptEv.StateKey == "" {
				w.naming = append(w.naming, keptEv)
			}
		}
	}
	w.run.setRoomName(roomDisplayName(w.naming))
	return nil
}

// skipNewerThanJoin leaves out of served events, in the topological order they are served in, the
// join and those citing it or another left out
func (w *stateDAGJoin) skipNewerThanJoin(evs []*types.Event) []*types.Event {
	return slices.DeleteFunc(evs, func(ev *types.Event) bool {
		_, skipped := w.skipped[ev.ID]
		if skipped || slices.ContainsFunc(ev.PrevStateEventIDs, func(prevID id.EventID) bool {
			_, found := w.skipped[prevID]
			return found
		}) {
			w.skipped[ev.ID] = struct{}{}
			return true
		}
		return false
	})
}

func (w *stateDAGJoin) cite(ev *types.Event) {
	if _, found := w.served[ev.ID]; !found {
		w.served[ev.ID] = false
	}
	for _, prevID := range ev.PrevStateEventIDs {
		w.served[prevID] = true
	}
}

// tips are the events of the last state DAG served that none of its events cites
func (w *stateDAGJoin) tips() []id.EventID {
	var tips []id.EventID
	for _, eventID := range slices.Sorted(maps.Keys(w.served)) {
		if !w.served[eventID] {
			tips = append(tips, eventID)
		}
	}
	return tips
}

// parseStateDAGEvents parses state DAG events another server served, giving each its ID, without
// those of other rooms or that cannot be parsed
func parseStateDAGEvents(ctx context.Context, raws []json.RawMessage, roomVersion string, roomID id.RoomID) []*types.Event {
	return slices.DeleteFunc(eventsInRoom(ctx, util.ParseRemoteEvents(ctx, raws, roomVersion), roomID), func(ev *types.Event) bool {
		eventID, err := util.GetEventReferenceHash(ev)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Skipping served state DAG event without a reference hash")
			return true
		}
		ev.ID = eventID
		return false
	})
}

// stageUnstored verifies the served state DAG's state events not stored here, skipping before their
// signatures are checked those that are, as verifying is the expensive part, and stages them, the
// first maxStaged of them unless it is zero. Returns those staged and which of the events were stored
// already.
func (run *jobRun) stageUnstored(
	ctx context.Context,
	evs []*types.Event,
	seen map[id.EventID]struct{},
	maxStaged int,
) ([]*types.Event, map[id.EventID]struct{}, error) {
	stored, err := run.jobs.db.Rooms.GetStoredEventIDs(ctx, util.EventsToIDs(evs))
	if err != nil {
		return nil, nil, err
	}
	unstored := slices.DeleteFunc(slices.Clone(evs), func(ev *types.Event) bool {
		_, found := stored[ev.ID]
		return found || ev.StateKey == nil
	})
	if maxStaged > 0 {
		unstored = unstored[:min(len(unstored), maxStaged)]
	}
	staged, err := util.VerifyParsedRemoteEvents(ctx, unstored, run.jobs.keyStore, seen)
	if err != nil {
		return nil, nil, err
	} else if err := run.jobs.db.Rooms.StageStateDAGEvents(ctx, run.roomID, staged); err != nil {
		return nil, nil, err
	}
	return staged, stored, nil
}

// stateDAGPDUs are PDUs of a state DAG room sent here while it had a job, or citing events it does
// not have. In a room this server is in, phase A pulls for those with a gap the prev events missing
// here, whose PDUs it makes targets too, then the state DAG events missing, which it stages, phase B
// replays these, and phase C publishes them ahead of the PDUs. A PDU no server fills the state DAG
// gap of fails; one whose gap a server failed to answer for, or that reaches another target of the
// job, and the PDUs citing it, are held back for a later round while the rest publish. In a room
// this server is not in, every PDU is sent, to be kept as an outlier membership or dropped.
type stateDAGPDUs struct {
	run  *jobRun
	pdus []*types.IngestTarget
	gap  bool
	// Whether this server is in the room, which a gap fill needs
	joined bool
	held   map[id.EventID]struct{}
	errs   []error
}

func (w *stateDAGPDUs) targets() []*types.IngestTarget {
	return w.pdus
}

func (w *stateDAGPDUs) kind() workKind {
	return pduWorkKind(w.gap)
}

func (w *stateDAGPDUs) fetch(ctx context.Context) error {
	if !w.gap {
		return nil
	}
	db := w.run.jobs.db
	var err error
	if w.joined, err = db.Rooms.IsServerJoined(ctx, w.run.roomID, w.run.jobs.config.ServerName); err != nil || !w.joined {
		return err
	}
	roomServers, err := db.Rooms.RoomServers(ctx, w.run.roomID)
	if err != nil {
		return err
	} else if err := w.pullPrevEvents(ctx); err != nil {
		return err
	}
	return w.fillStateDAG(ctx, roomServers)
}

// pullPrevEvents pulls the prev events missing here of the PDUs sent here, from their origins, and
// makes them PDU targets too, marked pulled: their own prev events missing are not pulled, so that
// the pulls stay bounded. Prev events still missing leave the PDU citing them a room extremity.
func (w *stateDAGPDUs) pullPrevEvents(ctx context.Context) error {
	pulled, err := w.run.jobs.fetcher.PrevEvents(ctx, w.run.roomID, w.run.roomVersion, w.pdus)
	if err != nil || len(pulled) == 0 {
		return err
	} else if err := w.run.jobs.db.Rooms.EnqueuePulledIngestPDUs(ctx, w.run.roomID, pulled); err != nil {
		return err
	}
	zerolog.Ctx(ctx).Info().Int("pulled", len(pulled)).Msg("Pulled prev events missing here")
	for _, target := range pulled {
		w.run.targets[target.Event.ID] = struct{}{}
	}
	w.pdus = sortTargetsTopologically(append(w.pdus, pulled...))
	return nil
}

func (w *stateDAGPDUs) build(ctx context.Context) error {
	if !w.gap || !w.joined {
		return nil
	}
	sent, _ := holdUnanswered(w.pdus, w.held)
	var from []id.EventID
	for _, target := range sent {
		from = append(from, target.Event.AuthDependencyIDs()...)
	}
	return w.run.jobs.db.Rooms.ReplayStateDAG(ctx, w.run.roomID, rooms.StateDAGReplay{
		RoomVersion: w.run.roomVersion,
		From:        from,
		Pending:     targetIDs(sent),
		Publish:     true,
	})
}

// publish sends the PDUs, with the events their gap fill replayed, but for those held back and those
// citing them, which stay targets, failing the work for a later round to ask again
func (w *stateDAGPDUs) publish(ctx context.Context) error {
	sent, held := holdUnanswered(w.pdus, w.held)
	if !w.gap || !w.joined {
		return w.run.sendPDUs(ctx, nil, nil, sent)
	} else if err := w.run.publishStateDAG(ctx, sent); err != nil {
		return err
	} else if len(held) == 0 {
		return nil
	}
	w.pdus = held
	err := fmt.Errorf("the state DAG gaps of %d PDUs are not filled yet", len(held))
	if len(w.errs) > 0 {
		err = fmt.Errorf("%w: %w", err, errors.Join(w.errs...))
	}
	return err
}
