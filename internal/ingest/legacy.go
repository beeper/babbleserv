package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	firstSendJoinRetryPause   = 2 * time.Second
	longestSendJoinRetryPause = 30 * time.Second
)

// joinWork is a local user's join to a room through a resident server, run on the remote join path
// in phases B and C: build the state before the join, then publish it, as a member's join should
// another have got this server into the room first. The room version's strategy fetches what the
// join needs in phase A.
type joinWork struct {
	run    *jobRun
	target *types.IngestTarget
	join   *databases.RemoteJoin
	// A server answered a send_join of the join, so accepted it, as a state DAG resident serving no
	// state DAG that connects it does
	answered bool
	// When the join's first send_join was sent, which its JoinTimeout counts from
	firstSent time.Time
}

// A refused join is replaced with a fresh one at most once, counted from the user's first join
const maxJoinReplacements = 1

func (w *joinWork) targets() []*types.IngestTarget {
	return []*types.IngestTarget{w.target}
}

func (w *joinWork) kind() workKind {
	return workJoin
}

// timesOut also bounds how long the join's response is read for, not just how long it is sent for
func (w *joinWork) timesOut() time.Time {
	return w.firstSent.Add(w.run.jobs.config.Rooms.Ingest.JoinTimeout)
}

func (w *joinWork) build(ctx context.Context) error {
	return w.refused(ctx, w.join.BuildState(ctx))
}

func (w *joinWork) publish(ctx context.Context) error {
	res, err := w.join.Publish(ctx)
	if errors.Is(err, rooms.ErrIngestTargetGone) {
		return w.gone(ctx)
	} else if err := w.refused(ctx, err); err != nil {
		return err
	} else if len(res.Rejected) > 0 {
		// A stored duplicate, or the join sent as a member's
		return w.run.fail(ctx, w.target, rejectedFailure(res.Rejected[0].Error))
	}
	// Published, stored before, or the user joined with a new local event instead
	if err := w.run.jobs.db.Rooms.ClearIngestTargets(ctx, w.run.roomID, []id.EventID{w.target.Event.ID}); err != nil {
		return err
	}
	w.run.untarget(w.target.Event.ID)
	zerolog.Ctx(ctx).Info().Stringer("event_id", w.target.Event.ID).Msg("Published remote join")
	w.run.joinDone(ctx, w.target, nil)
	return nil
}

// refused fails the target on an error refusing the join, passing any other on
func (w *joinWork) refused(ctx context.Context, err error) error {
	if errors.Is(err, rooms.ErrAuthStage4) || errors.Is(err, rooms.ErrAuthStage5) {
		return w.run.fail(ctx, w.target, rejectedFailure(err))
	}
	return err
}

// errResendRefused is a server refusing a join an earlier send_join may have landed with
var errResendRefused = errors.New("a server refused a join that may have landed")

// mayLand records that the send_join about to be sent may land, and when the first was sent,
// returning the target as it was, or errWorkDone once the target is gone
func (w *joinWork) mayLand(ctx context.Context) (*types.IngestTarget, error) {
	now := time.Now().UnixMilli()
	before, err := w.run.jobs.db.Rooms.UpdateIngestTarget(ctx, w.run.roomID, w.target.Event.ID, func(target *types.IngestTarget) {
		target.MayHaveLanded = true
		target.FirstSent = cmp.Or(target.FirstSent, now)
	})
	if err != nil {
		return nil, err
	} else if before == nil {
		return nil, w.gone(ctx)
	}
	w.firstSent = time.UnixMilli(cmp.Or(before.FirstSent, now))
	return before, nil
}

// gone ends the work of a join target another runner published, failed or replaced
func (w *joinWork) gone(ctx context.Context) error {
	zerolog.Ctx(ctx).Debug().Stringer("event_id", w.target.Event.ID).Msg("Join target is gone, another runner published, failed or replaced it")
	w.run.untarget(w.target.Event.ID)
	w.run.cancelNotice(w.target.Event.ID)
	return errWorkDone
}

// sendFailed handles a send_join that got no response, before being the target as it was before
// the send. A server refusing the join fails it, unless an earlier send may have landed, which
// returns errResendRefused, or another server answered one, so accepted the join, which leaves the
// join to the next attempt. Otherwise it pauses before the next attempt, failing the join once it
// timed out.
func (w *joinWork) sendFailed(ctx context.Context, before *types.IngestTarget, server string, attempt int, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	log := zerolog.Ctx(ctx).With().
		Stringer("event_id", w.target.Event.ID).
		Err(err).
		Str("server", server).
		Int("attempt", attempt+1).
		Logger()
	if errors.Is(err, errJoinTimedOut) {
		log.Warn().Msg("Timed out sending the join")
		return w.run.fail(ctx, w.target, failureFromError(err))
	}
	switch classifySendJoinError(err) {
	case sendJoinRefused:
		if w.answered {
			log.Warn().Msg("Server refused a join another server accepted")
			break
		} else if !before.MayHaveLanded {
			log.Warn().Msg("Server refused the join")
			return w.run.fail(ctx, w.target, failureFromError(err))
		}
		log.Warn().Msg("Server refused a join an earlier send_join may have landed with")
		return fmt.Errorf("%w: %w", errResendRefused, err)
	case sendJoinRateLimited:
		if !before.MayHaveLanded {
			if _, err := w.run.jobs.db.Rooms.UpdateIngestTarget(ctx, w.run.roomID, w.target.Event.ID, func(target *types.IngestTarget) {
				target.MayHaveLanded = false
			}); err != nil {
				return err
			}
		}
	}
	if time.Now().After(w.timesOut()) {
		log.Warn().Msg("Giving up sending the join")
		if w.answered {
			return w.run.fail(ctx, w.target, incompleteStateDAGFailure())
		}
		return w.run.fail(ctx, w.target, unreachableFailure(err))
	}
	// Each attempt is bounded by the request's timeout, and JoinTimeout bounds them all
	util.ReportProgress(ctx)
	pause := sendJoinRetryPause(attempt)
	log.Info().Dur("retry_in", pause).Msg("Failed to send the join, sending it again")
	select {
	case <-time.After(pause):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sendJoinRetryPause(attempt int) time.Duration {
	pause := min(firstSendJoinRetryPause<<min(attempt, 8), longestSendJoinRetryPause)
	return pause/2 + rand.N(pause/2)
}

// replaceJoin replaces the target with a join from a fresh make_join, which the next round sends,
// unless it replaced the user's first join already
func (w *joinWork) replaceJoin(ctx context.Context, refusal error) error {
	if w.target.Replacements >= maxJoinReplacements {
		zerolog.Ctx(ctx).Warn().Int("replacements", w.target.Replacements).Msg("Not replacing a refused join again")
		return w.run.fail(ctx, w.target, failureFromError(refusal))
	}
	userID := id.UserID(*w.target.Event.StateKey)
	joinEv, server, err := w.run.jobs.makeJoin(ctx, w.run.roomID, w.run.roomVersion, userID, w.target.Servers)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to make a fresh join")
		return w.run.fail(ctx, w.target, failureFromError(refusal))
	}
	servers := slices.Concat([]string{server}, slices.DeleteFunc(slices.Clone(w.target.Servers), func(other string) bool {
		return other == server
	}))
	if replaced, err := w.run.jobs.db.Rooms.ReplaceIngestJoin(ctx, w.target, joinEv, servers); err != nil {
		return err
	} else if !replaced {
		// The user's iju no longer names the target, which would otherwise be refused again each round
		return w.run.fail(ctx, w.target, failureFromError(refusal))
	}
	w.run.untarget(w.target.Event.ID)
	w.run.cancelNotice(w.target.Event.ID)
	zerolog.Ctx(ctx).Info().
		Stringer("event_id", w.target.Event.ID).
		Stringer("new_event_id", joinEv.ID).
		Msg("Replaced a join the resident server refused with a fresh one")
	return errWorkDone
}

// legacyJoin is a join to a room without a state DAG. Phase A sends the stored join event in a
// send_join, verifies the response and stages its events.
type legacyJoin struct {
	joinWork
}

func (w *legacyJoin) fetch(ctx context.Context) error {
	stateEvs, authEvs, err := w.sendJoin(ctx)
	if err != nil {
		return err
	}
	w.run.setRoomName(roomDisplayName(stateEvs))
	w.join, err = w.run.jobs.db.NewRemoteJoin(ctx, w.target.Event, util.EventsToIDs(stateEvs), slices.Concat(stateEvs, authEvs))
	if err != nil {
		return err
	}
	return w.refused(ctx, w.join.Stage(ctx))
}

// sendJoin sends the stored join event to a server of the room, and returns the verified state
// before the join and its auth chain. A send that may have landed is sent again until the join
// times out. Should a server refuse a join an earlier send may have landed with, the state before
// the join is fetched instead, or failing that the target replaced with a fresh join.
func (w *legacyJoin) sendJoin(ctx context.Context) (stateEvs, authEvs []*types.Event, err error) {
	for attempt := 0; ; attempt++ {
		server := w.target.Servers[attempt%len(w.target.Servers)]
		before, err := w.mayLand(ctx)
		if err != nil {
			return nil, nil, err
		}
		state, authChain, err := w.run.jobs.fetcher.SendJoin(ctx, server, w.target.Event, w.timesOut())
		var failure *types.IngestFailure
		if err == nil {
			util.ReportProgress(ctx)
			return w.verifyResponse(ctx, state, authChain)
		} else if errors.As(err, &failure) {
			return nil, nil, w.run.fail(ctx, w.target, failure)
		} else if err := w.sendFailed(ctx, before, server, attempt, err); errors.Is(err, errResendRefused) {
			return w.afterRefusedResend(ctx, err)
		} else if err != nil {
			return nil, nil, err
		}
	}
}

// afterRefusedResend fetches the state before a join that may have landed from the target's servers,
// which store it once they accepted it. Should none give it, the target is replaced with a fresh join.
func (w *legacyJoin) afterRefusedResend(ctx context.Context, refusal error) ([]*types.Event, []*types.Event, error) {
	ev := w.target.Event
	limit := w.run.jobs.config.Rooms.Ingest.MaxJoinEvents
	for _, server := range w.target.Servers {
		stateEvs, authEvs, err := w.run.jobs.fetcher.StateBefore(ctx, server, ev.RoomID, ev.RoomVersion, ev.ID, limit)
		util.ReportProgress(ctx)
		var failure *types.IngestFailure
		if errors.As(err, &failure) {
			return nil, nil, w.run.fail(ctx, w.target, failure)
		} else if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("server", server).Msg("Failed to fetch the state before the join")
			continue
		}
		return stateEvs, authEvs, nil
	}
	return nil, nil, w.replaceJoin(ctx, refusal)
}

// verifyResponse verifies the events of a send_join response, skipping those failing
func (w *legacyJoin) verifyResponse(ctx context.Context, state, authChain []json.RawMessage) ([]*types.Event, []*types.Event, error) {
	roomVersion := w.target.Event.RoomVersion
	seenIDs := make(map[id.EventID]struct{}, len(state)+len(authChain))
	stateEvs, err := util.VerifyRemoteEvents(ctx, state, roomVersion, w.run.jobs.keyStore, seenIDs)
	if err != nil {
		return nil, nil, err
	}
	authEvs, err := util.VerifyRemoteEvents(ctx, authChain, roomVersion, w.run.jobs.keyStore, seenIDs)
	if err != nil {
		return nil, nil, err
	}
	util.ReportProgress(ctx)
	return stateEvs, authEvs, nil
}

// legacyPDUs are PDUs of a room sent here while it had a job, or citing events it does not store.
// For those with a gap, phase A pulls the events they cite that are not stored here, and the state
// at prev events without it, asking each PDU's origin first, then the room's other servers. Phase C
// sends them as federated events, dropping those still citing missing events as /send did before.
type legacyPDUs struct {
	run  *jobRun
	pdus []*types.IngestTarget
	gap  bool
	fill *GapFill
}

func (w *legacyPDUs) targets() []*types.IngestTarget {
	return w.pdus
}

func (w *legacyPDUs) kind() workKind {
	return pduWorkKind(w.gap)
}

// fetch fills the PDUs' gaps. A gap no server filled leaves its PDU to be dropped once every server
// asked answered or refused, as asking again would not change it.
func (w *legacyPDUs) fetch(ctx context.Context) error {
	if !w.gap {
		return nil
	}
	db := w.run.jobs.db
	// Kept as outlier memberships or dropped, as a room this server is not in takes PDUs
	if joined, err := db.Rooms.IsServerJoined(ctx, w.run.roomID, w.run.jobs.config.ServerName); err != nil || !joined {
		return err
	}
	roomServers, err := db.Rooms.RoomServers(ctx, w.run.roomID)
	if err != nil {
		return err
	}
	w.fill, err = w.run.jobs.fetcher.GapFill(ctx, w.run.roomID, w.run.roomVersion, w.pdus, roomServers)
	return err
}

func (w *legacyPDUs) build(context.Context) error {
	return nil
}

// publish sends the PDUs, but for those whose gap a server failed to answer for and those citing
// them, which stay targets, failing the work for a later round to ask again
func (w *legacyPDUs) publish(ctx context.Context) error {
	var pulled []*types.Event
	var given *rooms.GivenStates
	sent, held := w.pdus, []*types.IngestTarget(nil)
	if w.fill != nil {
		pulled, given = w.fill.Pulled, w.fill.Given
		sent, held = holdUnanswered(w.pdus, w.fill.Unanswered)
	}
	if len(sent) > 0 {
		if err := w.send(ctx, pulled, given, sent); err != nil {
			return err
		}
	}
	if len(held) == 0 {
		return nil
	}
	w.pdus = held
	return fmt.Errorf("no server gave what %d PDUs cite: %w", len(held), errors.Join(w.fill.Errs...))
}

func (w *legacyPDUs) send(ctx context.Context, pulled []*types.Event, given *rooms.GivenStates, targets []*types.IngestTarget) error {
	return w.run.sendPDUs(ctx, pulled, given, targets)
}

// sendPDUs sends PDU targets as federated events, after the events pulled for them, then clears
// those the publish did not
func (run *jobRun) sendPDUs(ctx context.Context, pulled []*types.Event, given *rooms.GivenStates, targets []*types.IngestTarget) error {
	pdus := make(map[id.EventID]types.IngestTargetKind, len(targets))
	// Pulled events are older so go first, the send keeps the order of unrelated events
	evs := slices.Clone(pulled)
	for _, target := range targets {
		pdus[target.Event.ID] = types.IngestTargetPDU
		evs = append(evs, target.Event)
	}
	res, err := run.jobs.db.SendFederatedEvents(ctx, run.roomID, evs, given, rooms.SendFederatedEventsOptions{Targets: pdus})
	if err != nil {
		return err
	}
	for _, rejected := range res.Rejected {
		if _, found := pdus[rejected.Event.ID]; found && errors.Is(rejected.Error, rooms.ErrEventDropped) {
			zerolog.Ctx(ctx).Warn().Err(rejected.Error).Stringer("event_id", rejected.Event.ID).Msg("Dropped PDU target")
		}
	}
	// What the publish did not clear: dropped events, and outlier memberships of a room this server
	// is not in, which have no publish
	if err := run.jobs.db.Rooms.ClearIngestTargets(ctx, run.roomID, targetIDs(targets)); err != nil {
		return err
	}
	run.untarget(targetIDs(targets)...)
	return nil
}

// holdUnanswered splits PDU targets in topological order into those to send and those held back:
// the unanswered and those citing a held one
func holdUnanswered(pdus []*types.IngestTarget, unanswered map[id.EventID]struct{}) (sent, held []*types.IngestTarget) {
	heldIDs := make(map[id.EventID]struct{}, len(unanswered))
	maps.Copy(heldIDs, unanswered)
	for _, target := range pdus {
		if _, found := heldIDs[target.Event.ID]; found || citesAny(target.Event, heldIDs) {
			heldIDs[target.Event.ID] = struct{}{}
			held = append(held, target)
		} else {
			sent = append(sent, target)
		}
	}
	return sent, held
}

func citesAny(ev *types.Event, eventIDs map[id.EventID]struct{}) bool {
	cited := func(eventID id.EventID) bool {
		_, found := eventIDs[eventID]
		return found
	}
	return slices.ContainsFunc(ev.PrevEventIDs, cited) || slices.ContainsFunc(ev.AuthDependencyIDs(), cited)
}

// Most servers a gap fill asks beyond the PDUs' origins, and asks for the gaps of PDUs other servers
// sent
const maxGapFillServers = 5

// gapFillServers are the servers a gap fill asks, in order, without this one: the PDUs' origins, each
// of which has what its PDUs cite, then the room's other servers while fewer than maxGapFillServers
// are asked, so that a large room's dead servers cannot stretch a fill over hours
func gapFillServers(origins, roomServers []string, self string) []string {
	var servers []string
	add := func(server string) {
		if server != self && !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	for _, server := range origins {
		add(server)
	}
	for _, server := range roomServers {
		if len(servers) >= maxGapFillServers {
			break
		}
		add(server)
	}
	return servers
}

// roomDisplayName is how BabbleBot names a room from its state events: by name, else canonical alias,
// the last of each when there are several, as a state DAG in topological order holds every name the
// room had
func roomDisplayName(stateEvs []*types.Event) string {
	var name, alias string
	for _, ev := range stateEvs {
		if ev.StateKey == nil || *ev.StateKey != "" {
			continue
		}
		switch ev.Type {
		case event.StateRoomName:
			name = gjson.GetBytes(ev.Content, "name").String()
		case event.StateCanonicalAlias:
			alias = gjson.GetBytes(ev.Content, "alias").String()
		}
	}
	return cmp.Or(name, alias)
}

// makeJoin makes and signs a fresh join of the user from the first server giving a template of the
// room's version
func (j *Jobs) makeJoin(ctx context.Context, roomID id.RoomID, roomVersion string, userID id.UserID, servers []string) (*types.Event, string, error) {
	origin := spec.ServerName(j.config.ServerName)
	var errs []error
	for _, server := range servers {
		resp, err := j.fclient.MakeJoin(ctx, origin, spec.ServerName(server), roomID.String(), userID.String())
		// Held to the versions /join takes templates of
		if version := string(resp.RoomVersion); err == nil && (version != roomVersion || !j.config.JoinsRoomVersion(version)) {
			err = fmt.Errorf("%w: %s", errUnsupportedRoomVersion, version)
		}
		var joinEv *types.Event
		if err == nil {
			joinEv, err = util.MembershipTemplateEvent(userID, roomID, event.MembershipJoin, resp.JoinEvent, resp.RoomVersion)
		}
		if err == nil {
			keyID, key := j.config.MustGetActiveSigningKey()
			err = util.HashAndSignEvent(joinEv, j.config.ServerName, keyID, key)
		}
		util.ReportProgress(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", server, err))
			continue
		}
		return joinEv, server, nil
	}
	return nil, "", errors.Join(errs...)
}
