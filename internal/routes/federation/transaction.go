package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/tidwall/gjson"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/notifier"
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

	txnID := chi.URLParam(r, "txnID")
	if txnID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	}

	log := hlog.FromRequest(r).With().
		Str("txn_id", txnID).
		Logger()

	// TODO: check if we already processed this - prob need to lock the whole call!
	// refresh routine on lock, takes ctx, after wg wait cancel it
	if req.Origin != middleware.GetRequestServer(r) {
		util.ResponseErrorMessageJSON(
			w, r, mautrix.MForbidden,
			"Transaction origin does not match requesting server",
		)
		return
	}

	log.Debug().Msg("Processing incoming federation transaction")

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

	log.Info().Msg("Processed incoming federation transaction")
	util.ResponseJSON(w, r, http.StatusOK, resp)
}

func (f *FederationRoutes) processTransactionEDUs(r *http.Request, edus []*types.EDU) error {
	log := hlog.FromRequest(r)

	// Group edus by type
	edusByType := make(map[types.EDUType][]*types.EDU, 3)
	for _, edu := range edus {
		edusByType[edu.Type] = append(edusByType[edu.Type], edu)
	}

	var wg sync.WaitGroup

	for eduType, eus := range edusByType {
		wg.Add(1)
		go func(eduType types.EDUType, edus []*types.EDU) {
			defer wg.Done()

			switch eduType {
			case types.EDUTypeDeviceListUpdate:
				for _, edu := range edus {
					var content types.DeviceListUpdateEDUContent
					if err := json.Unmarshal(edu.Content, &content); err != nil {
						log.Err(err).Msg("Failed to unmarshal m.device_list_update content")
						return
					}
					if err := f.db.Accounts.StoreDeviceChange(r.Context(), content.UserID, content.DeviceID); err != nil {
						log.Err(err).Msg("Failed to store remote device change")
						return
					}
					f.notifiers.Accounts.SendChange(notifier.Change{
						UserIDs: []id.UserID{content.UserID},
					})
					log.Debug().
						Str("user_id", content.UserID.String()).
						Str("device_id", content.DeviceID.String()).
						Msg("Processed remote device list update")

					// TODO: store the keys! Check streamID! Bump user device list version!
				}

			case types.EDUTypeSigningKeyUpdate:
				for _, edu := range edus {
					var content types.SigningKeyUpdateEDUContent
					if err := json.Unmarshal(edu.Content, &content); err != nil {
						log.Err(err).Msg("Failed to unmarshal m.signing_key_update content")
						return
					}
					if err := f.db.Accounts.StoreDeviceChange(r.Context(), content.UserID, "*"); err != nil {
						log.Err(err).Msg("Failed to store remote device change")
						return
					}
					f.notifiers.Accounts.SendChange(notifier.Change{
						UserIDs: []id.UserID{content.UserID},
					})
					log.Debug().
						Str("user_id", content.UserID.String()).
						Msg("Processed remote signing key update")

					// TODO: store the keys!
				}

			case types.EDUTypeReceipt:
				// TODO

			case types.EDUTypeToDevice:
				tds := make([]*types.ToDevice, 0, len(edus))
				for _, edu := range edus {
					var content types.ToDeviceEDUContent
					if err := json.Unmarshal(edu.Content, &content); err != nil {
						panic(err)
					}

					for userID, deviceIDToContent := range content.Messages {
						if userID.Homeserver() != f.config.ServerName {
							log.Error().
								Str("user_id", userID.String()).
								Msg("Ignoring to-device for nonlocal user")
						}
						for deviceID, contentB := range deviceIDToContent {
							cnt, err := json.Marshal(contentB)
							if err != nil {
								log.Err(err).
									Any("content", contentB).
									Msg("Ignoring federated to-device message with invalid JSON content")
								continue
							}
							tds = append(tds, &types.ToDevice{
								Sender:   content.Sender,
								Type:     content.Type,
								UserID:   userID,
								DeviceID: deviceID,
								Content:  cnt,
							})
						}
					}
				}

				_, err := f.db.Transient.SendToDeviceEvents(r.Context(), tds, transient.SendToDeviceOptions{})
				if err != nil {
					panic(err)
				}

			default:
				log.Error().Str("type", string(eduType)).Msg("Ignoring unknown EDU type")
			}
		}(eduType, eus)
	}

	wg.Wait()
	return nil
}

func (f *FederationRoutes) processTransactionPDUs(r *http.Request, origin string, pdus []*types.Event) (*respTransaction, error) {
	verifyResults := rooms.SendEventsResult{
		Allowed:  make([]*types.Event, 0, len(pdus)),
		Rejected: make([]rooms.RejectedEvent, 0),
	}

	roomVersions := make(map[id.RoomID]string, 1)

	// Run some pre-checks before we send the events to the database layer
	for _, ev := range pdus {
		if roomVersions[ev.RoomID] == "" {
			room, err := f.db.Rooms.GetRoom(r.Context(), ev.RoomID)
			if err != nil {
				return nil, err
			} else if room == nil {
				if ev.Type == event.StateCreate && f.config.SecretSwitches.EnableFederatedSendRoomCreate {
					// If no room, and this is a create event, and we're allowed to receive create
					// events over federation - pull the room version from the event content.
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
				Stringer("type", ev.Type).
				Msg("Silently dropping event from unknown room")
			continue
		}
		ev.RoomVersion = roomVersions[ev.RoomID]

		verifyErr, err := util.VerifyEvent(r.Context(), ev, origin, f.keyStore)
		if err != nil {
			return nil, err
		} else if verifyErr == types.ErrEventRedacted {
			redactedEv, err := ev.GetRedactedEvent()
			if err != nil {
				return nil, err
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

	// Split up the PDUs by room
	roomToEvs := make(map[id.RoomID][]*types.Event, 5)
	for _, pdu := range verifyResults.Allowed {
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
		Str("origin", origin).
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
			// Now we have all the events we're going to try to send, fetch any missing
			// prev or auth events so we can send those as well (or we'll reject).
			var err error
			evs, err = f.getMissingEventsForSendBatch(r.Context(), origin, roomID, roomVersions[roomID], evs)
			if err != nil {
				hlog.FromRequest(r).Err(err).Msg("Get missing events failed")
				return
			}
			options := rooms.SendFederatedEventsOptions{}
			results, err := f.db.Rooms.SendFederatedEvents(backgroundCtx, roomID, evs, options)
			if err != nil {
				// This is *BAD*, an unexpected error handling results for a room, we can't bail the
				// request here as we'll poison other parallel room sends. So we just log and none
				// of the events in question will be in the response.
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

	resp := respTransaction{make(map[id.EventID]respTransactionResult, len(pdus))}

	for _, rejected := range verifyResults.Rejected {
		resp.PDUs[rejected.Event.ID] = respTransactionResult{
			// https://spec.matrix.org/v1.14/server-server-api/#rejection
			// "If an event in an incoming transaction is rejected, this should not cause the transaction request to be responded to with an error response."
			// Error: rejected.Error.Error(),
		}
	}

	for _, results := range allResults {
		for _, allowed := range results.Allowed {
			resp.PDUs[allowed.ID] = respTransactionResult{}
		}
		for _, rejected := range results.Rejected {
			resp.PDUs[rejected.Event.ID] = respTransactionResult{
				// As above
				// Error: rejected.Error.Error(),
			}
		}
	}

	return &resp, nil
}

func (f *FederationRoutes) getMissingEventsForSendBatch(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	roomVersion string,
	evs []*types.Event,
) ([]*types.Event, error) {
	eventsWithMissingPrevs := make(map[id.EventID]struct{}, len(evs))
	eventsWeHave := make(map[id.EventID]struct{}, len(evs))
	// Add these events incase they reference each other
	for _, ev := range evs {
		eventsWeHave[ev.ID] = struct{}{}
	}

	// Find events that have one or more missing events
	for _, ev := range evs {
		for _, prevID := range ev.PrevEventIDs {
			if _, ok := eventsWeHave[prevID]; ok {
				continue
			} else if exists, err := f.db.Rooms.DoesEventExist(ctx, prevID); err != nil {
				return nil, err
			} else if exists {
				eventsWeHave[prevID] = struct{}{}
				continue
			} else {
				eventsWithMissingPrevs[ev.ID] = struct{}{}
				break
			}
		}
	}

	if len(eventsWithMissingPrevs) == 0 {
		return evs, nil
	}

	// Now get the current room extremeties we know of, because it's possible we have none of the
	// prev events and we need to tell the other HS where to stop searching.
	roomExtremIDs, err := f.db.Rooms.GetRoomCurrentExtremEventIDs(ctx, roomID)
	if err != nil {
		return nil, err
	}

	// TODO: paginate if this doesn't get everything
	remoteEvs, err := f.fedclient.GetMissingEvents(ctx, &federation.ReqGetMissingEvents{
		ServerName:     origin,
		RoomID:         roomID,
		EarliestEvents: roomExtremIDs,
		LatestEvents:   slices.Collect(maps.Keys(eventsWithMissingPrevs)),
		Limit:          f.config.Federation.MaxFetchMissingEvents,
	})
	if err != nil {
		return nil, err
	}

	for _, b := range remoteEvs.Events {
		var ev types.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, err
		} else if ev.Origin != origin {
			return nil, fmt.Errorf("event origin mismatch")
		}
		ev.RoomVersion = roomVersion

		if verifyErr, err := util.VerifyEvent(ctx, &ev, origin, f.keyStore); err != nil {
			return nil, err
		} else if verifyErr != nil {
			zerolog.Ctx(ctx).Warn().Err(verifyErr).Msg("Missing event failed verification, ignoring")
			continue
		}

		evs = append(evs, &ev)
	}

	util.SortEventList(evs)
	return evs, nil
}
