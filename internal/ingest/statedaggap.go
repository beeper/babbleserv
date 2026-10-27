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
	firstStateDAGLimit = 10
	maxStateDAGLimit   = 1000
	// MSC4242's bound on either list of a state DAG get_missing_events request
	maxStateDAGRequestIDs = 100
	// Events a publish of a state DAG gap fill takes, at most
	maxGapPublishChunk = 500
)

var errGapBudgetSpent = errors.New("the job's run fetched as many state DAG events as its gap fills may")

// fillStateDAG pulls the state DAG events missing here that the PDUs reach along prev_state_events,
// see stateDAGFill, asking the PDUs' origins, then the room's other servers, at most
// maxGapFillServers, and stages them. A PDU whose gap stays open fails, unless a server failed to
// answer for it other than by refusing, which holds it back for a later round.
func (w *stateDAGPDUs) fillStateDAG(ctx context.Context, roomServers []string) error {
	db, roomID := w.run.jobs.db, w.run.roomID
	evs := make([]*types.Event, len(w.pdus))
	for i, target := range w.pdus {
		evs[i] = target.Event
	}
	walk := func(ctx context.Context) (*rooms.StateDAGGap, error) {
		return db.Rooms.WalkStateDAGGap(ctx, roomID, evs)
	}
	gap, err := walk(ctx)
	if err != nil {
		return err
	}
	fill := newStateDAGFill(evs, gap, w.run.jobs.config.Rooms.Ingest.MaxGapEvents, &w.run.gapBudget)
	open := fill.open()
	if len(open) == 0 {
		return nil
	}
	extremities, err := db.Rooms.GetRoomStateExtremEventIDs(ctx, roomID)
	if err != nil {
		return err
	}
	// The extremities published last are nearest the PDUs, and the server walks past the others
	// only to give events stored here, which are skipped
	earliestIDs := extremities[max(len(extremities)-maxStateDAGRequestIDs, 0):]
	var origins []string
	for _, target := range w.pdus {
		if slices.Contains(open, target.Event.ID) {
			origins = append(origins, target.Servers...)
		}
	}
	seen := make(map[id.EventID]struct{}, len(evs))
	for _, ev := range evs {
		seen[ev.ID] = struct{}{}
	}
	request := func(ctx context.Context, server string, latestIDs []id.EventID, limit int) (int, error) {
		raws, err := w.run.jobs.fetcher.missingStateDAGEvents(ctx, server, roomID, latestIDs, earliestIDs, limit)
		if err != nil {
			return 0, err
		}
		// The room's targets publish themselves, after the events they cite
		served := slices.DeleteFunc(parseStateDAGEvents(ctx, raws, w.run.roomVersion, roomID), func(ev *types.Event) bool {
			_, target := w.run.targets[ev.ID]
			return target
		})
		staged, _, err := w.run.stageUnstored(ctx, served, seen)
		return len(staged), err
	}
	if err := fill.run(ctx, gapFillServers(origins, roomServers, w.run.jobs.config.ServerName), request, walk); err != nil {
		return err
	}

	failures, held := fill.outcome()
	w.unanswered, w.errs = held, fill.errs
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
	// PDUs a server failed to answer for, other than by refusing
	unanswered map[id.EventID]struct{}
	errs       []error
}

// stateDAGRequest asks a server for the state DAG events missing here before the latest events, at
// most limit of them, stages them, and returns how many it staged
type stateDAGRequest func(ctx context.Context, server string, latestIDs []id.EventID, limit int) (int, error)

// stateDAGWalker walks the PDUs' gap again once requests staged more of it
type stateDAGWalker func(ctx context.Context) (*rooms.StateDAGGap, error)

func newStateDAGFill(pdus []*types.Event, gap *rooms.StateDAGGap, maxPerPDU int, budget *int) *stateDAGFill {
	return &stateDAGFill{
		pdus:       pdus,
		gap:        gap,
		fetched:    make(map[id.EventID]int),
		maxPerPDU:  maxPerPDU,
		budget:     budget,
		unanswered: make(map[id.EventID]struct{}),
	}
}

// run asks each server in turn for what the PDUs still miss
func (f *stateDAGFill) run(ctx context.Context, servers []string, request stateDAGRequest, walk stateDAGWalker) error {
	for _, server := range servers {
		if len(f.open()) == 0 {
			return nil
		} else if err := f.ask(ctx, server, request, walk); err != nil {
			return err
		}
	}
	return nil
}

// ask asks a server for the state DAG events the open PDUs miss, the limit doubling each request,
// until none is missing. Once a request gives nothing new, as a server capping its responses gives
// again the events nearest the PDUs, the requests walk from the frontier instead, the events reached
// that cite missing ones, and once that gives nothing new either, the server has nothing more to
// give. A server failing is not asked again; unless it refused, the PDUs open then were unanswered.
func (f *stateDAGFill) ask(ctx context.Context, server string, request stateDAGRequest, walk stateDAGWalker) error {
	latestIDs, fromFrontier := f.open(), false
	for limit := firstStateDAGLimit; len(latestIDs) > 0; limit = min(2*limit, maxStateDAGLimit) {
		open := f.open()
		fetched := 0
		for chunk := range slices.Chunk(latestIDs, maxStateDAGRequestIDs) {
			staged, err := request(ctx, server, chunk, limit)
			util.ReportProgress(ctx)
			if ctx.Err() != nil {
				return ctx.Err()
			} else if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Str("server", server).Msg("Failed to get missing state DAG events from server")
				f.errs = append(f.errs, fmt.Errorf("%s: %w", server, err))
				if !refusedRequest(err) {
					for _, eventID := range open {
						f.unanswered[eventID] = struct{}{}
					}
				}
				return nil
			}
			fetched += staged
			named := chunk
			if fromFrontier {
				named = open
			}
			if err := f.count(named, staged); err != nil {
				return err
			}
		}
		var err error
		if f.gap, err = walk(ctx); err != nil {
			return err
		} else if fetched == 0 && fromFrontier {
			return nil
		}
		fromFrontier = fromFrontier || fetched == 0
		if latestIDs = f.open(); fromFrontier && len(latestIDs) > 0 {
			latestIDs = f.frontier()
		}
	}
	return nil
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
	queue := slices.Collect(maps.Keys(f.gap.Missing))
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

// outcome returns, of the PDUs whose gap is still open, why each fails, and those a server failed to
// answer for, which are held back for a later round to ask again
func (f *stateDAGFill) outcome() (map[id.EventID]*types.IngestFailure, map[id.EventID]struct{}) {
	reaching := f.reaching()
	failures := make(map[id.EventID]*types.IngestFailure)
	held := make(map[id.EventID]struct{})
	for _, ev := range f.pdus {
		if _, found := reaching[ev.ID]; !found {
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
	gap, err := run.jobs.db.Rooms.WalkStateDAGGap(ctx, run.roomID, slices.Concat(unpublished, pending))
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
// from the room's extremities, as PDU targets marked pulled, not written yet. Each origin is asked for
// the gaps of the PDUs it sent, two a request, and the others are not asked: a server failing leaves
// the prev events it did not give missing, which only leaves the PDUs citing them room extremities.
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

// federation.ReqGetMissingEvents with MSC4242's flag walking the state DAG, which mautrix lacks
type reqGetMissingStateDAG struct {
	federation.ReqGetMissingEvents
	StateDAG bool `json:"org.matrix.msc4242.state_dag"`
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
	ctx, cancel := context.WithTimeout(ctx, getMissingEventsTimeout)
	defer cancel()
	var resp federation.RespGetMissingEvents
	err := f.fedclient.MakeRequest(ctx, server, true, http.MethodPost, federation.URLPath{"v1", "get_missing_events", roomID}, &reqGetMissingStateDAG{
		ReqGetMissingEvents: federation.ReqGetMissingEvents{
			EarliestEvents: slices.Concat([]id.EventID{}, earliestIDs),
			LatestEvents:   latestIDs,
			Limit:          limit,
		},
		StateDAG: true,
	}, &resp)
	return resp.Events, err
}
