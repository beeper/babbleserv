package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type reqTransaction struct {
	Origin          string `json:"origin"`
	OriginTimestamp int64  `json:"origin_server_ts"`

	PDUs []*types.Event `json:"pdus"`
	EDUs []*types.EDU   `json:"edus"`
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

	// TODO: check if we already processed this - prob need to lock the whole call!
	// refresh routine on lock, takes ctx, after wg wait cancel it
	if req.Origin != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"Transaction origin does not match requesting server",
		)
		return
	}

	var wg sync.WaitGroup
	var resp *respTransaction
	var pduErr, eduErr error

	wg.Add(1)
	go func() {
		resp, pduErr = f.processTransactionPDUs(r, req.Origin, req.PDUs)
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		eduErr = f.processTransactionEDUs(r, req.EDUs)
		wg.Done()
	}()

	wg.Wait()

	if pduErr != nil {
		util.ResponseErrorUnknownJSON(w, r, pduErr)
		return
	}
	if eduErr != nil {
		util.ResponseErrorUnknownJSON(w, r, eduErr)
		return
	}

	hlog.FromRequest(r).Info().Msg("Processed incoming federation transaction")
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
