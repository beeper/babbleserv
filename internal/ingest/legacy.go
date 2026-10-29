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

// joinWork is a local user's join through a resident server. The room version's strategy fetches in
// phase A; phases B and C build and publish the state before the join.
type joinWork struct {
	run    *jobRun
	target *types.IngestTarget
	join   *databases.RemoteJoin
	// A server accepted the join by answering a send_join, even one serving no state DAG connecting it
	answered  bool
	firstSent time.Time
}

const maxJoinReplacements = 1

func (w *joinWork) targets() []*types.IngestTarget {
	return []*types.IngestTarget{w.target}
}

func (w *joinWork) kind() workKind {
	return workJoin
}

// timesOut bounds the join's sends and the wait for a response to start
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
	if err := w.run.jobs.db.Rooms.ClearIngestTargets(ctx, w.run.roomID, []id.EventID{w.target.Event.ID}); err != nil {
		return err
	}
	w.run.untarget(w.target.Event.ID)
	zerolog.Ctx(ctx).Info().Stringer("event_id", w.target.Event.ID).Msg("Published remote join")
	w.run.joinDone(ctx, w.target, nil)
	return nil
}

func (w *joinWork) refused(ctx context.Context, err error) error {
	if errors.Is(err, rooms.ErrAuthStage4) || errors.Is(err, rooms.ErrAuthStage5) {
		return w.run.fail(ctx, w.target, rejectedFailure(err))
	}
	return err
}

var errResendRefused = errors.New("a server refused a join that may have landed")

// mayLand persists that the next send_join may land before it is sent, returning the target as it was
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

func (w *joinWork) gone(ctx context.Context) error {
	zerolog.Ctx(ctx).Debug().Stringer("event_id", w.target.Event.ID).Msg("Join target is gone, another runner published, failed or replaced it")
	w.run.untarget(w.target.Event.ID)
	w.run.cancelNotice(w.target.Event.ID)
	return errWorkDone
}

// sendFailed fails the join or pauses before the next attempt. A refusal fails it unless another
// server accepted it, or an earlier send may have landed, which returns errResendRefused.
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

// replaceJoin swaps the target for a fresh make_join's join for the next round to send, at most
// maxJoinReplacements times counting from the user's first join
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

// legacyJoin is a join to a room without a state DAG, fetched in phase A by a send_join
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

// sendJoin sends the join again until it times out, even when an earlier send may have landed
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

// afterRefusedResend fetches the state before a join that may have landed, which servers that
// accepted it store, replacing the join should none give it
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

// verifyResponse skips events failing verification
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

// legacyPDUs are PDUs sent while the room had a job, or citing events not stored here. Phase A fills
// their gaps; phase C sends them, dropping those still citing missing events as /send did before.
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

// fetch leaves a PDU whose gap no server filled to be dropped once every server asked answered or
// refused, as asking again would not change it
func (w *legacyPDUs) fetch(ctx context.Context) error {
	if !w.gap {
		return nil
	}
	db := w.run.jobs.db
	// A room this server is not in keeps PDUs as outlier memberships or drops them, without a fill
	if joined, err := db.Rooms.IsServerJoined(ctx, w.run.roomID, w.run.jobs.config.ServerName); err != nil || !joined {
		return err
	}
	roomServers, err := db.Rooms.RoomServers(ctx, w.run.roomID)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(w.run.jobs.config.Rooms.Ingest.GapFillTimeout)
	w.fill, err = w.run.jobs.fetcher.GapFill(ctx, w.run.roomID, w.run.roomVersion, w.pdus, roomServers, deadline)
	return err
}

func (w *legacyPDUs) build(context.Context) error {
	return nil
}

// publish holds back PDUs whose gap went unanswered, and those citing them, failing the work so a
// later round asks again
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

func (run *jobRun) sendPDUs(ctx context.Context, pulled []*types.Event, given *rooms.GivenStates, targets []*types.IngestTarget) error {
	pdus := make(map[id.EventID]types.IngestTargetKind, len(targets))
	// Pulled events go first as they are older; the send keeps the order of unrelated events
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
	// The publish leaves dropped events and outlier memberships as targets
	if err := run.jobs.db.Rooms.ClearIngestTargets(ctx, run.roomID, targetIDs(targets)); err != nil {
		return err
	}
	run.untarget(targetIDs(targets)...)
	return nil
}

// holdUnanswered takes pdus in topological order, holding back the unanswered and those citing them
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

const maxGapFillServers = 5

// gapFillServers asks the PDUs' origins first, as each has what its PDUs cite, then other servers up
// to maxGapFillServers, so a large room's dead servers cannot stretch a fill over hours
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

// roomDisplayName takes the last name, else canonical alias, as a state DAG holds every name the room had
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
