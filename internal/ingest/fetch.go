package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Prev events of a gap fill's pulled events whose state is fetched from the server that answered
	// get_missing_events. Events citing the others are dropped, and the state fetched again with the
	// next event citing them.
	maxPrevEventStatesFetched = 10
	// Events of a prev event's state fetched concurrently when too few are missing to fetch it whole
	prevEventStatesFetchConcurrency = 5
	// Each server's part of a gap fill, bounded as when /send ran it within the minute its sender waits
	getMissingEventsTimeout     = 20 * time.Second
	prevEventStatesFetchTimeout = 30 * time.Second
)

// errGapRefused is a gap fill every server asked refused, which asking again would not change
var errGapRefused = errors.New("every server asked refused the events the PDUs cite")

// Fetcher pulls from other servers the events and state that a room's events need and this server
// does not store, for the room's ingest job
type Fetcher struct {
	config    config.BabbleConfig
	db        *databases.Databases
	fedclient *federation.Client
	keyStore  *util.KeyStore
}

func NewFetcher(cfg config.BabbleConfig, db *databases.Databases, fedclient *federation.Client, keyStore *util.KeyStore) *Fetcher {
	return &Fetcher{config: cfg, db: db, fedclient: fedclient, keyStore: keyStore}
}

// GapFill pulls the events missing between a joined room's extremities and the PDUs citing prev
// events missing here, asking each server in turn until one answers get_missing_events, then fetches
// from that server the state at the pulled events' prev events that have none here. As Synapse does,
// it never fetches the state at the PDUs' own prev events: their sender pushed them, so should have
// what they cite. Fails wrapping errGapRefused when every server asked refused.
func (f *Fetcher) GapFill(
	ctx context.Context,
	servers []string,
	roomID id.RoomID,
	roomVersion string,
	pdus []*types.Event,
) ([]*types.Event, *rooms.GivenStates, error) {
	missing, err := f.db.Rooms.GetMissingEventsRequest(ctx, roomID, pdus)
	if err != nil {
		return nil, nil, err
	} else if len(missing.Missing) == 0 {
		return nil, nil, nil
	}
	var errs []error
	refused := true
	for _, server := range servers {
		pulled, err := f.missingEvents(ctx, server, roomID, roomVersion, pdus, missing)
		util.ReportProgress(ctx)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		} else if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("server", server).Msg("Failed to get missing events from server")
			errs = append(errs, fmt.Errorf("%s: %w", server, err))
			refused = refused && refusedRequest(err)
			continue
		}
		given := f.prevEventStates(ctx, server, roomID, roomVersion, slices.Concat(pulled, pdus), pulled)
		return pulled, given, nil
	}
	if len(errs) > 0 && refused {
		return nil, nil, fmt.Errorf("%w: %w", errGapRefused, errors.Join(errs...))
	}
	return nil, nil, errors.Join(errs...)
}

// refusedRequest reports whether a server answered a request with what asking again would not
// change: a status below 500 other than 429, or a response that is not the JSON asked for
func refusedRequest(err error) bool {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return false
	}
	return httpErr.Response.StatusCode < http.StatusInternalServerError && httpErr.Response.StatusCode != http.StatusTooManyRequests
}

// missingEvents pulls from a server the events missing before the PDUs, returning those pulled
func (f *Fetcher) missingEvents(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	pdus []*types.Event,
	missing *rooms.MissingEventsRequest,
) ([]*types.Event, error) {
	var latestIDs []id.EventID
	for _, ev := range pdus {
		if slices.ContainsFunc(ev.PrevEventIDs, func(prevID id.EventID) bool {
			_, found := slices.BinarySearch(missing.Missing, prevID)
			return found
		}) {
			latestIDs = append(latestIDs, ev.ID)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, getMissingEventsTimeout)
	defer cancel()
	// TODO: paginate if this doesn't get everything
	// Events from before this server's earliest timeline event, such as its join, are kept out of the
	// timeline as Synapse does, and the state at them fetched instead
	resp, err := f.fedclient.GetMissingEvents(ctx, &federation.ReqGetMissingEvents{
		ServerName:     server,
		RoomID:         roomID,
		EarliestEvents: missing.Extremities,
		LatestEvents:   latestIDs,
		Limit:          f.config.Federation.MaxFetchMissingEvents,
		MinDepth:       int(missing.MinDepth),
	})
	if err != nil {
		return nil, err
	}

	seenIDs := make(map[id.EventID]struct{}, len(pdus)+len(resp.Events))
	for _, ev := range pdus {
		seenIDs[ev.ID] = struct{}{}
	}
	remoteEvs, err := util.VerifyRemoteEvents(ctx, resp.Events, roomVersion, f.keyStore, seenIDs)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(eventsInRoom(ctx, remoteEvs, roomID), func(ev *types.Event) bool {
		// Not every server honours min_depth
		return ev.Depth < missing.MinDepth
	}), nil
}

// prevEventStates fetches the state before the prev events of the pulled events that have none here
func (f *Fetcher) prevEventStates(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	evs, pulled []*types.Event,
) *rooms.GivenStates {
	log := zerolog.Ctx(ctx).With().
		Stringer("room_id", roomID).
		Str("server", server).
		Logger()

	if len(pulled) == 0 {
		return nil
	}
	prevIDs, err := f.db.Rooms.GetPrevEventsWithoutState(ctx, roomID, evs, pulled)
	if err != nil {
		log.Err(err).Msg("Failed to get prev events without state")
		return nil
	} else if len(prevIDs) == 0 {
		return nil
	} else if len(prevIDs) > maxPrevEventStatesFetched {
		log.Warn().
			Int("prev_events", len(prevIDs)).
			Msg("Too many prev events without state, fetching the state at some of them")
		prevIDs = prevIDs[:maxPrevEventStatesFetched]
	}

	ctx, cancel := context.WithTimeout(ctx, prevEventStatesFetchTimeout)
	defer cancel()

	known := make(map[id.EventID]struct{}, len(evs))
	for _, ev := range evs {
		known[ev.ID] = struct{}{}
	}
	prevStates := &rooms.GivenStates{BeforePrevs: make(map[id.EventID][]id.EventID, len(prevIDs))}
	for _, prevID := range prevIDs {
		stateIDs, fetched, err := f.fetchPrevEventState(ctx, server, roomID, roomVersion, prevID, known)
		if err != nil {
			log.Warn().Err(err).Stringer("prev_event_id", prevID).Msg("Failed to fetch the state at prev event")
			continue
		}
		util.ReportProgress(ctx)
		prevStates.BeforePrevs[prevID] = stateIDs
		prevStates.Events = append(prevStates.Events, fetched...)
		for _, ev := range fetched {
			known[ev.ID] = struct{}{}
		}
	}
	return prevStates
}

func (f *Fetcher) fetchPrevEventState(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	prevID id.EventID,
	known map[id.EventID]struct{},
) ([]id.EventID, []*types.Event, error) {
	stateIDs, err := f.fedclient.GetStateIDs(ctx, server, roomID, prevID)
	if err != nil {
		return nil, nil, err
	} else if stateIDs == nil {
		return nil, nil, fmt.Errorf("null state_ids response for %s", prevID)
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
		return nil, nil, err
	}
	unfetched := make(map[id.EventID]struct{}, len(missing))
	for _, eventID := range missing {
		unfetched[eventID] = struct{}{}
	}

	var pdus []federation.PDU
	// Fetching each event has a lot of overhead, so as Synapse does the whole state is fetched when
	// many are missing
	if len(missing)*10 >= len(stateIDs.PDUs)+len(stateIDs.AuthChain) {
		resp, err := f.fedclient.GetState(ctx, server, roomID, prevID)
		if err != nil {
			return nil, nil, err
		} else if resp == nil {
			return nil, nil, fmt.Errorf("null state response for %s", prevID)
		}
		pdus = slices.Concat(resp.PDUs, resp.AuthChain)
		if _, found := unfetched[prevID]; found {
			pdus = append(pdus, f.fetchEvents(ctx, server, []id.EventID{prevID})...)
		}
	} else {
		pdus = f.fetchEvents(ctx, server, missing)
	}

	pdus = wantedPDUs(ctx, pdus, roomVersion, unfetched)
	remoteEvs, err := util.VerifyRemoteEvents(ctx, pdus, roomVersion, f.keyStore, make(map[id.EventID]struct{}))
	if err != nil {
		return nil, nil, err
	}
	fetched := make([]*types.Event, 0, len(missing))
	for _, ev := range eventsInRoom(ctx, remoteEvs, roomID) {
		if _, found := unfetched[ev.ID]; found {
			delete(unfetched, ev.ID)
			fetched = append(fetched, ev)
		}
	}
	if _, found := unfetched[prevID]; found {
		return nil, nil, fmt.Errorf("failed to fetch prev event %s", prevID)
	} else if len(unfetched) > 0 {
		zerolog.Ctx(ctx).Warn().
			Stringer("prev_event_id", prevID).
			Int("events", len(unfetched)).
			Msg("Failed to fetch some events of the state at prev event")
	}
	return stateIDs.PDUs, fetched, nil
}

// StateBefore fetches the state before an event of the room another server holds, with its auth
// chain, as a send_join response gives the state before a join. Every event is fetched, stored here
// or not, as a remote join takes the state from the events given. More events than the limit fail
// with a tooLargeFailure before any is verified.
func (f *Fetcher) StateBefore(
	ctx context.Context,
	server string,
	roomID id.RoomID,
	roomVersion string,
	eventID id.EventID,
	limit int,
) (stateEvs, authEvs []*types.Event, err error) {
	resp, err := f.fedclient.GetState(ctx, server, roomID, eventID)
	if err != nil {
		return nil, nil, err
	} else if resp == nil {
		return nil, nil, fmt.Errorf("null state response for %s", eventID)
	} else if count := len(resp.PDUs) + len(resp.AuthChain); count > limit {
		return nil, nil, tooLargeFailure(count, limit)
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
