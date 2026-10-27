package ingest

import (
	"context"
	"encoding/json"
	"errors"
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
// citing events without state here dropped until state DAG gap fills, so that no target the job
// cannot fill holds the room's job
type stateDAGStrategy struct{}

func (stateDAGStrategy) collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error) {
	joins, pdus := splitTargets(targets)
	joined := false
	if len(joins) > 0 && len(pdus) > 0 {
		var err error
		if joined, err = run.jobs.db.Rooms.IsServerJoined(ctx, run.roomID, run.jobs.config.ServerName); err != nil {
			return nil, err
		}
	}
	return stateDAGWorks(run, joins, sortTargetsTopologically(pdus), joined), nil
}

// stateDAGWorks are the units of work publishing a round's targets, the PDUs in topological order,
// ordered as legacyWorks orders them: while this server is not in the room the joins alone, else
// the PDUs ahead of the joins but for those citing a pending join, or a PDU citing one, which go
// after them
func stateDAGWorks(run *jobRun, joins, pdus []*types.IngestTarget, joined bool) []ingestWork {
	works := make([]ingestWork, 0, len(joins)+2)
	joinWorks := make([]ingestWork, len(joins))
	for i, target := range joins {
		joinWorks[i] = &stateDAGJoin{joinWork: joinWork{run: run, target: target}}
	}
	if len(joins) > 0 && !joined {
		return joinWorks
	}
	beforeJoins, afterJoins := splitCitingJoins(pdus, joins)
	for _, chunk := range chunkPDUs(beforeJoins, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, &stateDAGPDUs{run: run, pdus: chunk})
	}
	works = append(works, joinWorks...)
	for _, chunk := range chunkPDUs(afterJoins, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, &stateDAGPDUs{run: run, pdus: chunk})
	}
	return works
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

// stateDAGJoin is a join to a state DAG room. Phase A sends the stored join event in a send_join and
// stages the state DAG its response streams, until a server's connects the join to the create event.
// Phase B replays it, giving its events state here, then builds the state before the join.
type stateDAGJoin struct {
	joinWork
	// Of the last response: its state DAG's events, and whether any of them cites each, which
	// leaves the tips uncited; the events it gave, and those verified; its name and alias events
	served   map[id.EventID]bool
	received int
	seen     map[id.EventID]struct{}
	naming   []*types.Event
}

func (w *stateDAGJoin) fetch(ctx context.Context) error {
	return w.sendJoin(ctx)
}

func (w *stateDAGJoin) build(ctx context.Context) error {
	tips := w.tips()
	if err := w.run.jobs.db.Rooms.ReplayStateDAG(ctx, w.target.Event, tips); err != nil {
		return err
	}
	var err error
	if w.join, err = w.run.jobs.db.NewStateDAGRemoteJoin(ctx, w.target.Event, tips); err != nil {
		return err
	}
	return w.joinWork.build(ctx)
}

// sendJoin sends the stored join event to the target's servers until one serves a state DAG that
// connects the join to the create event, staging what each serves, as the MSC requires. A send that
// may have landed is sent again, across the servers, until the join times out, and a server refusing
// one that may have landed has the target replaced with a fresh join. A server whose state DAG does
// not connect the join, or that serves none, is not asked again: once no server is left the join
// fails.
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
		w.served, w.received, w.seen, w.naming = make(map[id.EventID]bool), 0, make(map[id.EventID]struct{}), nil
		var stageErr error
		err = w.run.jobs.fetcher.SendJoinStateDAG(ctx, server, w.target.Event, func(raws []json.RawMessage) error {
			stageErr = w.stage(ctx, raws)
			return stageErr
		})
		var failure *types.IngestFailure
		switch {
		case errors.As(stageErr, &failure):
			return w.run.fail(ctx, w.target, failure)
		case stageErr != nil:
			return stageErr
		case errors.Is(err, errBadStateDAGResponse):
			log.Warn().Err(err).Str("server", server).Msg("Server answered the join with no state DAG")
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
// skipping, before their signatures are checked, those stored already. It records what the state DAG
// cites, for its tips, and names the room for BabbleBot's notices.
func (w *stateDAGJoin) stage(ctx context.Context, raws []json.RawMessage) error {
	w.received += len(raws)
	if limit := w.run.jobs.config.Rooms.Ingest.MaxJoinEvents; w.received > limit {
		return tooLargeFailure("state DAG has over", limit, limit)
	}
	roomID := w.run.roomID
	parsed := eventsInRoom(ctx, util.ParseRemoteEvents(ctx, raws, w.target.Event.RoomVersion), roomID)
	// The join is the job's to publish, so is never staged, though a resident may serve it
	parsed = slices.DeleteFunc(parsed, func(ev *types.Event) bool {
		eventID, err := util.GetEventReferenceHash(ev)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Skipping served state DAG event without a reference hash")
			return true
		}
		ev.ID = eventID
		return eventID == w.target.Event.ID
	})
	stored, err := w.run.jobs.db.Rooms.GetStoredEventIDs(ctx, util.EventsToIDs(parsed))
	if err != nil {
		return err
	}
	unstored := slices.DeleteFunc(slices.Clone(parsed), func(ev *types.Event) bool {
		_, found := stored[ev.ID]
		return found
	})
	verified, err := util.VerifyParsedRemoteEvents(ctx, unstored, w.run.jobs.keyStore, w.seen)
	if err != nil {
		return err
	}
	staged := slices.DeleteFunc(verified, func(ev *types.Event) bool { return ev.StateKey == nil })
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
	return w.run.jobs.db.Rooms.StageStateDAGEvents(ctx, roomID, staged)
}

// cite records a served state DAG event and the events it cites
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

// stateDAGPDUs are PDUs of a state DAG room sent here while it had a job. Phase C sends those whose
// prev state events are in place here or among them, see Jobs.SplitConnected, as /send does, and
// drops the rest until state DAG gap fills. In a room this server is not in, every PDU is sent, to be
// kept as an outlier membership or dropped.
type stateDAGPDUs struct {
	run  *jobRun
	pdus []*types.IngestTarget
}

func (w *stateDAGPDUs) targets() []*types.IngestTarget {
	return w.pdus
}

func (w *stateDAGPDUs) fetch(context.Context) error {
	return nil
}

func (w *stateDAGPDUs) build(context.Context) error {
	return nil
}

func (w *stateDAGPDUs) publish(ctx context.Context) error {
	sent, gaps := w.pdus, []*types.IngestTarget(nil)
	if joined, err := w.run.jobs.db.Rooms.IsServerJoined(ctx, w.run.roomID, w.run.jobs.config.ServerName); err != nil {
		return err
	} else if joined {
		if sent, gaps, err = w.run.jobs.splitConnectedTargets(ctx, w.pdus); err != nil {
			return err
		}
	}
	if len(sent) > 0 {
		if err := w.run.sendPDUs(ctx, nil, nil, sent); err != nil {
			return err
		}
	}
	if len(gaps) == 0 {
		return nil
	}
	for _, target := range gaps {
		zerolog.Ctx(ctx).Warn().
			Stringer("event_id", target.Event.ID).
			Strs("prev_state_event_ids", util.StringersToStrs(target.Event.PrevStateEventIDs)).
			Msg("Dropping state DAG PDU target citing events without state here")
	}
	return w.run.jobs.db.Rooms.ClearIngestTargets(ctx, w.run.roomID, targetIDs(gaps))
}
