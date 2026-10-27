package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	// The limit of a state DAG get_missing_events request starts at this and doubles each request,
	// as the MSC recommends, up to the most a babbleserv answers with
	firstStateDAGLimit       = 10
	MaxStateDAGMissingEvents = 1000
	// MSC4242's bound on either list of a state DAG get_missing_events request
	MaxStateDAGRequestIDs = 100
	maxGapPublishChunk    = 500
	maxStateDAGServerTime = 2 * time.Minute
)

// ReqGetMissingEvents is federation.ReqGetMissingEvents with MSC4242's flag walking the state DAG,
// which mautrix lacks
type ReqGetMissingEvents struct {
	federation.ReqGetMissingEvents
	StateDAG bool `json:"org.matrix.msc4242.state_dag"`
}

var errGapBudgetSpent = errors.New("the job's run fetched as many state DAG events as its gap fills may")

// fillStateDAG pulls the state DAG events missing here that the PDUs reach along prev_state_events,
// see stateDAGFill, asking the PDUs' origins, then the room's other servers, see gapFillServers, and
// stages them. A PDU whose gap stays open fails, unless a server failed to answer for it other than
// by refusing, or the fill's bounds stopped it while servers were still giving the gap, which holds
// it back for a later round, as does reaching another of the job's targets, which publishes first.
func (w *stateDAGPDUs) fillStateDAG(ctx context.Context, roomServers []string) error {
	db, roomID := w.run.jobs.db, w.run.roomID
	evs := make([]*types.Event, len(w.pdus))
	for i, target := range w.pdus {
		evs[i] = target.Event
	}
	pendingIDs := slices.Collect(maps.Keys(w.run.targets))
	walk := func(ctx context.Context) (*rooms.StateDAGGap, error) {
		return db.Rooms.WalkStateDAGGap(ctx, roomID, evs, pendingIDs, false)
	}
	gap, err := walk(ctx)
	if err != nil {
		return err
	}
	cfg := w.run.jobs.config.Rooms.Ingest
	fill := newStateDAGFill(evs, gap, cfg.MaxGapEvents, &w.run.gapBudget, time.Now().Add(cfg.GapFillTimeout))
	if open := fill.open(); len(open) > 0 {
		if err := w.askStateDAG(ctx, fill, open, roomServers, walk); err != nil {
			return err
		}
	}

	failures, held := fill.outcome()
	w.held, w.errs = held, fill.errs
	kept := w.pdus[:0]
	for _, target := range w.pdus {
		if failure := failures[target.Event.ID]; failure == nil {
			kept = append(kept, target)
		} else if err := w.run.fail(ctx, target, failure); !errors.Is(err, errWorkDone) {
			return err
		}
	}
	w.pdus = kept
	return nil
}

// askStateDAG runs the fill's requests for the open PDUs, staging the events served
func (w *stateDAGPDUs) askStateDAG(ctx context.Context, fill *stateDAGFill, open []id.EventID, roomServers []string, walk stateDAGWalker) error {
	db, roomID := w.run.jobs.db, w.run.roomID
	extremities, err := db.Rooms.GetRoomStateExtremEventIDs(ctx, roomID)
	if err != nil {
		return err
	}
	// The extremities published last are nearest the PDUs, and the server walks past the others
	// only to give events stored here, which are skipped
	earliestIDs := extremities[max(len(extremities)-MaxStateDAGRequestIDs, 0):]
	var origins []string
	for _, target := range w.pdus {
		if slices.Contains(open, target.Event.ID) {
			origins = append(origins, target.Servers...)
		}
	}
	seen := make(map[id.EventID]struct{}, len(fill.pdus))
	for _, ev := range fill.pdus {
		seen[ev.ID] = struct{}{}
	}
	request := func(ctx context.Context, server string, latestIDs []id.EventID, limit, maxStaged int) (int, error) {
		raws, err := w.run.jobs.fetcher.missingStateDAGEvents(ctx, server, roomID, latestIDs, earliestIDs, limit)
		if err != nil {
			return 0, err
		}
		// The room's targets publish themselves, after the events they cite
		served := slices.DeleteFunc(parseStateDAGEvents(ctx, raws[:min(len(raws), limit)], w.run.roomVersion, roomID), func(ev *types.Event) bool {
			_, target := w.run.targets[ev.ID]
			return target
		})
		staged, _, err := w.run.stageUnstored(ctx, served, seen, maxStaged)
		return len(staged), err
	}
	return fill.run(ctx, gapFillServers(origins, roomServers, w.run.jobs.config.ServerName), request, walk)
}

// stateDAGFill is the state DAG gap fill of PDUs of a state DAG room (MSC4242): get_missing_events
// requests walking the state DAG back from the PDUs to this server's state extremities, until no PDU
// reaches a state DAG event missing here, as the MSC has servers fill their state DAG
type stateDAGFill struct {
	pdus []*types.Event
	gap  *rooms.StateDAGGap
	// The events fetched by requests naming each PDU, or walking from events it reaches
	fetched   map[id.EventID]int
	maxPerPDU int
	// What the job's run may fetch still
	budget *int
	// The most rounds of requests a server is asked, see stateDAGRounds, and when the fill stops
	// asking servers
	maxRounds int
	deadline  time.Time
	// PDUs a server failed to answer for, other than by refusing, or left open at the fill's bounds
	// while giving their gap
	unanswered map[id.EventID]struct{}
	errs       []error
}

// stateDAGRequest asks a server for the state DAG events missing here before the latest events, at
// most limit of them, stages at most maxStaged of those not stored, and returns how many it staged
type stateDAGRequest func(ctx context.Context, server string, latestIDs []id.EventID, limit, maxStaged int) (int, error)

// stateDAGWalker walks the PDUs' gap again once requests staged more of it
type stateDAGWalker func(ctx context.Context) (*rooms.StateDAGGap, error)

func newStateDAGFill(pdus []*types.Event, gap *rooms.StateDAGGap, maxPerPDU int, budget *int, deadline time.Time) *stateDAGFill {
	return &stateDAGFill{
		pdus:       pdus,
		gap:        gap,
		fetched:    make(map[id.EventID]int),
		maxPerPDU:  maxPerPDU,
		budget:     budget,
		maxRounds:  stateDAGRounds(maxPerPDU),
		deadline:   deadline,
		unanswered: make(map[id.EventID]struct{}),
	}
}

// stateDAGRounds is how many rounds of requests a server is asked at most: those growing the limit to
// MaxStateDAGMissingEvents, enough more at it to give a PDU its whole gap, and one to walk from the
// frontier
func stateDAGRounds(maxPerPDU int) int {
	rounds := 1
	for limit := firstStateDAGLimit; limit < MaxStateDAGMissingEvents; limit *= 2 {
		rounds++
	}
	return rounds + maxPerPDU/MaxStateDAGMissingEvents + 1
}

// run asks each server in turn for what the PDUs still miss, until the fill's deadline, which holds
// back the PDUs still open. With the rounds and time each server is asked bounded too, a server
// giving a little of the gap each time cannot keep the job asking for hours.
func (f *stateDAGFill) run(ctx context.Context, servers []string, request stateDAGRequest, walk stateDAGWalker) error {
	for _, server := range servers {
		if len(f.open()) == 0 {
			return nil
		} else if !time.Now().Before(f.deadline) {
			zerolog.Ctx(ctx).Warn().Msg("State DAG gap fill took as long as it may, holding back the PDUs whose gap is open")
			f.hold(f.open())
			return nil
		} else if err := f.ask(ctx, server, request, walk); err != nil {
			return err
		}
	}
	return nil
}

func (f *stateDAGFill) hold(eventIDs []id.EventID) {
	for _, eventID := range eventIDs {
		f.unanswered[eventID] = struct{}{}
	}
}

// ask asks a server for the state DAG events the open PDUs miss, the limit doubling each request,
// until none is missing. Once a request fills none of the gap, as a server capping its responses
// gives again the events nearest the PDUs, the requests walk from the frontier instead, the first
// MaxStateDAGRequestIDs of the events reached that cite missing ones, and once that fills none
// either, the server has nothing more to give. Only events missing before that the walk then reaches
// fill the gap: others a server sends are staged, and counted against the bounds, but give it no
// more requests. A server failing is not asked again; unless it refused, the PDUs open then were
// unanswered, as are those open once the server, still giving their gap, was asked its most rounds
// or for maxStateDAGServerTime: Synapse caps its answers at 20 events. Each request stages no more
// than the bounds leave, and one past them.
func (f *stateDAGFill) ask(ctx context.Context, server string, request stateDAGRequest, walk stateDAGWalker) error {
	deadline := f.deadline
	if serverDeadline := time.Now().Add(maxStateDAGServerTime); serverDeadline.Before(deadline) {
		deadline = serverDeadline
	}
	latestIDs, fromFrontier := f.open(), false
	limit := firstStateDAGLimit
	for round := 0; len(latestIDs) > 0; round, limit = round+1, min(2*limit, MaxStateDAGMissingEvents) {
		if round == f.maxRounds || !time.Now().Before(deadline) {
			zerolog.Ctx(ctx).Warn().Str("server", server).Int("rounds", round).Msg("Server is still giving the state DAG gap at the bounds it is asked within, asking the next")
			f.hold(f.open())
			return nil
		}
		open := f.open()
		missing := maps.Clone(f.gap.Missing)
		for chunk := range slices.Chunk(latestIDs, MaxStateDAGRequestIDs) {
			named := chunk
			if fromFrontier {
				named = open
			}
			staged, err := request(ctx, server, chunk, limit, f.stageable(named))
			util.ReportProgress(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			} else if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Str("server", server).Msg("Failed to get missing state DAG events from server")
				f.errs = append(f.errs, fmt.Errorf("%s: %w", server, err))
				if !refusedRequest(err) {
					f.hold(open)
				}
				return nil
			} else if err := f.count(named, staged); err != nil {
				return err
			}
		}
		var err error
		if f.gap, err = walk(ctx); err != nil {
			return err
		}
		filled := filledAny(missing, f.gap.Missing)
		if !filled && fromFrontier {
			return nil
		}
		fromFrontier = fromFrontier || !filled
		if latestIDs = f.open(); fromFrontier && len(latestIDs) > 0 {
			frontier := f.frontier()
			latestIDs = frontier[:min(len(frontier), MaxStateDAGRequestIDs)]
		}
	}
	return nil
}

func filledAny(before, after map[id.EventID]struct{}) bool {
	for eventID := range before {
		if _, found := after[eventID]; !found {
			return true
		}
	}
	return false
}

// stageable is how many events a request naming the PDUs may stage: what the job's run and the PDU
// with the least left may fetch still, and one more, so that a gap past them shows
func (f *stateDAGFill) stageable(pdus []id.EventID) int {
	left := *f.budget
	for _, eventID := range pdus {
		left = min(left, f.maxPerPDU-f.fetched[eventID])
	}
	return max(left, 0) + 1
}

func (f *stateDAGFill) count(pdus []id.EventID, staged int) error {
	for _, eventID := range pdus {
		f.fetched[eventID] += staged
	}
	if *f.budget -= staged; *f.budget < 0 {
		return errGapBudgetSpent
	}
	return nil
}

// reaching returns the events, the PDUs among them, that reach a state DAG event missing here along
// prev_state_events, through the events stored without state or unpublished and the other PDUs
func (f *stateDAGFill) reaching() map[id.EventID]struct{} {
	return f.reachingFrom(slices.Collect(maps.Keys(f.gap.Missing)))
}

// reachingPending returns the events, the PDUs among them, that reach another of the job's targets
// pending, which publishes before them, as reaching reaches missing events
func (f *stateDAGFill) reachingPending() map[id.EventID]struct{} {
	pending := maps.Clone(f.gap.Pending)
	for _, ev := range f.pdus {
		delete(pending, ev.ID)
	}
	return f.reachingFrom(slices.Collect(maps.Keys(pending)))
}

func (f *stateDAGFill) reachingFrom(sources []id.EventID) map[id.EventID]struct{} {
	citedBy := make(map[id.EventID][]id.EventID)
	cite := func(eventID id.EventID, cited []id.EventID) {
		for _, citedID := range cited {
			citedBy[citedID] = append(citedBy[citedID], eventID)
		}
	}
	for eventID, cited := range f.gap.Cites {
		cite(eventID, cited)
	}
	for _, ev := range f.pdus {
		cite(ev.ID, ev.AuthDependencyIDs())
	}
	reaching := make(map[id.EventID]struct{})
	queue := sources
	for len(queue) > 0 {
		eventID := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, citer := range citedBy[eventID] {
			if _, found := reaching[citer]; !found {
				reaching[citer] = struct{}{}
				queue = append(queue, citer)
			}
		}
	}
	return reaching
}

// open returns the PDUs, in order, whose gap is open and that may fetch more
func (f *stateDAGFill) open() []id.EventID {
	reaching := f.reaching()
	var open []id.EventID
	for _, ev := range f.pdus {
		if _, found := reaching[ev.ID]; found && f.fetched[ev.ID] <= f.maxPerPDU {
			open = append(open, ev.ID)
		}
	}
	return open
}

// frontier returns the events reached, the PDUs among them, that cite a missing event, sorted
func (f *stateDAGFill) frontier() []id.EventID {
	citesMissing := func(cited []id.EventID) bool {
		return slices.ContainsFunc(cited, func(eventID id.EventID) bool {
			_, missing := f.gap.Missing[eventID]
			return missing
		})
	}
	var frontier []id.EventID
	for eventID, cited := range f.gap.Cites {
		if citesMissing(cited) {
			frontier = append(frontier, eventID)
		}
	}
	for _, ev := range f.pdus {
		if citesMissing(ev.AuthDependencyIDs()) {
			frontier = append(frontier, ev.ID)
		}
	}
	slices.Sort(frontier)
	return frontier
}

// outcome returns, of the PDUs whose gap is still open, why each fails, and those unanswered, which
// are held back for a later round to ask again, as are those reaching another of the job's targets,
// so that it publishes first
func (f *stateDAGFill) outcome() (map[id.EventID]*types.IngestFailure, map[id.EventID]struct{}) {
	reaching, reachingPending := f.reaching(), f.reachingPending()
	failures := make(map[id.EventID]*types.IngestFailure)
	held := make(map[id.EventID]struct{})
	for _, ev := range f.pdus {
		if _, found := reaching[ev.ID]; !found {
			if _, found := reachingPending[ev.ID]; found {
				held[ev.ID] = struct{}{}
			}
			continue
		} else if f.fetched[ev.ID] > f.maxPerPDU {
			failures[ev.ID] = tooLargeGapFailure(f.maxPerPDU)
		} else if _, found := f.unanswered[ev.ID]; found {
			held[ev.ID] = struct{}{}
		} else {
			failures[ev.ID] = unfilledGapFailure()
		}
	}
	return failures, held
}

// publishStateDAG publishes PDU targets of a state DAG room this server is in, with the events they
// and the pending events reach along prev_state_events that the job replayed but did not publish yet,
// as federated events in topological chunks, see publishChunks. The replayed events take the state
// their replay gave them and stay out of the timeline. An event without state here is replayed as its
// chunk comes, once the targets it may cite published. The pending events, a join publishing next,
// are not published.
func (run *jobRun) publishStateDAG(ctx context.Context, targets []*types.IngestTarget, pending ...*types.Event) error {
	byID := make(map[id.EventID]*types.IngestTarget, len(targets))
	unpublished := make([]*types.Event, 0, len(targets))
	for _, target := range targets {
		byID[target.Event.ID] = target
		unpublished = append(unpublished, target.Event)
	}
	gap, err := run.jobs.db.Rooms.WalkStateDAGGap(ctx, run.roomID, slices.Concat(unpublished, pending), nil, true)
	if err != nil {
		return err
	}
	stateless := make(map[id.EventID]struct{})
	for eventID, ev := range gap.Events {
		unpublished = append(unpublished, ev)
		if _, found := gap.Unpublished[eventID]; !found {
			stateless[eventID] = struct{}{}
		}
	}
	waiting := make(map[id.EventID]struct{}, len(targets)+len(pending))
	for _, eventID := range slices.Concat(util.EventsToIDs(pending), targetIDs(targets)) {
		waiting[eventID] = struct{}{}
	}
	order := rooms.SortEventsTopologically(unpublished)
	for _, chunk := range publishChunks(order, byID, stateless, maxGapPublishChunk, maxPDUChunkBytes) {
		var replayed []*types.Event
		var chunkTargets []*types.IngestTarget
		var replay []id.EventID
		for _, ev := range chunk {
			if target := byID[ev.ID]; target != nil {
				chunkTargets = append(chunkTargets, target)
				continue
			}
			replayed = append(replayed, ev)
			if _, found := stateless[ev.ID]; found {
				replay = append(replay, ev.ID)
			}
		}
		if len(replay) > 0 {
			if err := run.jobs.db.Rooms.ReplayStateDAG(ctx, run.roomID, rooms.StateDAGReplay{
				RoomVersion: run.roomVersion,
				From:        replay,
				Pending:     slices.Collect(maps.Keys(waiting)),
				Publish:     true,
			}); err != nil {
				return err
			}
		}
		if err := run.sendPDUs(ctx, replayed, nil, chunkTargets); err != nil {
			return err
		}
		util.ReportProgress(ctx)
		for _, target := range chunkTargets {
			delete(waiting, target.Event.ID)
		}
	}
	return nil
}

// publishChunks splits events in topological order into the chunks a state DAG gap fill publishes,
// of at most maxEvents and, past their first, maxBytes. A chunk holding a target ends ahead of an
// event without state here: it may cite the target, so is replayed once the target published.
func publishChunks(
	evs []*types.Event,
	targets map[id.EventID]*types.IngestTarget,
	stateless map[id.EventID]struct{},
	maxEvents, maxBytes int,
) [][]*types.Event {
	var chunks [][]*types.Event
	size, holdsTarget := 0, false
	for _, ev := range evs {
		target := targets[ev.ID]
		evSize := 0
		if target != nil {
			evSize = target.Size
		} else {
			evSize = len(ev.ToMsgpack())
		}
		_, withoutState := stateless[ev.ID]
		if n := len(chunks); n == 0 || len(chunks[n-1]) == maxEvents || size+evSize > maxBytes || withoutState && holdsTarget {
			chunks = append(chunks, nil)
			size, holdsTarget = 0, false
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], ev)
		size += evSize
		holdsTarget = holdsTarget || target != nil
	}
	return chunks
}

// PrevEvents pulls the prev events missing here of the PDU targets of a state DAG room sent here,
// from the room's extremities, as PDU targets marked pulled, not written yet. Every origin is asked
// for the gaps of the PDUs it sent, two a request, and the others are not asked: a server failing
// leaves the prev events it did not give missing, which only leaves the PDUs citing them room
// extremities.
func (f *Fetcher) PrevEvents(ctx context.Context, roomID id.RoomID, roomVersion string, targets []*types.IngestTarget) ([]*types.IngestTarget, error) {
	evs := make([]*types.Event, len(targets))
	seenIDs := make(map[id.EventID]struct{}, len(targets))
	for i, target := range targets {
		evs[i] = target.Event
		seenIDs[target.Event.ID] = struct{}{}
	}
	missing, err := f.db.Rooms.GetMissingEventsRequest(ctx, roomID, evs)
	if err != nil {
		return nil, err
	} else if len(missing.Missing) == 0 {
		return nil, nil
	}
	sent := slices.DeleteFunc(slices.Clone(targets), func(target *types.IngestTarget) bool { return target.Pulled })
	var origins []string
	for _, target := range sent {
		origins = append(origins, target.Servers...)
	}
	plan := newGapPlan(sent, missing.Missing)
	servers := gapFillServers(origins, nil, f.config.ServerName)
	for _, server := range servers {
		if err := plan.ask(ctx, server, true, func(ctx context.Context, server string, latestIDs, pulledIDs []id.EventID) ([]*types.Event, error) {
			earliestIDs := slices.Concat(missing.Extremities, pulledIDs)
			return f.missingEvents(ctx, server, roomID, roomVersion, latestIDs, earliestIDs, missing.MinDepth, seenIDs)
		}); err != nil {
			return nil, err
		}
	}
	now := time.Now().UnixMilli()
	var pulled []*types.IngestTarget
	for _, server := range servers {
		for _, ev := range plan.pulledFrom[server] {
			pulled = append(pulled, &types.IngestTarget{
				Kind:     types.IngestTargetPDU,
				Event:    ev,
				Servers:  []string{server},
				Enqueued: now,
				Size:     len(ev.ToMsgpack()),
				Pulled:   true,
			})
		}
	}
	return pulled, nil
}

// missingStateDAGEvents asks a server for the events of a state DAG room's state DAG that the latest
// events reach along prev_state_events, a message's included, before the earliest ones: at most
// limit of them, those fewest hops away first (MSC4242)
func (f *Fetcher) missingStateDAGEvents(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	latestIDs, earliestIDs []id.EventID,
	limit int,
) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, f.requestTimeout())
	defer cancel()
	var resp federation.RespGetMissingEvents
	err := f.gapClient.MakeRequest(ctx, server, true, http.MethodPost, federation.URLPath{"v1", "get_missing_events", roomID}, &ReqGetMissingEvents{
		ReqGetMissingEvents: federation.ReqGetMissingEvents{
			EarliestEvents: slices.Concat([]id.EventID{}, earliestIDs),
			LatestEvents:   latestIDs,
			Limit:          limit,
		},
		StateDAG: true,
	}, &resp)
	return resp.Events, err
}
