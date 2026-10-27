package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	// Prev events of a gap fill's pulled events whose state is fetched from the server that pulled
	// them. Events citing the others are dropped, and the state fetched again with the next event
	// citing them.
	maxPrevEventStatesFetched = 10
	// Events of a prev event's state fetched concurrently when too few are missing to fetch it whole
	prevEventStatesFetchConcurrency = 5
	// PDUs a get_missing_events request names. Synapse answers at most 20 events a request and pulls
	// 10 for a PDU it fills the gap of, so each PDU named gets that many.
	gapPDUsPerRequest = 2
	// The limit of a get_missing_events request the spec has a server take when none is given
	defaultMissingEventsLimit = 10
	// The largest get_missing_events response read, room for MaxStateDAGMissingEvents events of the largest
	// size a PDU may have: a server answering with more is taken to refuse
	maxMissingEventsResponseBytes = MaxStateDAGMissingEvents * util.MaxPDUBytes
)

// Fetcher pulls from other servers the events and state that a room's events need and this server
// does not store, for the room's ingest job
type Fetcher struct {
	config    config.BabbleConfig
	db        *databases.Databases
	fedclient *federation.Client
	// fedclient without its timeout on whole requests, for responses read as they arrive
	streamClient *federation.Client
	// fedclient reading responses of at most maxMissingEventsResponseBytes
	gapClient *federation.Client
	keyStore  *util.KeyStore
}

func NewFetcher(cfg config.BabbleConfig, db *databases.Databases, fedclient *federation.Client, keyStore *util.KeyStore) *Fetcher {
	streamClient := *fedclient
	streamClient.HTTP = &http.Client{Transport: fedclient.HTTP.Transport, CheckRedirect: fedclient.HTTP.CheckRedirect}
	gapClient := *fedclient
	gapClient.ResponseSizeLimit = maxMissingEventsResponseBytes
	return &Fetcher{config: cfg, db: db, fedclient: fedclient, streamClient: &streamClient, gapClient: &gapClient, keyStore: keyStore}
}

// requestTimeout bounds each request of a gap fill: half a lease, so that the progress reported
// after it refreshes the job's lease before its watchdog stops it
func (f *Fetcher) requestTimeout() time.Duration {
	return f.config.Rooms.Ingest.LeaseTimeout / 2
}

// GapFill is what a gap fill pulled for PDUs citing events missing here
type GapFill struct {
	Pulled []*types.Event
	Given  *rooms.GivenStates
	// The PDUs whose gap is still open because a server failed to answer for them, other than by
	// refusing, so asking again may fill it, and why: a get_missing_events request, or the state at
	// a prev event the PDUs reach, which the fill's bound on those may also have left unfetched
	Unanswered map[id.EventID]struct{}
	Errs       []error
}

// GapFill pulls the events missing between a joined room's extremities and the PDU targets citing
// prev events missing here. Each PDU's origin is asked for its own PDUs' gaps, then the first
// maxGapFillServers of the servers gapFillServers gives for the gaps still open, and the state at
// the pulled events' prev events that have none here is fetched from the server that pulled them. As
// Synapse does, it never fetches the state at the PDUs' own prev events: their sender pushed them, so
// should have what they cite. Fails only on errors of this server's own.
func (f *Fetcher) GapFill(
	ctx context.Context,
	roomID id.RoomID,
	roomVersion string,
	targets []*types.IngestTarget,
	roomServers []string,
) (*GapFill, error) {
	pdus := make([]*types.Event, len(targets))
	var origins []string
	for i, target := range targets {
		pdus[i] = target.Event
		origins = append(origins, target.Servers...)
	}
	missing, err := f.db.Rooms.GetMissingEventsRequest(ctx, roomID, pdus)
	if err != nil {
		return nil, err
	} else if len(missing.Missing) == 0 {
		return &GapFill{}, nil
	}

	seenIDs := make(map[id.EventID]struct{}, len(pdus))
	for _, ev := range pdus {
		seenIDs[ev.ID] = struct{}{}
	}
	plan := newGapPlan(targets, missing.Missing)
	servers := gapFillServers(origins, roomServers, f.config.ServerName)
	err = plan.run(ctx, servers, func(ctx context.Context, server string, latestIDs, pulledIDs []id.EventID) ([]*types.Event, error) {
		// What was pulled already is known here, so the server stops at it as at the extremities
		earliestIDs := slices.Concat(missing.Extremities, pulledIDs)
		return f.missingEvents(ctx, server, roomID, roomVersion, latestIDs, earliestIDs, missing.MinDepth, seenIDs)
	})
	if err != nil {
		return nil, err
	}

	fill := &GapFill{Unanswered: plan.unanswered(), Errs: plan.errs}
	for _, server := range servers {
		fill.Pulled = append(fill.Pulled, plan.pulledFrom[server]...)
	}
	evs := slices.Concat(pdus, fill.Pulled)
	states := &prevEventStates{unfetched: make(map[id.EventID]struct{})}
	for _, server := range servers {
		if err := f.addPrevEventStates(ctx, server, roomID, roomVersion, evs, plan.pulledFrom[server], states); err != nil {
			return nil, err
		}
	}
	fill.Given, fill.Errs = states.given, append(fill.Errs, states.errs...)
	maps.Copy(fill.Unanswered, reachingPrevs(pdus, fill.Pulled, states.unfetched))
	return fill, nil
}

// reachingPrevs returns the PDUs reaching, along prev_events through the events pulled, one of the
// prev events, without which the events citing them are dropped
func reachingPrevs(pdus, pulled []*types.Event, prevIDs map[id.EventID]struct{}) map[id.EventID]struct{} {
	reaching := make(map[id.EventID]struct{}, len(prevIDs))
	maps.Copy(reaching, prevIDs)
	for _, ev := range rooms.SortEventsTopologically(slices.Concat(pulled, pdus)) {
		if slices.ContainsFunc(ev.PrevEventIDs, func(prevID id.EventID) bool {
			_, found := reaching[prevID]
			return found
		}) {
			reaching[ev.ID] = struct{}{}
		}
	}
	pdusReaching := make(map[id.EventID]struct{})
	for _, ev := range pdus {
		if _, found := reaching[ev.ID]; found {
			pdusReaching[ev.ID] = struct{}{}
		}
	}
	return pdusReaching
}

type missingEventsFunc func(ctx context.Context, server string, latestIDs, pulledIDs []id.EventID) ([]*types.Event, error)

// gapPlan is the get_missing_events requests of a gap fill. A PDU's gap is open while it cites a
// prev event neither stored nor pulled. Each request names few PDUs, as servers cap the events they
// answer with, and once a server failed or refused, it is not asked again.
type gapPlan struct {
	pdus    []*types.Event
	origins map[id.EventID]string
	missing map[id.EventID]struct{}

	pulledFrom map[string][]*types.Event
	pulledIDs  []id.EventID
	pulled     map[id.EventID]struct{}
	asked      map[string]map[id.EventID]struct{}
	dropped    map[string]struct{}
	// PDUs a server failed to answer for, other than by refusing
	failed map[id.EventID]struct{}
	errs   []error
}

func newGapPlan(targets []*types.IngestTarget, missingIDs []id.EventID) *gapPlan {
	plan := &gapPlan{
		pdus:       make([]*types.Event, len(targets)),
		origins:    make(map[id.EventID]string, len(targets)),
		missing:    make(map[id.EventID]struct{}, len(missingIDs)),
		pulledFrom: make(map[string][]*types.Event),
		pulled:     make(map[id.EventID]struct{}),
		asked:      make(map[string]map[id.EventID]struct{}),
		dropped:    make(map[string]struct{}),
		failed:     make(map[id.EventID]struct{}),
	}
	for i, target := range targets {
		plan.pdus[i] = target.Event
		if len(target.Servers) > 0 {
			plan.origins[target.Event.ID] = target.Servers[0]
		}
	}
	for _, missingID := range missingIDs {
		plan.missing[missingID] = struct{}{}
	}
	return plan
}

// run asks each server for the open gaps of the PDUs it sent, then the first maxGapFillServers for
// the gaps still open
func (p *gapPlan) run(ctx context.Context, servers []string, missingEvents missingEventsFunc) error {
	for _, server := range servers {
		if err := p.ask(ctx, server, true, missingEvents); err != nil {
			return err
		}
	}
	for _, server := range servers[:min(len(servers), maxGapFillServers)] {
		if err := p.ask(ctx, server, false, missingEvents); err != nil {
			return err
		}
	}
	return nil
}

// ask asks a server for the open gaps it was not asked for yet, only of the PDUs it sent if own is
// set, a few PDUs a request
func (p *gapPlan) ask(ctx context.Context, server string, own bool, missingEvents missingEventsFunc) error {
	asked := p.asked[server]
	if asked == nil {
		asked = make(map[id.EventID]struct{})
		p.asked[server] = asked
	}
	for {
		if _, found := p.dropped[server]; found {
			return nil
		}
		var open []id.EventID
		for _, ev := range p.pdus {
			_, wasAsked := asked[ev.ID]
			if !wasAsked && (!own || p.origins[ev.ID] == server) && p.open(ev) {
				open = append(open, ev.ID)
			}
		}
		if len(open) == 0 {
			return nil
		}
		latestIDs := open[:min(len(open), gapPDUsPerRequest)]
		for _, eventID := range latestIDs {
			asked[eventID] = struct{}{}
		}
		evs, err := missingEvents(ctx, server, latestIDs, p.pulledIDs)
		util.ReportProgress(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		} else if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("server", server).Msg("Failed to get missing events from server")
			p.errs = append(p.errs, fmt.Errorf("%s: %w", server, err))
			p.dropped[server] = struct{}{}
			if !refusedRequest(err) {
				for _, eventID := range open {
					p.failed[eventID] = struct{}{}
				}
			}
			return nil
		}
		for _, ev := range evs {
			if _, found := p.pulled[ev.ID]; !found {
				p.pulled[ev.ID] = struct{}{}
				p.pulledIDs = append(p.pulledIDs, ev.ID)
				p.pulledFrom[server] = append(p.pulledFrom[server], ev)
			}
		}
	}
}

// open reports whether a PDU cites a prev event neither stored, in the PDUs nor pulled
func (p *gapPlan) open(ev *types.Event) bool {
	return slices.ContainsFunc(ev.PrevEventIDs, func(prevID id.EventID) bool {
		_, missing := p.missing[prevID]
		_, pulled := p.pulled[prevID]
		return missing && !pulled
	})
}

func (p *gapPlan) unanswered() map[id.EventID]struct{} {
	unanswered := make(map[id.EventID]struct{})
	for _, ev := range p.pdus {
		if _, found := p.failed[ev.ID]; found && p.open(ev) {
			unanswered[ev.ID] = struct{}{}
		}
	}
	return unanswered
}

// refusedRequest reports whether a server answered a request with what asking again would not
// change: a status below 500 other than 429, or a response that is not the JSON asked for. A
// response whose body failed to arrive whole, as one timing out or cut off, was not answered.
func refusedRequest(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return false
	}
	status := httpErr.Response.StatusCode
	if status < http.StatusMultipleChoices {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		return errors.As(err, &syntaxErr) || errors.As(err, &typeErr) ||
			errors.Is(err, mautrix.ErrResponseTooLong) || errors.Is(err, mautrix.ErrBodyReadReachedLimit)
	}
	return status < http.StatusInternalServerError && status != http.StatusTooManyRequests
}

func (f *Fetcher) missingEvents(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	latestIDs, earliestIDs []id.EventID,
	minDepth int64,
	seenIDs map[id.EventID]struct{},
) ([]*types.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, f.requestTimeout())
	defer cancel()
	// Events from before this server's earliest timeline event, such as its join, are kept out of the
	// timeline as Synapse does, and the state at them fetched instead
	limit := cmp.Or(f.config.Federation.MaxFetchMissingEvents, defaultMissingEventsLimit)
	resp, err := f.gapClient.GetMissingEvents(ctx, &federation.ReqGetMissingEvents{
		ServerName:     server,
		RoomID:         roomID,
		EarliestEvents: earliestIDs,
		LatestEvents:   latestIDs,
		Limit:          limit,
		MinDepth:       int(minDepth),
	})
	if err != nil {
		return nil, err
	}
	remoteEvs, err := util.VerifyRemoteEvents(ctx, resp.Events[:min(len(resp.Events), limit)], roomVersion, f.keyStore, seenIDs)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(eventsInRoom(ctx, remoteEvs, roomID), func(ev *types.Event) bool {
		// Not every server honours min_depth
		return ev.Depth < minDepth
	}), nil
}

// prevEventStates are the states a gap fill fetched at prev events of the events it pulled: those
// given, and the prev events a server failed to give the state at, other than by refusing, or that
// the fill's bound left unfetched
type prevEventStates struct {
	given     *rooms.GivenStates
	unfetched map[id.EventID]struct{}
	errs      []error
}

// addPrevEventStates adds to the states, fetched from a server, the state before the prev events of
// the events it pulled that have none here, within what a gap fill fetches
func (f *Fetcher) addPrevEventStates(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	evs, pulled []*types.Event,
	states *prevEventStates,
) error {
	log := zerolog.Ctx(ctx).With().
		Stringer("room_id", roomID).
		Str("server", server).
		Logger()

	if len(pulled) == 0 {
		return nil
	}
	prevIDs, err := f.db.Rooms.GetPrevEventsWithoutState(ctx, roomID, evs, pulled)
	if err != nil {
		return err
	} else if states.given != nil {
		prevIDs = slices.DeleteFunc(prevIDs, func(prevID id.EventID) bool {
			_, found := states.given.BeforePrevs[prevID]
			return found
		})
	}
	if len(prevIDs) == 0 {
		return nil
	} else if states.given == nil {
		states.given = &rooms.GivenStates{BeforePrevs: make(map[id.EventID][]id.EventID, len(prevIDs))}
	}
	if budget := max(maxPrevEventStatesFetched-len(states.given.BeforePrevs), 0); len(prevIDs) > budget {
		log.Warn().
			Int("prev_events", len(prevIDs)).
			Msg("Too many prev events without state, fetching the state at some of them")
		for _, prevID := range prevIDs[budget:] {
			states.unfetched[prevID] = struct{}{}
		}
		prevIDs = prevIDs[:budget]
	}

	known := make(map[id.EventID]struct{}, len(evs)+len(states.given.Events))
	for _, ev := range slices.Concat(evs, states.given.Events) {
		known[ev.ID] = struct{}{}
	}
	for _, prevID := range prevIDs {
		stateIDs, fetched, fetchErr, err := f.fetchPrevEventState(ctx, server, roomID, roomVersion, prevID, known)
		util.ReportProgress(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		} else if err != nil {
			return err
		} else if fetchErr != nil {
			log.Warn().Err(fetchErr).Stringer("prev_event_id", prevID).Msg("Failed to fetch the state at prev event")
			if !refusedRequest(fetchErr) {
				states.unfetched[prevID] = struct{}{}
				states.errs = append(states.errs, fmt.Errorf("%s: state at %s: %w", server, prevID, fetchErr))
			}
			continue
		}
		delete(states.unfetched, prevID)
		states.given.BeforePrevs[prevID] = stateIDs
		states.given.Events = append(states.given.Events, fetched...)
		for _, ev := range fetched {
			known[ev.ID] = struct{}{}
		}
	}
	return nil
}

// fetchPrevEventState fetches from a server the state at a prev event, with the events of it that
// are neither stored here nor known. The first error is the server failing to give it, the second
// this server's own failing.
func (f *Fetcher) fetchPrevEventState(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	prevID id.EventID,
	known map[id.EventID]struct{},
) ([]id.EventID, []*types.Event, error, error) {
	reqCtx, cancel := context.WithTimeout(ctx, f.requestTimeout())
	defer cancel()
	stateIDs, err := f.fedclient.GetStateIDs(reqCtx, server, roomID, prevID)
	if err != nil {
		return nil, nil, err, nil
	} else if stateIDs == nil {
		return nil, nil, fmt.Errorf("null state_ids response for %s", prevID), nil
	}
	wanted := slices.DeleteFunc(
		slices.Concat([]id.EventID{prevID}, stateIDs.PDUs, stateIDs.AuthChain),
		func(eventID id.EventID) bool {
			_, found := known[eventID]
			return found
		},
	)
	slices.Sort(wanted)
	missing, err := f.db.Rooms.GetUnstoredEventIDs(ctx, slices.Compact(wanted))
	if err != nil {
		return nil, nil, nil, err
	}
	unfetched := make(map[id.EventID]struct{}, len(missing))
	for _, eventID := range missing {
		unfetched[eventID] = struct{}{}
	}

	var pdus []federation.PDU
	// Fetching each event has a lot of overhead, so as Synapse does the whole state is fetched when
	// many are missing
	if len(missing)*10 >= len(stateIDs.PDUs)+len(stateIDs.AuthChain) {
		resp, err := f.fedclient.GetState(reqCtx, server, roomID, prevID)
		if err != nil {
			return nil, nil, err, nil
		} else if resp == nil {
			return nil, nil, fmt.Errorf("null state response for %s", prevID), nil
		}
		pdus = slices.Concat(resp.PDUs, resp.AuthChain)
		if _, found := unfetched[prevID]; found {
			pdus = append(pdus, f.fetchEvents(reqCtx, server, []id.EventID{prevID})...)
		}
	} else {
		pdus = f.fetchEvents(reqCtx, server, missing)
	}

	pdus = wantedPDUs(ctx, pdus, roomVersion, unfetched)
	remoteEvs, err := util.VerifyRemoteEvents(reqCtx, pdus, roomVersion, f.keyStore, make(map[id.EventID]struct{}))
	if err != nil {
		return nil, nil, err, nil
	}
	fetched := make([]*types.Event, 0, len(missing))
	for _, ev := range eventsInRoom(ctx, remoteEvs, roomID) {
		if _, found := unfetched[ev.ID]; found {
			delete(unfetched, ev.ID)
			fetched = append(fetched, ev)
		}
	}
	if _, found := unfetched[prevID]; found {
		return nil, nil, fmt.Errorf("failed to fetch prev event %s", prevID), nil
	} else if len(unfetched) > 0 {
		zerolog.Ctx(ctx).Warn().
			Stringer("prev_event_id", prevID).
			Int("events", len(unfetched)).
			Msg("Failed to fetch some events of the state at prev event")
	}
	return stateIDs.PDUs, fetched, nil, nil
}

// StateBefore fetches the state before an event of the room another server holds, with its auth
// chain, as a send_join response gives the state before a join. Every event is fetched, stored here
// or not, as a remote join takes the state from the events given. More events than the limit fail
// with a tooLargeFailure before any is verified. The server may take as long to answer as for a
// send_join, meanwhile reporting progress.
func (f *Fetcher) StateBefore(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	eventID id.EventID,
	limit int,
) (stateEvs, authEvs []*types.Event, err error) {
	fetchCtx, cancel := context.WithTimeout(ctx, f.config.Rooms.Ingest.SendJoinResponseTimeout)
	defer cancel()
	waited := reportProgressEvery(fetchCtx, f.config)
	resp, err := f.streamClient.GetState(fetchCtx, server, roomID, eventID)
	waited()
	if err != nil {
		return nil, nil, err
	} else if resp == nil {
		return nil, nil, fmt.Errorf("null state response for %s", eventID)
	} else if count := len(resp.PDUs) + len(resp.AuthChain); count > limit {
		return nil, nil, tooLargeFailure("state and auth chain have", count, limit)
	}
	seenIDs := make(map[id.EventID]struct{}, len(resp.PDUs)+len(resp.AuthChain))
	if stateEvs, err = util.VerifyRemoteEvents(ctx, resp.PDUs, roomVersion, f.keyStore, seenIDs); err != nil {
		return nil, nil, err
	} else if authEvs, err = util.VerifyRemoteEvents(ctx, resp.AuthChain, roomVersion, f.keyStore, seenIDs); err != nil {
		return nil, nil, err
	}
	return eventsInRoom(ctx, stateEvs, roomID), eventsInRoom(ctx, authEvs, roomID), nil
}

// Keeps the PDUs of the wanted events, told apart by reference hash before any is verified
func wantedPDUs(ctx context.Context, pdus []federation.PDU, roomVersion string, wanted map[id.EventID]struct{}) []federation.PDU {
	kept := make([]federation.PDU, 0, len(wanted))
	for _, pdu := range pdus {
		ev := &types.Event{RoomVersion: roomVersion}
		var eventID id.EventID
		err := json.Unmarshal(pdu, ev)
		if err == nil {
			eventID, err = util.GetEventReferenceHash(ev)
		}
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Skipping fetched event that cannot be parsed")
		} else if _, found := wanted[eventID]; found {
			kept = append(kept, pdu)
		}
	}
	return kept
}

// Fetches events from a server one request each, skipping any that fail
func (f *Fetcher) fetchEvents(ctx context.Context, server string, eventIDs []id.EventID) []federation.PDU {
	pdus := make([][]federation.PDU, len(eventIDs))
	limit := make(chan struct{}, prevEventStatesFetchConcurrency)
	var wg sync.WaitGroup
	for i, eventID := range eventIDs {
		limit <- struct{}{}
		wg.Go(func() {
			defer func() { <-limit }()
			defer util.RecoverPanic(zerolog.Ctx(ctx), nil)
			resp, err := f.fedclient.GetEvent(ctx, server, eventID)
			if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Stringer("event_id", eventID).Msg("Failed to fetch event")
				return
			}
			pdus[i] = resp.PDUs
		})
	}
	wg.Wait()
	return slices.Concat(pdus...)
}

// Keeps the events of the room, which another server may not stick to
func eventsInRoom(ctx context.Context, evs []*types.Event, roomID id.RoomID) []*types.Event {
	return slices.DeleteFunc(evs, func(ev *types.Event) bool {
		if ev.RoomID == roomID {
			return false
		}
		zerolog.Ctx(ctx).Warn().
			Stringer("event_id", ev.ID).
			Stringer("event_room_id", ev.RoomID).
			Msg("Remote event is for another room, ignoring")
		return true
	})
}
