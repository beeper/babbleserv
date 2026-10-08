package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

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
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type reqTransaction struct {
	Origin          string `json:"origin"`
	OriginTimestamp int64  `json:"origin_server_ts"`

	PDUs []json.RawMessage `json:"pdus"`
	EDUs []*types.EDU      `json:"edus"`
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

	wg.Go(func() {
		defer util.RecoverPanic(&log, &pduErr)
		resp, pduErr = f.processTransactionPDUs(r, req.Origin, parseTransactionPDUs(&log, req.PDUs))
	})
	wg.Go(func() {
		defer util.RecoverPanic(&log, &eduErr)
		eduErr = f.processTransactionEDUs(r, req.Origin, req.EDUs)
	})
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

const remoteDeviceIngestConcurrency = 4

func (f *FederationRoutes) processTransactionEDUs(r *http.Request, origin string, edus []*types.EDU) error {
	log := hlog.FromRequest(r)

	edusByType := make(map[types.EDUType][]*types.EDU, 3)
	for _, edu := range edus {
		if edu != nil {
			edusByType[edu.Type] = append(edusByType[edu.Type], edu)
		}
	}

	var wg sync.WaitGroup
	for eduType, edus := range edusByType {
		wg.Go(func() {
			defer util.RecoverPanic(log, nil)

			switch eduType {
			case types.EDUTypeDeviceListUpdate, types.EDUTypeSigningKeyUpdate:
				f.processTransactionDeviceListEDUs(r, origin, eduType, edus)

			case types.EDUTypeReceipt:
				// TODO

			case types.EDUTypePresence:
				f.processTransactionPresenceEDUs(r, origin, eduType, edus)

			case types.EDUTypeToDevice:
				f.processTransactionToDeviceEDUs(r, origin, eduType, edus)

			default:
				log.Error().Str("type", string(eduType)).Msg("Ignoring unknown EDU type")
			}
		})
	}

	wg.Wait()
	return nil
}

func (f *FederationRoutes) processTransactionDeviceListEDUs(r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) {
	log := hlog.FromRequest(r)
	userEDUs := make(map[id.UserID][]types.RemoteDeviceEDU)
	for _, edu := range edus {
		var (
			userID id.UserID
			parsed types.RemoteDeviceEDU
			err    error
		)
		if eduType == types.EDUTypeDeviceListUpdate {
			var update types.RemoteDeviceListUpdate
			userID, update, err = util.ParseDeviceListUpdateEDU(edu.Content)
			parsed.DeviceList = &update
		} else {
			var update types.RemoteSigningKeyUpdate
			userID, update, err = util.ParseSigningKeyUpdateEDU(edu.Content)
			parsed.SigningKeys = &update
		}
		if err != nil {
			log.Warn().Err(err).Str("type", string(eduType)).Msg("Dropping EDU that cannot be attributed to a user")
		} else if checkEDUOrigin(log, origin, eduType, userID) {
			userEDUs[userID] = append(userEDUs[userID], parsed)
		}
	}

	var ingests sync.WaitGroup
	running := make(chan struct{}, remoteDeviceIngestConcurrency)
	for userID, updates := range userEDUs {
		running <- struct{}{}
		ingests.Go(func() {
			defer func() { <-running }()
			log := log.With().Stringer("user_id", userID).Logger()
			defer util.RecoverPanic(&log, nil)

			notified, err := f.db.Accounts.IngestRemoteDeviceUpdates(r.Context(), userID, updates)
			if err != nil {
				log.Err(err).Msg("Failed to ingest remote device updates")
				return
			}
			log.Debug().Int("updates", len(updates)).Bool("notified", notified).Msg("Ingested remote device updates")
		})
	}
	ingests.Wait()
}

func (f *FederationRoutes) processTransactionPresenceEDUs(r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) {
	log := hlog.FromRequest(r)
	for _, edu := range edus {
		var content types.PresenceEDUContent
		if err := json.Unmarshal(edu.Content, &content); err != nil {
			log.Warn().Err(err).Msg("Ignoring invalid m.presence content")
			continue
		}
		for _, presenceItem := range content.Push {
			if !checkEDUOrigin(log, origin, eduType, presenceItem.UserID) {
				continue
			}
			if localpart, _, err := presenceItem.UserID.ParseAndValidateRelaxed(); err != nil || localpart == "" ||
				presenceItem.LastActiveAgo < 0 ||
				presenceItem.LastActiveAgo > int64(math.MaxInt64/time.Millisecond) ||
				(presenceItem.Presence != event.PresenceOnline &&
					presenceItem.Presence != event.PresenceOffline &&
					presenceItem.Presence != event.PresenceUnavailable) {
				log.Warn().Stringer("user_id", presenceItem.UserID).
					Msg("Ignoring invalid m.presence EDU item")
				continue
			}
			// Calculate last active time from last_active_ago
			lastActive := time.Now()
			if presenceItem.LastActiveAgo > 0 {
				lastActive = lastActive.Add(-time.Duration(presenceItem.LastActiveAgo) * time.Millisecond)
			}

			presence := &types.Presence{
				UserID:     presenceItem.UserID,
				Presence:   presenceItem.Presence,
				Message:    presenceItem.StatusMsg,
				LastActive: lastActive,
			}

			if err := f.db.Transient.UpdateUserPresence(r.Context(), presenceItem.UserID, presence); err != nil {
				log.Err(err).
					Str("user_id", presenceItem.UserID.String()).
					Msg("Failed to store remote presence")
				continue
			}
			log.Debug().
				Str("user_id", presenceItem.UserID.String()).
				Str("presence", string(presenceItem.Presence)).
				Msg("Processed remote presence update")
		}
	}
}

func (f *FederationRoutes) processTransactionToDeviceEDUs(r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) {
	log := hlog.FromRequest(r)
	tds := make([]*types.ToDevice, 0, len(edus))
	for _, edu := range edus {
		var content types.ToDeviceEDUContent
		if err := json.Unmarshal(edu.Content, &content); err != nil {
			log.Err(err).Msg("Failed to unmarshal m.direct_to_device content")
			continue
		} else if !checkEDUOrigin(log, origin, eduType, content.Sender) {
			continue
		}

		for userID, deviceIDToContent := range content.Messages {
			if userID.Homeserver() != f.config.ServerName {
				log.Error().
					Str("user_id", userID.String()).
					Msg("Ignoring to-device for nonlocal user")
				continue
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
	if _, err := f.db.SendToDeviceEvents(r.Context(), tds, transient.SendToDeviceOptions{}); err != nil {
		log.Err(err).Msg("Failed to store remote to-device messages")
	}
}

func checkEDUOrigin(log *zerolog.Logger, origin string, eduType types.EDUType, userID id.UserID) bool {
	if userID.Homeserver() == origin {
		return true
	}
	log.Warn().
		Str("type", string(eduType)).
		Stringer("user_id", userID).
		Str("origin", origin).
		Msg("Dropping EDU about a user of another server")
	return false
}

// As Synapse does, a PDU that cannot be parsed is dropped without a result
func parseTransactionPDUs(log *zerolog.Logger, raws []json.RawMessage) []*types.Event {
	pdus := make([]*types.Event, 0, len(raws))
	for _, raw := range raws {
		var ev *types.Event
		if err := json.Unmarshal(raw, &ev); err != nil || ev == nil {
			log.Warn().Err(err).Msg("Dropping PDU that cannot be parsed")
			continue
		}
		pdus = append(pdus, ev)
	}
	return pdus
}

type pduRoomLookups interface {
	GetRoom(ctx context.Context, roomID id.RoomID) (*types.Room, error)
	GetCurrentMembershipAndEvent(ctx context.Context, userID id.UserID, roomID id.RoomID) (*types.MembershipTup, *types.Event, error)
}

// pduRoom is the room of a transaction's PDUs: the version they are verified with, empty for a room
// unknown here, and whether this server is joined to it
type pduRoom struct {
	version string
	joined  bool
}

// pduRooms looks up the room of each PDU of a transaction once
type pduRooms struct {
	ctx         context.Context
	db          pduRoomLookups
	serverName  string
	createRooms bool
	rooms       map[id.RoomID]pduRoom
}

func newPDURooms(ctx context.Context, db pduRoomLookups, serverName string, createRooms bool) *pduRooms {
	return &pduRooms{
		ctx:         ctx,
		db:          db,
		serverName:  serverName,
		createRooms: createRooms,
		rooms:       make(map[id.RoomID]pduRoom, 1),
	}
}

// roomVersion returns the version of the PDU's room. A room without a record takes the version of
// its create event, or for a leave of a local user, the version of the invite the leave answers.
func (p *pduRooms) roomVersion(ev *types.Event) (string, error) {
	room, found := p.rooms[ev.RoomID]
	if !found {
		stored, err := p.db.GetRoom(p.ctx, ev.RoomID)
		if err != nil {
			return "", err
		} else if stored != nil {
			room = pduRoom{version: stored.Version, joined: stored.LocalMembers > 0}
		}
		p.rooms[ev.RoomID] = room
	}
	if room.version != "" {
		return room.version, nil
	} else if ev.Type == event.StateCreate && p.createRooms {
		// If no room, and this is a create event, and we're allowed to receive create
		// events over federation - pull the room version from the event content.
		room.version = gjson.GetBytes(ev.Content, "room_version").String()
		p.rooms[ev.RoomID] = room
		return room.version, nil
	}
	return p.invitedVersion(ev)
}

func (p *pduRooms) invitedVersion(ev *types.Event) (string, error) {
	if ev.Type != event.StateMember || ev.StateKey == nil || ev.Membership() != event.MembershipLeave {
		return "", nil
	}
	targetUserID := id.UserID(*ev.StateKey)
	if targetUserID.Homeserver() != p.serverName {
		return "", nil
	}
	membershipTup, membershipEv, err := p.db.GetCurrentMembershipAndEvent(p.ctx, targetUserID, ev.RoomID)
	if err != nil || membershipTup == nil || membershipTup.Membership != event.MembershipInvite {
		return "", err
	}
	return membershipEv.RoomVersion, nil
}

func (f *FederationRoutes) processTransactionPDUs(r *http.Request, origin string, pdus []*types.Event) (*respTransaction, error) {
	resp := respTransaction{make(map[id.EventID]respTransactionResult, len(pdus))}
	txnRooms := newPDURooms(r.Context(), f.db.Rooms, f.config.ServerName, f.config.SecretSwitches.EnableFederatedSendRoomCreate)

	// Run some pre-checks before we send the events to the database layer
	roomToEvs := make(map[id.RoomID][]*types.Event, 5)
	for _, ev := range pdus {
		roomVersion, err := txnRooms.roomVersion(ev)
		if err != nil {
			return nil, err
		} else if roomVersion == "" {
			// If we have no room version we can't calculate the reference hash,
			// so we *silently* drop it (synapse + dendrite do this, spec unclear).
			hlog.FromRequest(r).Warn().
				Stringer("room_id", ev.RoomID).
				Stringer("type", ev.Type).
				Msg("Silently dropping event from unknown room")
			continue
		}
		ev.RoomVersion = roomVersion

		verifiedEv, verifyErr, err := util.VerifyRemoteEvent(r.Context(), ev, f.keyStore)
		if err != nil {
			return nil, err
		} else if verifyErr != nil {
			// https://spec.matrix.org/v1.14/server-server-api/#rejection
			// "If an event in an incoming transaction is rejected, this should not cause the transaction request to be responded to with an error response."
			resp.PDUs[ev.ID] = respTransactionResult{}
			hlog.FromRequest(r).Err(verifyErr).
				Stringer("room_id", ev.RoomID).
				Stringer("event_id", ev.ID).
				Msg("Federated event failed verification")
			continue
		}

		if verifiedEv.Redacted {
			hlog.FromRequest(r).Warn().
				Stringer("room_id", ev.RoomID).
				Stringer("event_id", ev.ID).
				Msg("Processing redacted event over federation")
		}
		roomToEvs[verifiedEv.RoomID] = append(roomToEvs[verifiedEv.RoomID], verifiedEv)
	}

	var wg sync.WaitGroup
	resultsCh := make(chan *rooms.SendEventsResult, len(roomToEvs))

	for roomID, evs := range roomToEvs {
		room := txnRooms.rooms[roomID]
		wg.Go(func() {
			log := hlog.FromRequest(r).With().Stringer("room_id", roomID).Logger()
			// A panic leaves the room's events out of the response, as an error does
			defer util.RecoverPanic(&log, nil)
			var prevStates *rooms.GivenStates
			// Events of a room this server is not in are only kept as outlier memberships, which
			// need no missing events
			if room.joined {
				// Now we have all the events we're going to try to send, fetch any missing
				// prev or auth events so we can send those as well (or we'll reject).
				pulled, err := f.getMissingEventsForSendBatch(r.Context(), origin, roomID, room.version, evs)
				if err != nil {
					log.Err(err).Msg("Get missing events failed")
					return
				}
				// Pulled events are older so go first, the send keeps the order of unrelated events
				evs = slices.Concat(pulled, evs)
				prevStates = f.getPrevEventStatesForSendBatch(r.Context(), origin, roomID, room.version, evs, pulled)
			}
			results, err := f.db.SendFederatedEvents(r.Context(), roomID, evs, prevStates)
			if err != nil {
				// This is *BAD*, an unexpected error handling results for a room, we can't bail the
				// request here as we'll poison other parallel room sends. So we just log and none
				// of the events in question will be in the response.
				// Does this make other servers angry? Will they retry those events?
				log.Err(err).Msg("Sending federated events failed")
				return
			}
			resultsCh <- results
		})
	}

	wg.Wait()
	close(resultsCh)

	for results := range resultsCh {
		for _, allowed := range results.Allowed {
			resp.PDUs[allowed.ID] = respTransactionResult{}
		}
		for _, rejected := range results.Rejected {
			result := respTransactionResult{}
			if errors.Is(rejected.Error, rooms.ErrEventDropped) {
				// Not stored at all, unlike rejected events
				result.Error = rejected.Error.Error()
				hlog.FromRequest(r).Warn().
					Err(rejected.Error).
					Stringer("room_id", rejected.Event.RoomID).
					Stringer("event_id", rejected.Event.ID).
					Msg("Dropped federated event from batch")
			}
			resp.PDUs[rejected.Event.ID] = result
		}
	}

	return &resp, nil
}

const (
	// Prev events of a room's batch whose state is fetched from the sending server. Events citing
	// the others are dropped, and the state fetched again with the next event citing them.
	maxPrevEventStatesFetched = 10
	// Events of a prev event's state fetched concurrently when too few are missing to fetch it whole
	prevEventStatesFetchConcurrency = 5
	// Both fetches fit in the minute a sending server typically waits for its transaction
	getMissingEventsTimeout     = 20 * time.Second
	prevEventStatesFetchTimeout = 30 * time.Second
)

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

// Pulls the events missing between the room's extremities and the batch events citing prev events
// that are missing here, returning those pulled
func (f *FederationRoutes) getMissingEventsForSendBatch(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	roomVersion string,
	evs []*types.Event,
) ([]*types.Event, error) {
	missing, err := f.db.Rooms.GetMissingEventsRequest(ctx, roomID, evs)
	if err != nil {
		return nil, err
	} else if len(missing.Missing) == 0 {
		return nil, nil
	}
	var latestIDs []id.EventID
	for _, ev := range evs {
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
		ServerName:     origin,
		RoomID:         roomID,
		EarliestEvents: missing.Extremities,
		LatestEvents:   latestIDs,
		Limit:          f.config.Federation.MaxFetchMissingEvents,
		MinDepth:       int(missing.MinDepth),
	})
	if err != nil {
		return nil, err
	}

	seenIDs := make(map[id.EventID]struct{}, len(evs)+len(resp.Events))
	for _, ev := range evs {
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

func (f *FederationRoutes) getPrevEventStatesForSendBatch(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	roomVersion string,
	evs, pulled []*types.Event,
) *rooms.GivenStates {
	log := zerolog.Ctx(ctx).With().
		Stringer("room_id", roomID).
		Str("origin", origin).
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
		stateIDs, fetched, err := f.fetchPrevEventState(ctx, origin, roomID, roomVersion, prevID, known)
		if err != nil {
			log.Warn().Err(err).Stringer("prev_event_id", prevID).Msg("Failed to fetch the state at prev event")
			continue
		}
		prevStates.BeforePrevs[prevID] = stateIDs
		prevStates.Events = append(prevStates.Events, fetched...)
		for _, ev := range fetched {
			known[ev.ID] = struct{}{}
		}
	}
	return prevStates
}

func (f *FederationRoutes) fetchPrevEventState(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	roomVersion string,
	prevID id.EventID,
	known map[id.EventID]struct{},
) ([]id.EventID, []*types.Event, error) {
	stateIDs, err := f.fedclient.GetStateIDs(ctx, origin, roomID, prevID)
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
		resp, err := f.fedclient.GetState(ctx, origin, roomID, prevID)
		if err != nil {
			return nil, nil, err
		} else if resp == nil {
			return nil, nil, fmt.Errorf("null state response for %s", prevID)
		}
		pdus = slices.Concat(resp.PDUs, resp.AuthChain)
		if _, found := unfetched[prevID]; found {
			pdus = append(pdus, f.fetchEvents(ctx, origin, []id.EventID{prevID})...)
		}
	} else {
		pdus = f.fetchEvents(ctx, origin, missing)
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
func (f *FederationRoutes) fetchEvents(ctx context.Context, origin string, eventIDs []id.EventID) []federation.PDU {
	pdus := make([][]federation.PDU, len(eventIDs))
	limit := make(chan struct{}, prevEventStatesFetchConcurrency)
	var wg sync.WaitGroup
	for i, eventID := range eventIDs {
		limit <- struct{}{}
		wg.Go(func() {
			defer func() { <-limit }()
			defer util.RecoverPanic(zerolog.Ctx(ctx), nil)
			resp, err := f.fedclient.GetEvent(ctx, origin, eventID)
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
