package federation

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
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
	GetIngestJob(ctx context.Context, roomID id.RoomID) (*types.IngestJob, error)
	GetCurrentMembershipAndEvent(ctx context.Context, userID id.UserID, roomID id.RoomID) (*types.MembershipTup, *types.Event, error)
}

// pduRoom is the room of a transaction's PDUs: the version they are verified with, empty for a room
// unknown here, whether this server is joined to it, and whether it has an ingest job, which a join
// to a room without a record yet has
type pduRoom struct {
	version   string
	joined    bool
	ingesting bool
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
		job, err := p.db.GetIngestJob(p.ctx, ev.RoomID)
		if err != nil {
			return "", err
		} else if job != nil {
			room.ingesting = true
			room.version = cmp.Or(room.version, job.RoomVersion)
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
			ctx := log.WithContext(r.Context())
			// A panic leaves the room's events out of the response, as an error does
			defer util.RecoverPanic(&log, nil)
			var results *rooms.SendEventsResult
			var err error
			if !room.ingesting {
				results, err = f.sendRoomPDUs(ctx, origin, roomID, room, evs)
			}
			if room.ingesting || errors.Is(err, rooms.ErrRoomIngesting) {
				results, err = f.enqueueRoomPDUs(ctx, origin, roomID, evs)
			}
			if err != nil {
				// This is *BAD*, an unexpected error handling results for a room, we can't bail the
				// request here as we'll poison other parallel room sends. So we just log and none
				// of the events in question will be in the response.
				// Does this make other servers angry? Will they retry those events?
				log.Err(err).Msg("Sending federated events failed")
				return
			} else if results != nil {
				resultsCh <- results
			}
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

// sendRoomPDUs sends a room's PDUs, unless it has an ingest job by the time they publish. In a room
// this server is joined to, the events missing between the room's extremities and the PDUs are
// pulled from the origin first.
func (f *FederationRoutes) sendRoomPDUs(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	room pduRoom,
	evs []*types.Event,
) (*rooms.SendEventsResult, error) {
	var prevStates *rooms.GivenStates
	// Events of a room this server is not in are only kept as outlier memberships, which need no
	// missing events
	if room.joined {
		fetcher := f.ingestJobs.Fetcher()
		pulled, err := fetcher.MissingEvents(ctx, origin, roomID, room.version, evs)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Get missing events failed")
			return nil, nil
		}
		// Pulled events are older so go first, the send keeps the order of unrelated events
		evs = slices.Concat(pulled, evs)
		prevStates = fetcher.PrevEventStates(ctx, origin, roomID, room.version, evs, pulled)
	}
	return f.db.SendFederatedEvents(ctx, roomID, evs, prevStates, rooms.SendFederatedEventsOptions{
		RefuseWhileIngesting: true,
	})
}

// enqueueRoomPDUs writes a room's PDUs as targets of its ingest job, which publishes them once it has
// what they need, and answers them as received
func (f *FederationRoutes) enqueueRoomPDUs(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	evs []*types.Event,
) (*rooms.SendEventsResult, error) {
	if err := f.db.Rooms.EnqueueIngestPDUs(ctx, roomID, origin, evs); err != nil {
		return nil, err
	}
	f.ingestJobs.Run(roomID)
	zerolog.Ctx(ctx).Debug().Int("events", len(evs)).Msg("Enqueued PDUs for the room's ingest job")
	return &rooms.SendEventsResult{Allowed: evs}, nil
}
