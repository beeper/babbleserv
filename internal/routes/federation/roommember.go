package federation

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/beeper/libserv/pkg/requestlog"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// invite_room_state is unread, so left unparsed: a stripped create event is no PDU to check
type inviteRequest struct {
	Event       *types.Event      `json:"event"`
	InviteState []json.RawMessage `json:"invite_room_state"`
	RoomVersion string            `json:"room_version"`
}

// https://spec.matrix.org/v1.10/server-server-api/#put_matrixfederationv2inviteroomideventid
func (f *FederationRoutes) SignInvite(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	if roomID == "" {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	} else if req.Event == nil || req.Event.Type != event.StateMember || req.Event.StateKey == nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Event is not a member event")
		return
	} else if req.Event.Membership() != event.MembershipInvite {
		// Stored as an outlier, unauthorized, when this server is not in the room
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Membership is not invite")
		return
	} else if id.UserID(*req.Event.StateKey).Homeserver() != f.config.ServerName {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "The invite event must be for this server")
		return
	} else if req.Event.RoomID != roomID {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Event room ID does not match the request")
		return
	} else if !f.config.JoinsRoomVersion(req.RoomVersion) {
		responseIncompatibleRoomVersion(w, r, req.RoomVersion, "Room version not supported")
		return
	}

	req.Event.RoomVersion = req.RoomVersion
	req.Event.ID = util.EventIDFromRequestURLParam(r, "eventID")

	// Verify the event ID and signature
	verifyErr, err := util.VerifyEventFromServer(r.Context(), req.Event, middleware.GetRequestServer(r), f.keyStore)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if verifyErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
		return
	}

	keyID, key := f.config.MustGetActiveSigningKey()
	signature, err := util.GetEventSignature(req.Event, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	req.Event.Signatures[f.config.ServerName] = map[string]string{
		keyID: signature,
	}

	// For a room this server is not in, the invite is stored as an outlier, so the target local user
	// sees it in sync without it forming part of the room. Otherwise it is sent as any federated event,
	// and received a second time over federation as a duplicate.
	if res, err := f.db.SendFederatedEvents(r.Context(), roomID, []*types.Event{req.Event}, nil, rooms.SendFederatedEventsOptions{}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(res.Rejected) > 0 {
		util.ResponseRejectedEventJSON(w, r, res.Rejected[0].Error)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Event *types.Event `json:"event"`
	}{req.Event})
}

// https://spec.matrix.org/v1.14/server-server-api/#get_matrixfederationv1make_leaveroomiduserid
func (f *FederationRoutes) MakeLeave(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipLeave, false)
}

// https://spec.matrix.org/v1.14/server-server-api/#put_matrixfederationv2send_leaveroomideventid
func (f *FederationRoutes) SendLeave(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipLeave, membershipResponse{})
}

// https://spec.matrix.org/v1.11/server-server-api/#get_matrixfederationv1make_joinroomiduserid
func (f *FederationRoutes) MakeJoin(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipJoin, true)
}

// https://spec.matrix.org/v1.11/server-server-api/#put_matrixfederationv2send_joinroomideventid
func (f *FederationRoutes) SendJoin(w http.ResponseWriter, r *http.Request) {
	release := func() {}
	defer func() { release() }()
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipJoin, membershipResponse{
		// Refuse a state DAG too large to serve before the join is sent, rather than leave the joiner
		// joined here alone; a join stored already is answered again as the first was. An origin at
		// its limit of served state DAGs waits here, holding no transaction.
		beforeSend: func(room *types.Room, ev *types.Event) error {
			if !util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) {
				return nil
			}
			acquired, err := f.stateDAGJoins.acquire(r.Context(), middleware.GetRequestServer(r))
			if err != nil {
				return err
			}
			release = acquired
			if stored, err := f.db.Rooms.GetStoredEventIDs(r.Context(), []id.EventID{ev.ID}); err != nil || len(stored) > 0 {
				return err
			}
			return f.db.Rooms.CheckServedStateDAG(r.Context(), room.ID)
		},
		afterSend: func(room *types.Room, ev *types.Event) (any, error) {
			if util.RoomVersionHas(room.Version, gomatrixserverlib.IRoomVersion.StateDAGs) {
				return stateDAGJoinResponse{f: f, room: room, ev: ev}, nil
			}
			// The state before the join. A join stored before without state, as a prev event's,
			// takes the room's current state.
			stateWithAuthChain, err := f.db.Rooms.GetRoomStateWithAuthChainAtEvent(r.Context(), room.ID, ev.ID)
			if errors.Is(err, types.ErrStateUnavailable) {
				stateWithAuthChain, err = f.db.Rooms.GetCurrentRoomStateWithAuthChain(r.Context(), room.ID)
			}
			if err != nil {
				return nil, err
			}
			return struct {
				AuthChain []*types.Event `json:"auth_chain"`
				State     []*types.Event `json:"state"`
			}{stateWithAuthChain.AuthChain, stateWithAuthChain.StateEvents}, nil
		},
	})
}

// stateDAGJoinResponse is MSC4242's send_join response, {origin, event, state_dag}, streamed a page a
// transaction as a large room's state DAG is far more than one transaction reads.
type stateDAGJoinResponse struct {
	f    *FederationRoutes
	room *types.Room
	ev   *types.Event
}

// write leaves the body unfinished should reading fail part way or the joiner stop reading for
// stateDAGWriteTimeout, so the joiner sees a failed request and sends the join again
func (resp stateDAGJoinResponse) write(w http.ResponseWriter, r *http.Request) {
	log := hlog.FromRequest(r)
	head, err := json.Marshal(struct {
		Origin string       `json:"origin"`
		Event  *types.Event `json:"event"`
	}{resp.f.config.ServerName, resp.ev})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	conn := http.NewResponseController(connWriter(w))
	var out io.Writer = w
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		log.Warn().Err(err).Msg("Serving the state DAG of a join without write deadlines")
	} else {
		out = deadlineWriter{w, conn}
		defer conn.SetWriteDeadline(time.Time{})
	}
	body := bufio.NewWriterSize(out, stateDAGWriteBuffer)
	body.Write(head[:len(head)-1])
	body.WriteString(`,"state_dag":[`)
	served := 0
	err = resp.f.db.Rooms.StreamServedStateDAG(r.Context(), resp.room.ID, resp.ev.ID, func(evs []*types.Event) error {
		for _, ev := range evs {
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			} else if served > 0 {
				body.WriteByte(',')
			}
			body.Write(b)
			served++
		}
		return body.Flush()
	})
	if err != nil {
		log.Err(err).Int("served", served).Msg("Failed to serve the state DAG of a join")
		return
	}
	body.WriteString("]}")
	if err := body.Flush(); err != nil {
		log.Warn().Err(err).Msg("Failed to finish serving the state DAG of a join")
	}
	log.Debug().Int("state_dag", served).Msg("Served the state DAG of a join")
}

// https://spec.matrix.org/v1.14/server-server-api/#get_matrixfederationv1make_knockroomiduserid
func (f *FederationRoutes) MakeKnock(w http.ResponseWriter, r *http.Request) {
	f.makeMembershipEventForOtherServer(w, r, event.MembershipKnock, true)
}

// https://spec.matrix.org/v1.14/server-server-api/#put_matrixfederationv1send_knockroomideventid
func (f *FederationRoutes) SendKnock(w http.ResponseWriter, r *http.Request) {
	f.sendMembershipEventFromOtherServer(w, r, event.MembershipKnock, membershipResponse{
		afterSend: func(room *types.Room, _ *types.Event) (any, error) {
			strippedState, err := f.db.Rooms.RoomStrippedState(r.Context(), room.ID)
			if err != nil {
				return nil, err
			}
			return struct {
				KnockState []*types.PartialEvent `json:"knock_room_state"`
			}{strippedState}, nil
		},
	})
}

var errTooManyStateDAGJoins = errors.New("the server has as many state DAGs of joins served and waiting as it may")

const (
	stateDAGWriteTimeout = 30 * time.Second
	stateDAGWriteBuffer  = 64 << 10
)

type deadlineWriter struct {
	w    http.ResponseWriter
	conn *http.ResponseController
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	if err := d.conn.SetWriteDeadline(time.Now().Add(stateDAGWriteTimeout)); err != nil {
		return 0, err
	}
	return d.w.Write(p)
}

// connWriter unwraps the access log's writer, which hides the connection from http.ResponseController
func connWriter(w http.ResponseWriter) http.ResponseWriter {
	if counting, ok := w.(*requestlog.CountingResponseWriter); ok {
		return counting.ResponseWriter
	}
	return w
}

// originLimiter bounds each origin's concurrent requests like Synapse's rc_federation, queueing a
// bounded number more
type originLimiter struct {
	limit, queue int
	mu           sync.Mutex
	origins      map[string]*originTurns
}

type originTurns struct {
	turns   chan struct{}
	waiting int
}

func newOriginLimiter(limit, queue int) *originLimiter {
	return &originLimiter{limit: limit, queue: queue, origins: make(map[string]*originTurns)}
}

func (l *originLimiter) acquire(ctx context.Context, origin string) (func(), error) {
	l.mu.Lock()
	o := l.origins[origin]
	if o == nil {
		o = &originTurns{turns: make(chan struct{}, l.limit)}
		l.origins[origin] = o
	}
	select {
	case o.turns <- struct{}{}:
		l.mu.Unlock()
		return l.release(origin, o), nil
	default:
	}
	if o.waiting >= l.queue {
		l.mu.Unlock()
		return nil, errTooManyStateDAGJoins
	}
	o.waiting++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		o.waiting--
		l.forgetIdle(origin, o)
	}()
	select {
	case o.turns <- struct{}{}:
		return l.release(origin, o), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *originLimiter) release(origin string, o *originTurns) func() {
	return sync.OnceFunc(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		<-o.turns
		l.forgetIdle(origin, o)
	})
}

// forgetIdle runs under the lock
func (l *originLimiter) forgetIdle(origin string, o *originTurns) {
	if len(o.turns) == 0 && o.waiting == 0 {
		delete(l.origins, origin)
	}
}
