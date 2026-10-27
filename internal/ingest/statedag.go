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

// stateDAGStrategy runs jobs of state DAG rooms (MSC4242): joins replay the state DAG the resident
// serves, oldest first, and PDUs publish once their gaps are filled
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

// Each tip is a state extremity row the join's publish writes, so past this many the join fails
const maxStateDAGJoinTips = 1000

// stateDAGJoin stages the state DAG served with the join (phase A), then replays it and builds the
// state before the join (B). If this server got into the room meanwhile, the events the join reaches
// that lack state here publish ahead of it, and the tips arrive as PDUs.
type stateDAGJoin struct {
	joinWork
	// Of the last response: served maps each event to whether another cites it, skipped holds the
	// join and the events citing a skipped one
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

// A member's join publishes after the events its replay gave state, as none may be cited unpublished
func (w *stateDAGJoin) publish(ctx context.Context) error {
	if w.member {
		if err := w.run.publishStateDAG(ctx, nil, w.target.Event); err != nil {
			return err
		}
	}
	return w.joinWork.publish(ctx)
}

// sendJoin tries the target's servers until one serves a state DAG connecting the join to the create
// event, as the MSC requires, never asking again one whose did not. A send that may have landed is
// retried until timeout, and a refusal of it replaces the join unless a server already answered it.
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

// stage stages a chunk of the served state DAG. Events citing the join cannot have state before it
// publishes, so arrive as PDUs instead. Only a chunk holding events of the room reports progress, so
// a server serving junk cannot keep the job going.
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

// skipNewerThanJoin relies on the state DAG being served in topological order
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

func (w *stateDAGJoin) tips() []id.EventID {
	var tips []id.EventID
	for _, eventID := range slices.Sorted(maps.Keys(w.served)) {
		if !w.served[eventID] {
			tips = append(tips, eventID)
		}
	}
	return tips
}

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

// stateDAGPDUs are PDUs of a state DAG room sent while it had a job, or citing events missing here.
// In a joined room phase A pulls their missing prev events and state DAG, phase B replays it, and
// phase C publishes it ahead of the PDUs. A PDU whose gap no server fills fails; one left unanswered,
// or reaching another target, is held back for a later round with the PDUs citing it.
type stateDAGPDUs struct {
	run    *jobRun
	pdus   []*types.IngestTarget
	gap    bool
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

// pullPrevEvents makes missing prev events targets, marked pulled so theirs are not pulled in turn
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

// Held back PDUs stay targets, and failing the work leaves them for a later round
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
