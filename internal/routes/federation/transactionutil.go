package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/rs/zerolog/hlog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (f *FederationRoutes) processTransactionEDUs(r *http.Request, edus []*types.EDU) error {
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
						for deviceID, contentB := range deviceIDToContent {
							cnt, err := json.Marshal(contentB)
							if err != nil {
								hlog.FromRequest(r).Err(err).
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

				res, err := f.db.Transient.SendToDeviceEvents(r.Context(), tds, transient.SendToDeviceOptions{})
				if err != nil {
					panic(err)
				}

				hlog.FromRequest(r).Info().Any("CONTENT", res).Msg("SENT RES")
			default:
				panic(fmt.Errorf("unknown edu type: %s", eduType))
			}
		}(eduType, eus)
	}

	// goroutine each, switch edu_type
	// - case m.to_device
	// - case m.receipt
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

	// Now we have all the events we're going to try to send, fetch any missing
	// prev or auth events so we can send those as well (or we'll reject).
	evs, err := f.getMissingEventsForSendBatch(r.Context(), origin, roomVersions, verifyResults.Allowed)
	if err != nil {
		return nil, err
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
