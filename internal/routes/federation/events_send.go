package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/tidwall/gjson"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type reqTransaction struct {
	Origin          string `json:"origin"`
	OriginTimestamp int64  `json:"origin_server_ts"`

	PDUs []*types.Event   `json:"pdus"`
	EDUs []map[string]any `json:"edus"`
}

type respTransactionResult struct {
	Error string `json:"error,omitempty"`
}

type respTransaction struct {
	PDUs map[id.EventID]respTransactionResult `json:"pdus"`
}

func (f *FederationRoutes) SendTransaction(w http.ResponseWriter, r *http.Request) {
	var req reqTransaction
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	if req.Origin != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"Transaction origin does not match requesting server",
		)
		return
	}

	verifyResults := rooms.SendEventsResult{
		Allowed:  make([]*types.Event, 0, len(req.PDUs)),
		Rejected: make([]rooms.RejectedEvent, 0),
	}

	roomVersions := make(map[id.RoomID]string, 1)

	// Run some pre-checks before we send the events to the database layer
	for _, ev := range req.PDUs {
		if roomVersions[ev.RoomID] == "" {
			room, err := f.db.Rooms.GetRoom(r.Context(), ev.RoomID)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			} else if room == nil {
				if ev.Type == event.StateCreate && f.config.SecretSwitches.EnableFederatedSendRoomCreate {
					// If no room, and this is a create event, and we're allowed to
					// receive create events over federation (ie - not prod) - pull
					// the room version from the event content.
					rmver := gjson.GetBytes(ev.Content, "room_version")
					if rmver.Exists() {
						roomVersions[ev.RoomID] = rmver.String()
					}
				}
			} else {
				roomVersions[ev.RoomID] = room.Version
			}
		}

		if roomVersions[ev.RoomID] == "" {
			// If we have no room version we can't calculate the reference hash,
			// so we *silently* drop it (synapse + dendrite do this, spec unclear).
			hlog.FromRequest(r).Warn().
				Stringer("room_id", ev.RoomID).
				Msg("Silently dropping event from unknown room")
			continue
		}
		ev.RoomVersion = roomVersions[ev.RoomID]

		verifyErr, err := util.VerifyEvent(r.Context(), ev, req.Origin, f.keyStore)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if verifyErr == types.ErrEventRedacted {
			redactedEv, err := ev.GetRedactedEvent()
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
			redactedEv.RoomVersion = roomVersions[ev.RoomID]
			redactedEv.ID = ev.ID
			hlog.FromRequest(r).Warn().
				Stringer("room_id", ev.RoomID).
				Stringer("event_id", ev.ID).
				Msg("Processing redacted event over federation")
			verifyResults.Allowed = append(verifyResults.Allowed, redactedEv)
		} else if verifyErr != nil {
			verifyResults.Rejected = append(verifyResults.Rejected, rooms.RejectedEvent{
				Event: ev,
				Error: verifyErr,
			})
		} else {
			verifyResults.Allowed = append(verifyResults.Allowed, ev)
		}
	}

	// Now we have all the events we're going to try to send, fetch any missing
	// prev or auth events so we can send those as well (or we'll reject).
	evs, err := f.getMissingEventsForSendBatch(r.Context(), req.Origin, roomVersions, verifyResults.Allowed)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// Split up the PDUs by room
	roomToEvs := make(map[id.RoomID][]*types.Event, 5)
	for _, pdu := range evs {
		if _, found := roomToEvs[pdu.RoomID]; !found {
			roomToEvs[pdu.RoomID] = make([]*types.Event, 0, 0)
		}
		roomToEvs[pdu.RoomID] = append(roomToEvs[pdu.RoomID], pdu)
	}

	// Switch to a background context here - we've done all the event verification
	// and fetching from remote and now we're going to pass them to the database
	// layer, where we don't want to end up in an inconsistent state. If the
	// sending server dies and retries the same events we'll OK the retry request
	// with "event already exists" errors.
	backgroundCtx := hlog.FromRequest(r).With().
		Str("background_task", "ProcessIncomingFederatedTransaction").
		Str("origin", req.Origin).
		Logger().
		WithContext(context.Background())

	var wg sync.WaitGroup
	doneCh := make(chan struct{})
	resultsCh := make(chan *rooms.SendEventsResult)
	allResults := make([]*rooms.SendEventsResult, 0, len(roomToEvs))

	go func() {
		for results := range resultsCh {
			allResults = append(allResults, results)
		}
		doneCh <- struct{}{}
	}()

	for roomID, evs := range roomToEvs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			options := rooms.SendFederatedEventsOptions{}
			results, err := f.db.Rooms.SendFederatedEvents(backgroundCtx, roomID, evs, options)
			if err != nil {
				// This is *BAD*, an unexpected error handling results for a room,
				// we can't bail the request here as we'll poison other parallel
				// room sends. So we just log and none of the events in question
				// will be in the response.
				// Does this make other servers angry? Will they retry those events?
				hlog.FromRequest(r).Err(err).Msg("Sending federated events failed")
				return
			}
			resultsCh <- results
		}()
	}

	wg.Wait()
	close(resultsCh)
	<-doneCh

	resp := respTransaction{make(map[id.EventID]respTransactionResult, len(req.PDUs))}

	for _, rejected := range verifyResults.Rejected {
		resp.PDUs[rejected.Event.ID] = respTransactionResult{
			Error: rejected.Error.Error(),
		}
	}

	for _, results := range allResults {
		for _, allowed := range results.Allowed {
			resp.PDUs[allowed.ID] = respTransactionResult{}
		}
		for _, rejected := range results.Rejected {
			resp.PDUs[rejected.Event.ID] = respTransactionResult{
				Error: rejected.Error.Error(),
			}
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, resp)
}

func (f *FederationRoutes) getMissingEventsForSendBatch(
	ctx context.Context,
	origin string,
	roomVersions map[id.RoomID]string,
	evs []*types.Event,
) ([]*types.Event, error) {
	eventsWeHave := make(map[id.EventID]struct{}, len(evs))
	for _, ev := range evs {
		eventsWeHave[ev.ID] = struct{}{}
	}

	completeEvs := make([]*types.Event, 0, len(evs))
	var ev *types.Event
	var fetched int

	handleEventID := func(evID id.EventID) error {
		if _, found := eventsWeHave[evID]; found {
			return nil
		} else if exists := f.db.Rooms.MustDoesEventExist(ctx, evID); exists {
			eventsWeHave[evID] = struct{}{}
			return nil
		} else {
			if fetched >= f.config.Federation.MaxFetchMissingEvents {
				return errors.New("too many missing events, rejecting batch")
			}

			zerolog.Ctx(ctx).Info().
				Str("event_id", evID.String()).
				Msg("Fetching missing event from remote server")

			res, err := f.fclient.GetEvent(
				ctx,
				spec.ServerName(f.config.ServerName),
				spec.ServerName(origin),
				evID.String(),
			)
			if err != nil {
				return err
			} else if len(res.PDUs) != 1 {
				return errors.New("invalid get event response from server")
			}

			fetched += 1
			b := res.PDUs[0]
			var ev types.Event
			if err := json.Unmarshal(b, &ev); err != nil {
				return err
			}
			ev.RoomVersion = roomVersions[ev.RoomID]

			if verifyErr, err := util.VerifyEvent(ctx, &ev, ev.Origin, f.keyStore); err != nil {
				return err
			} else if verifyErr == types.ErrEventRedacted {
				redactedEv, err := ev.GetRedactedEvent()
				if err != nil {
					return err
				}
				redactedEv.RoomVersion = roomVersions[ev.RoomID]
				redactedEv.ID = ev.ID
				ev = *redactedEv
				zerolog.Ctx(ctx).Warn().
					Str("room_id", ev.RoomID.String()).
					Str("event_id", ev.ID.String()).
					Msg("Processing redacted event fetched over federation")
			} else if verifyErr != nil {
				return fmt.Errorf("error verifying event: %w", verifyErr)
			}

			// Prepend it to our list of events to process, such that we process
			// this event next (keeps prev event chains together in the list).
			evs = append([]*types.Event{&ev}, evs...)
			return nil
		}
	}

	for len(evs) > 0 {
		// Pop the first event, check each of it's prev and auth events
		ev, evs = evs[0], evs[1:]
		for _, evID := range append(ev.PrevEventIDs, ev.AuthEventIDs...) {
			if err := handleEventID(evID); err != nil {
				zerolog.Ctx(ctx).Err(err).
					Str("room_id", ev.RoomID.String()).
					Str("event_id", ev.ID.String()).
					Msg("Error handling missing event")
			}
		}
		completeEvs = append(completeEvs, ev)
	}

	// Now re-sort the events since we appended prev events after each other
	util.SortEventList(completeEvs)

	return completeEvs, nil
}
