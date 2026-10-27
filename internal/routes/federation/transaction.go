package federation

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
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

// The spec's limits, past which Synapse refuses a transaction
const (
	maxTransactionPDUs = 50
	maxTransactionEDUs = 100
)

func (f *FederationRoutes) SendTransaction(w http.ResponseWriter, r *http.Request) {
	arrived := time.Now()
	var req reqTransaction
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	txnID := chi.URLParam(r, "txnID")
	if txnID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	} else if len(req.PDUs) > maxTransactionPDUs || len(req.EDUs) > maxTransactionEDUs {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, fmt.Sprintf(
			"A transaction carries at most %d PDUs and %d EDUs", maxTransactionPDUs, maxTransactionEDUs,
		))
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

	// Answer within the minute the sender waits, so it doesn't resend the transaction meanwhile. The
	// PDUs stop waiting on a queue a fifth early, so that EDUs held for them still have time.
	queueWait := f.config.Rooms.Ingest.QueueWait
	deadline := arrived.Add(queueWait)
	var resp *respTransaction
	pduErr, eduErr := runTransaction(&log, func(admitted *admission) (err error) {
		resp, err = f.processTransactionPDUs(r, deadline.Add(-queueWait/5), req.Origin, parseTransactionPDUs(&log, req.PDUs), admitted)
		return err
	}, func() error {
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		return f.processTransactionEDUs(ctx, r, req.Origin, req.EDUs)
	})

	if errors.Is(pduErr, errQueueFull) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MLimitExceeded, "Too many events of a room are waiting to be processed")
		return
	} else if pduErr != nil {
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

// runTransaction holds the EDUs until the PDUs succeed, or cannot wait on an ingest queue: a failed
// transaction is resent whole, so its EDUs would be processed twice
func runTransaction(log *zerolog.Logger, pdus func(*admission) error, edus func() error) (pduErr, eduErr error) {
	admitted := newAdmission()
	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() { admitted.resolve(pduErr == nil) }()
		defer util.RecoverPanic(log, &pduErr)
		pduErr = pdus(admitted)
	})
	wg.Go(func() {
		defer util.RecoverPanic(log, &eduErr)
		if admitted.wait() {
			eduErr = edus()
		}
	})
	wg.Wait()
	return pduErr, eduErr
}

const remoteDeviceIngestConcurrency = 4

// processTransactionEDUs fails for to-device messages it failed to store, and for device list
// updates the deadline left unstored, which are lost unless the transaction is resent: presence is
// ephemeral
func (f *FederationRoutes) processTransactionEDUs(ctx context.Context, r *http.Request, origin string, edus []*types.EDU) error {
	log := hlog.FromRequest(r)

	edusByType := make(map[types.EDUType][]*types.EDU, 3)
	for _, edu := range edus {
		if edu != nil {
			edusByType[edu.Type] = append(edusByType[edu.Type], edu)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, len(edusByType))
	i := 0
	for eduType, edus := range edusByType {
		errp := &errs[i]
		i++
		wg.Go(func() {
			defer util.RecoverPanic(log, nil)

			switch eduType {
			case types.EDUTypeDeviceListUpdate, types.EDUTypeSigningKeyUpdate:
				*errp = f.processTransactionDeviceListEDUs(ctx, r, origin, eduType, edus)

			case types.EDUTypeReceipt:
				// TODO

			case types.EDUTypePresence:
				f.processTransactionPresenceEDUs(ctx, r, origin, eduType, edus)

			case types.EDUTypeToDevice:
				*errp = f.processTransactionToDeviceEDUs(ctx, r, origin, eduType, edus)

			default:
				log.Error().Str("type", string(eduType)).Msg("Ignoring unknown EDU type")
			}
		})
	}

	wg.Wait()
	return errors.Join(errs...)
}

// processTransactionDeviceListEDUs fails only on the context ending: the sender would never resend
// the updates, whereas an update failing otherwise would fail every resend
func (f *FederationRoutes) processTransactionDeviceListEDUs(ctx context.Context, r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) error {
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
	var unstored atomic.Int32
	running := make(chan struct{}, remoteDeviceIngestConcurrency)
	for userID, updates := range userEDUs {
		running <- struct{}{}
		ingests.Go(func() {
			defer func() { <-running }()
			log := log.With().Stringer("user_id", userID).Logger()
			defer util.RecoverPanic(&log, nil)

			notified, err := f.db.Accounts.IngestRemoteDeviceUpdates(ctx, userID, updates)
			if err != nil {
				log.Err(err).Msg("Failed to ingest remote device updates")
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					unstored.Add(1)
				}
				return
			}
			log.Debug().Int("updates", len(updates)).Bool("notified", notified).Msg("Ingested remote device updates")
		})
	}
	ingests.Wait()
	if n := unstored.Load(); n > 0 {
		return fmt.Errorf("%s updates of %d users were not stored in time: %w", eduType, n, ctx.Err())
	}
	return nil
}

func (f *FederationRoutes) processTransactionPresenceEDUs(ctx context.Context, r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) {
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

			if err := f.db.Transient.UpdateUserPresence(ctx, presenceItem.UserID, presence); err != nil {
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

func (f *FederationRoutes) processTransactionToDeviceEDUs(ctx context.Context, r *http.Request, origin string, eduType types.EDUType, edus []*types.EDU) error {
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
	if _, err := f.db.SendToDeviceEvents(ctx, tds, transient.SendToDeviceOptions{}); err != nil {
		log.Err(err).Msg("Failed to store remote to-device messages")
		return err
	}
	return nil
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
// its create event, or for a leave of a local user, the version of the invite it ends.
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
	return p.heldMembershipVersion(ev)
}

func (p *pduRooms) anyIngesting() bool {
	for _, room := range p.rooms {
		if room.ingesting {
			return true
		}
	}
	return false
}

func (p *pduRooms) heldMembershipVersion(ev *types.Event) (string, error) {
	if ev.Type != event.StateMember || ev.StateKey == nil || ev.Membership() != event.MembershipLeave {
		return "", nil
	}
	targetUserID := id.UserID(*ev.StateKey)
	if targetUserID.Homeserver() != p.serverName {
		return "", nil
	}
	membershipTup, membershipEv, err := p.db.GetCurrentMembershipAndEvent(p.ctx, targetUserID, ev.RoomID)
	if err != nil || membershipTup == nil {
		return "", err
	} else if membershipTup.Membership != event.MembershipInvite {
		return "", nil
	}
	return membershipEv.RoomVersion, nil
}

func (f *FederationRoutes) processTransactionPDUs(
	r *http.Request,
	deadline time.Time,
	origin string,
	pdus []*types.Event,
	admitted *admission,
) (*respTransaction, error) {
	resp := respTransaction{make(map[id.EventID]respTransactionResult, len(pdus))}
	txnRooms := newPDURooms(r.Context(), f.db.Rooms, f.config.ServerName, f.config.SecretSwitches.EnableFederatedSendRoomCreate)

	versions := make([]string, len(pdus))
	for i, ev := range pdus {
		var err error
		if versions[i], err = txnRooms.roomVersion(ev); err != nil {
			return nil, err
		}
	}
	if !txnRooms.anyIngesting() {
		admitted.resolve(true)
	}

	// Run some pre-checks before we send the events to the database layer
	roomToEvs := make(map[id.RoomID][]*types.Event, 5)
	for i, ev := range pdus {
		if versions[i] == "" {
			// If we have no room version we can't calculate the reference hash,
			// so we *silently* drop it (synapse + dendrite do this, spec unclear).
			hlog.FromRequest(r).Warn().
				Stringer("room_id", ev.RoomID).
				Stringer("type", ev.Type).
				Msg("Silently dropping event from unknown room")
			continue
		}
		ev.RoomVersion = versions[i]

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

	waits := f.waitForQueues(r, txnRooms, roomToEvs, deadline)

	var wg sync.WaitGroup
	resultsCh := make(chan *rooms.SendEventsResult, len(roomToEvs))
	errsCh := make(chan error, len(roomToEvs))

	for roomID, evs := range roomToEvs {
		room := txnRooms.rooms[roomID]
		wg.Go(func() {
			log := hlog.FromRequest(r).With().Stringer("room_id", roomID).Logger()
			ctx := log.WithContext(r.Context())
			// A panic leaves the room's events out of the response rather than failing every resend
			defer util.RecoverPanic(&log, nil)
			results, err := f.processRoomPDUs(ctx, origin, roomID, room, waits[roomID], evs)
			if err != nil {
				log.Err(err).Msg("Failed to enqueue federated events for the room's ingest job")
				errsCh <- fmt.Errorf("%s: %w", roomID, err)
			} else if results != nil {
				resultsCh <- results
			}
		})
	}

	wg.Wait()
	close(resultsCh)
	close(errsCh)
	for _, wait := range waits {
		<-wait.done
		if wait.err != nil {
			return nil, wait.err
		}
	}
	var errs []error
	for err := range errsCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

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

// processRoomPDUs publishes the PDUs connected here and enqueues the rest after, as enqueueing first
// would start the job the publish defers to. A room with a job enqueues them all, as does a failed
// split or publish, so only an enqueue error fails the transaction.
func (f *FederationRoutes) processRoomPDUs(
	ctx context.Context,
	origin string,
	roomID id.RoomID,
	room pduRoom,
	wait *queueWait,
	evs []*types.Event,
) (*rooms.SendEventsResult, error) {
	log := zerolog.Ctx(ctx)
	if wait != nil {
		// A wait failing fails the whole transaction
		<-wait.done
		if wait.err != nil {
			return nil, nil
		}
		return f.enqueueRoomPDUs(ctx, origin, roomID, evs)
	}
	connected, rest := evs, []*types.Event(nil)
	// A room this server is not in only keeps outlier memberships, which need nothing they cite
	if room.joined {
		var err error
		if connected, rest, err = f.ingestJobs.SplitConnected(ctx, evs, nil); err != nil {
			log.Warn().Err(err).Msg("Failed to split federated events, enqueueing them all")
			return f.enqueueRoomPDUs(ctx, origin, roomID, evs)
		}
	}
	results := &rooms.SendEventsResult{}
	if len(connected) > 0 {
		var err error
		results, err = f.db.SendFederatedEvents(ctx, roomID, connected, nil, rooms.SendFederatedEventsOptions{
			RefuseWhileIngesting: true,
		})
		if err != nil {
			if !errors.Is(err, rooms.ErrRoomIngesting) {
				log.Warn().Err(err).Msg("Sending federated events failed, enqueueing them")
			}
			return f.enqueueRoomPDUs(ctx, origin, roomID, evs)
		}
	}
	if len(rest) == 0 {
		return results, nil
	}
	queued, err := f.enqueueRoomPDUs(ctx, origin, roomID, rest)
	if err != nil {
		return nil, err
	}
	results.Allowed = append(results.Allowed, queued.Allowed...)
	return results, nil
}

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

var errQueueFull = errors.New("the room's ingest queue stayed full")

const queuePollInterval = time.Second

type admission struct {
	once sync.Once
	done chan struct{}
	ok   bool
}

func newAdmission() *admission {
	return &admission{done: make(chan struct{})}
}

func (a *admission) resolve(ok bool) {
	a.once.Do(func() {
		a.ok = ok
		close(a.done)
	})
}

func (a *admission) wait() bool {
	<-a.done
	return a.ok
}

type queueWait struct {
	done chan struct{}
	err  error
}

func (f *FederationRoutes) waitForQueues(
	r *http.Request,
	txnRooms *pduRooms,
	roomToEvs map[id.RoomID][]*types.Event,
	deadline time.Time,
) map[id.RoomID]*queueWait {
	waits := make(map[id.RoomID]*queueWait)
	for roomID := range roomToEvs {
		if !txnRooms.rooms[roomID].ingesting {
			continue
		}
		wait := &queueWait{done: make(chan struct{})}
		waits[roomID] = wait
		go func() {
			defer close(wait.done)
			log := hlog.FromRequest(r).With().Stringer("room_id", roomID).Logger()
			defer util.RecoverPanic(&log, &wait.err)
			wait.err = f.waitForQueue(log.WithContext(r.Context()), roomID, deadline)
		}()
	}
	return waits
}

func (f *FederationRoutes) waitForQueue(ctx context.Context, roomID id.RoomID, deadline time.Time) error {
	cfg := f.config.Rooms.Ingest
	for waited := false; ; waited = true {
		queue, err := f.db.Rooms.GetIngestQueue(ctx, roomID)
		if err != nil {
			return err
		} else if !queue.Full(cfg.MaxQueuedPDUs, cfg.MaxQueuedPDUBytes) {
			if waited {
				zerolog.Ctx(ctx).Info().Msg("Room's ingest queue has room again")
			}
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("%w: %s", errQueueFull, roomID)
		} else if !waited {
			zerolog.Ctx(ctx).Warn().
				Int64("pdus", queue.PDUs).
				Int64("bytes", queue.Bytes).
				Msg("Room's ingest queue is full, waiting for it to drain")
		}
		select {
		case <-time.After(min(queuePollInterval, left)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
