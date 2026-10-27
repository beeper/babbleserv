package ingest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
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

// Pauses between sending a join's send_join again
const (
	firstSendJoinRetryPause   = 2 * time.Second
	longestSendJoinRetryPause = 30 * time.Second
)

// legacyJoin is a local user's join to a room through a resident server, run on the remote join
// path. Phase A sends the stored join event in a send_join, verifies the response and stages its
// events; phase B builds the state before the join; phase C publishes it, as a member's join should
// another have got this server into the room first.
type legacyJoin struct {
	run    *jobRun
	target *types.IngestTarget
	join   *databases.RemoteJoin
}

func (w *legacyJoin) targets() []*types.IngestTarget {
	return []*types.IngestTarget{w.target}
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

func (w *legacyJoin) build(ctx context.Context) error {
	return w.refused(ctx, w.join.BuildState(ctx))
}

func (w *legacyJoin) publish(ctx context.Context) error {
	res, err := w.join.Publish(ctx)
	if err := w.refused(ctx, err); err != nil {
		return err
	} else if len(res.Rejected) > 0 {
		// A stored duplicate, or the join sent as a member's
		return w.run.fail(ctx, w.target, rejectedFailure(res.Rejected[0].Error))
	}
	// Published, stored before, or the user joined with a new local event instead
	if err := w.run.jobs.db.Rooms.ClearIngestTargets(ctx, w.run.roomID, []id.EventID{w.target.Event.ID}); err != nil {
		return err
	}
	zerolog.Ctx(ctx).Info().Stringer("event_id", w.target.Event.ID).Msg("Published remote join")
	w.run.joinDone(ctx, w.target, nil)
	return nil
}

// refused fails the target on an error refusing the join, passing any other on
func (w *legacyJoin) refused(ctx context.Context, err error) error {
	if errors.Is(err, rooms.ErrAuthStage4) || errors.Is(err, rooms.ErrAuthStage5) {
		return w.run.fail(ctx, w.target, rejectedFailure(err))
	}
	return err
}

// sendJoin sends the stored join event to a server of the room, and returns the verified state
// before the join and its auth chain. A send that may have landed is sent again until the join
// times out. Should a server refuse a join an earlier send may have landed with, the state before
// the join is fetched instead, or failing that the target replaced with a fresh join.
func (w *legacyJoin) sendJoin(ctx context.Context) (stateEvs, authEvs []*types.Event, err error) {
	ev := w.target.Event
	log := zerolog.Ctx(ctx).With().Stringer("event_id", ev.ID).Logger()
	timeout := time.UnixMilli(w.target.Enqueued).Add(w.run.jobs.config.Rooms.Ingest.JoinTimeout)
	origin := spec.ServerName(w.run.jobs.config.ServerName)
	for attempt := 0; ; attempt++ {
		server := w.target.Servers[attempt%len(w.target.Servers)]
		before, err := w.run.jobs.db.Rooms.UpdateIngestTarget(ctx, w.run.roomID, ev.ID, func(target *types.IngestTarget) {
			target.MayHaveLanded = true
		})
		if err != nil {
			return nil, nil, err
		} else if before == nil {
			log.Debug().Msg("Join target is gone, another runner published, failed or replaced it")
			w.run.cancelNotice(ev.ID)
			return nil, nil, errWorkDone
		}

		resp, err := w.run.jobs.fclient.SendJoin(ctx, origin, spec.ServerName(server), ev.PDU())
		if err == nil {
			util.ReportProgress(ctx)
			return w.verifyResponse(ctx, resp.StateEvents, resp.AuthEvents)
		} else if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		log := log.With().Err(err).Str("server", server).Int("attempt", attempt+1).Logger()
		switch classifySendJoinError(err) {
		case sendJoinRefused:
			if !before.MayHaveLanded {
				log.Warn().Msg("Server refused the join")
				return nil, nil, w.run.fail(ctx, w.target, failureFromError(err))
			}
			log.Warn().Msg("Server refused a join an earlier send_join may have landed with")
			return w.afterRefusedResend(ctx, err)
		case sendJoinRateLimited:
			if !before.MayHaveLanded {
				if _, err := w.run.jobs.db.Rooms.UpdateIngestTarget(ctx, w.run.roomID, ev.ID, func(target *types.IngestTarget) {
					target.MayHaveLanded = false
				}); err != nil {
					return nil, nil, err
				}
			}
		}
		if time.Now().After(timeout) {
			log.Warn().Msg("Giving up sending the join")
			return nil, nil, w.run.fail(ctx, w.target, unreachableFailure(err))
		}
		// Each attempt is bounded by the request's timeout, and JoinTimeout bounds them all
		util.ReportProgress(ctx)
		pause := sendJoinRetryPause(attempt)
		log.Info().Dur("retry_in", pause).Msg("Failed to send the join, sending it again")
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func sendJoinRetryPause(attempt int) time.Duration {
	pause := min(firstSendJoinRetryPause<<min(attempt, 8), longestSendJoinRetryPause)
	return pause/2 + rand.N(pause/2)
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

// replaceJoin replaces the target with a join from a fresh make_join, which the next round sends
func (w *legacyJoin) replaceJoin(ctx context.Context, refusal error) error {
	userID := id.UserID(*w.target.Event.StateKey)
	joinEv, server, err := w.run.jobs.makeJoin(ctx, w.run.roomID, userID, w.target.Servers)
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
	w.run.cancelNotice(w.target.Event.ID)
	zerolog.Ctx(ctx).Info().
		Stringer("event_id", w.target.Event.ID).
		Stringer("new_event_id", joinEv.ID).
		Msg("Replaced a join the resident server refused with a fresh one")
	return errWorkDone
}

// verifyResponse verifies the events of a send_join response, skipping those failing, within the
// size a join accepts
func (w *legacyJoin) verifyResponse(ctx context.Context, state, authChain gomatrixserverlib.EventJSONs) ([]*types.Event, []*types.Event, error) {
	limit := w.run.jobs.config.Rooms.Ingest.MaxJoinEvents
	if count := len(state) + len(authChain); count > limit {
		return nil, nil, w.run.fail(ctx, w.target, tooLargeFailure(count, limit))
	}
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
	run    *jobRun
	pdus   []*types.IngestTarget
	gap    bool
	pulled []*types.Event
	given  *rooms.GivenStates
}

func (w *legacyPDUs) targets() []*types.IngestTarget {
	return w.pdus
}

func (w *legacyPDUs) events() []*types.Event {
	evs := make([]*types.Event, len(w.pdus))
	for i, target := range w.pdus {
		evs[i] = target.Event
	}
	return evs
}

// fetch fills the PDUs' gap. A fill every server refused leaves the PDUs to be dropped, as asking
// again would not change it; one failing otherwise fails the work, so that a later round asks again.
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
	var origins []string
	for _, target := range w.pdus {
		origins = append(origins, target.Servers...)
	}
	servers := gapFillServers(origins, roomServers, w.run.jobs.config.ServerName)
	pulled, given, err := w.run.jobs.fetcher.GapFill(ctx, servers, w.run.roomID, w.run.roomVersion, w.events())
	if errors.Is(err, errGapRefused) {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Servers refused the events the room's PDUs cite")
		return nil
	} else if err != nil {
		return err
	}
	w.pulled, w.given = pulled, given
	return nil
}

func (w *legacyPDUs) build(context.Context) error {
	return nil
}

func (w *legacyPDUs) publish(ctx context.Context) error {
	pdus := make(map[id.EventID]types.IngestTargetKind, len(w.pdus))
	for _, target := range w.pdus {
		pdus[target.Event.ID] = types.IngestTargetPDU
	}
	// Pulled events are older so go first, the send keeps the order of unrelated events
	evs := slices.Concat(w.pulled, w.events())
	res, err := w.run.jobs.db.SendFederatedEvents(ctx, w.run.roomID, evs, w.given, rooms.SendFederatedEventsOptions{Targets: pdus})
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
	return w.run.jobs.db.Rooms.ClearIngestTargets(ctx, w.run.roomID, targetIDs(w.pdus))
}

// Most servers a gap fill asks
const maxGapFillServers = 5

// gapFillServers are the servers a gap fill asks, in order: the PDUs' origins, then the room's
// other servers, without this one, at most maxGapFillServers
func gapFillServers(origins, roomServers []string, self string) []string {
	servers := make([]string, 0, maxGapFillServers)
	for _, server := range slices.Concat(origins, roomServers) {
		if len(servers) == maxGapFillServers {
			break
		} else if server != self && !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	return servers
}

// roomDisplayName is how BabbleBot names a room from its state: by name, else canonical alias
func roomDisplayName(stateEvs []*types.Event) string {
	var alias string
	for _, ev := range stateEvs {
		if ev.StateKey == nil || *ev.StateKey != "" {
			continue
		}
		switch ev.Type {
		case event.StateRoomName:
			if name := gjson.GetBytes(ev.Content, "name").String(); name != "" {
				return name
			}
		case event.StateCanonicalAlias:
			alias = gjson.GetBytes(ev.Content, "alias").String()
		}
	}
	return alias
}

// makeJoin makes and signs a fresh join of the user from the first server giving a template
func (j *Jobs) makeJoin(ctx context.Context, roomID id.RoomID, userID id.UserID, servers []string) (*types.Event, string, error) {
	origin := spec.ServerName(j.config.ServerName)
	var errs []error
	for _, server := range servers {
		resp, err := j.fclient.MakeJoin(ctx, origin, spec.ServerName(server), roomID.String(), userID.String())
		// Held to the versions /join takes templates of
		if version := string(resp.RoomVersion); err == nil &&
			(!j.config.JoinsRoomVersion(version) || util.RoomVersionHas(version, gomatrixserverlib.IRoomVersion.StateDAGs)) {
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
