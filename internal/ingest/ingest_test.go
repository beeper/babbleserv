package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func remoteError(code int, contents string) error {
	return fmt.Errorf("send_join: %w", gomatrix.HTTPError{Code: code, Message: "Failed to PUT JSON", Contents: []byte(contents)})
}

func TestClassifySendJoinError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want sendJoinOutcome
	}{
		{"forbidden", remoteError(http.StatusForbidden, `{"errcode":"M_FORBIDDEN"}`), sendJoinRefused},
		{"not found", remoteError(http.StatusNotFound, `{}`), sendJoinRefused},
		{"bad request", remoteError(http.StatusBadRequest, `{}`), sendJoinRefused},
		{"rate limited", remoteError(http.StatusTooManyRequests, `{"errcode":"M_LIMIT_EXCEEDED"}`), sendJoinRateLimited},
		{"server error", remoteError(http.StatusInternalServerError, `{}`), sendJoinRetry},
		{"bad gateway", remoteError(http.StatusBadGateway, `<html>`), sendJoinRetry},
		{"timeout", context.DeadlineExceeded, sendJoinRetry},
		{"connection", &net.OpError{Op: "dial", Err: errors.New("refused")}, sendJoinRetry},
		{"undecodable response", errors.New("unexpected EOF"), sendJoinRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifySendJoinError(tc.err))
		})
	}
}

func TestFailureFromError(t *testing.T) {
	assert.Equal(t, &types.IngestFailure{Status: 403, ErrCode: "M_FORBIDDEN", Message: "You are banned"},
		failureFromError(remoteError(403, `{"errcode":"M_FORBIDDEN","error":"You are banned"}`)),
		"a remote error is answered as the remote server gave it")

	failure := failureFromError(remoteError(502, `<html>bad gateway</html>`))
	assert.Equal(t, 502, failure.Status)
	assert.Equal(t, "M_UNKNOWN", failure.ErrCode)

	failure = failureFromError(fmt.Errorf("%w: not allowed", rooms.ErrAuthStage5))
	assert.Equal(t, 403, failure.Status)
	assert.Equal(t, "M_FORBIDDEN", failure.ErrCode)

	given := &types.IngestFailure{Status: 413, ErrCode: "M_TOO_LARGE", Message: "big"}
	assert.Same(t, given, failureFromError(fmt.Errorf("wrapped: %w", given)))

	failure = failureFromError(fmt.Errorf("%w: org.matrix.msc4242.12", errUnsupportedRoomVersion))
	assert.Equal(t, 400, failure.Status)
	assert.Equal(t, "M_UNSUPPORTED_ROOM_VERSION", failure.ErrCode)

	failure = failureFromError(errors.New("storage failed: key 0x15 conflicted"))
	assert.Equal(t, 500, failure.Status)
	assert.Equal(t, "M_UNKNOWN", failure.ErrCode)
	assert.NotContains(t, failure.Message, "storage", "this server's errors are not passed on")

	for _, err := range []error{errNoProgress, context.Canceled, fmt.Errorf("send: %w", context.DeadlineExceeded)} {
		failure = failureFromError(err)
		assert.Equal(t, "Timed out joining the room", failure.Message, err)
		assert.NotContains(t, failure.Message, "context")
	}

	failure = unreachableFailure(&net.OpError{Op: "dial", Err: errors.New("connection refused")})
	assert.Equal(t, 502, failure.Status)
	assert.NotContains(t, failure.Message, "dial")
	assert.Equal(t, 503, unreachableFailure(remoteError(503, `{"errcode":"M_UNKNOWN","error":"down"}`)).Status)

	failure = tooLargeFailure("state and auth chain have", 2000, 1000)
	assert.Equal(t, 413, failure.Status)
	assert.Contains(t, failure.Message, "2000")
}

func testTarget(kind types.IngestTargetKind, eventID id.EventID, enqueued int64) *types.IngestTarget {
	stateKey := "@alice:example.com"
	ev := &types.Event{ID: eventID}
	ev.RoomID, ev.Type = "!room:example.com", event.EventMessage
	if kind == types.IngestTargetJoin {
		ev.Type, ev.StateKey, ev.Content = event.StateMember, &stateKey, []byte(`{"membership":"join"}`)
	}
	return &types.IngestTarget{Kind: kind, Event: ev, Enqueued: enqueued}
}

func TestLegacyWorksRunJoinsOldestFirst(t *testing.T) {
	joins, pdus := splitTargets([]*types.IngestTarget{
		testTarget(types.IngestTargetPDU, "$pdu2", 5),
		testTarget(types.IngestTargetJoin, "$join2", 3),
		testTarget(types.IngestTargetPDU, "$pdu1", 2),
		testTarget(types.IngestTargetJoin, "$join1", 1),
		testTarget(types.IngestTargetPDU, "$pdu0", 2),
	})
	citesJoin := testTarget(types.IngestTargetPDU, "$citesJoin", 6)
	citesJoin.Event.PrevEventIDs = []id.EventID{"$join1"}
	citesThat := testTarget(types.IngestTargetPDU, "$citesThat", 7)
	citesThat.Event.PrevEventIDs = []id.EventID{"$citesJoin"}

	kinds := legacyWorkKinds(&jobRun{})
	works := roundWorks(kinds, joins, pdus[1:], []*types.IngestTarget{pdus[0], citesJoin, citesThat}, true)
	require.Len(t, works, 5)
	assert.Equal(t, []id.EventID{"$pdu1", "$pdu2"}, targetIDs(works[0].targets()),
		"in a room this server is in, connected PDUs go first, together")
	assert.False(t, works[0].(*legacyPDUs).gap)
	assert.Equal(t, []id.EventID{"$pdu0"}, targetIDs(works[1].targets()), "then those with a gap")
	assert.True(t, works[1].(*legacyPDUs).gap)
	assert.Equal(t, []id.EventID{"$join1"}, targetIDs(works[2].targets()), "then the joins, oldest first")
	assert.Equal(t, []id.EventID{"$join2"}, targetIDs(works[3].targets()))
	assert.Equal(t, []id.EventID{"$citesJoin", "$citesThat"}, targetIDs(works[4].targets()),
		"PDUs citing a pending join, or one citing it, go after the joins")
	assert.True(t, works[4].(*legacyPDUs).gap)

	works = roundWorks(kinds, joins, nil, nil, false)
	assert.Len(t, works, 2, "while this server is not in the room, PDUs wait for its joins")

	works = roundWorks(kinds, nil, pdus, nil, false)
	require.Len(t, works, 1, "a room's PDUs go once no join is left, joined or not")
	assert.Len(t, works[0].targets(), 3)
}

func testPDU(eventID id.EventID, prevIDs []id.EventID, authIDs ...id.EventID) *types.Event {
	ev := &types.Event{ID: eventID, RoomVersion: "11", PrevEventIDs: prevIDs, AuthEventIDs: authIDs}
	ev.RoomID, ev.Type = "!room:example.com", event.EventMessage
	return ev
}

func TestSplitConnected(t *testing.T) {
	stored := map[id.EventID]struct{}{"$create": {}, "$power": {}, "$member": {}, "$tip": {}}
	auth := []id.EventID{"$create", "$power", "$member"}
	evs := rooms.SortEventsTopologically([]*types.Event{
		testPDU("$citesConnected", []id.EventID{"$connected"}, auth...),
		testPDU("$connected", []id.EventID{"$tip"}, auth...),
		testPDU("$gap", []id.EventID{"$missing"}, auth...),
		testPDU("$citesGap", []id.EventID{"$gap", "$connected"}, auth...),
		testPDU("$missingAuth", []id.EventID{"$tip"}, "$create", "$outlier"),
		testPDU("$merge", []id.EventID{"$tip", "$citesConnected"}, auth...),
	})
	connected, rest := splitConnected(evs, stored, nil, nil)
	assert.Equal(t, []id.EventID{"$connected", "$citesConnected", "$merge"}, util.EventsToIDs(connected),
		"citing stored events and earlier connected ones, in topological order")
	assert.Equal(t, []id.EventID{"$gap", "$citesGap", "$missingAuth"}, util.EventsToIDs(rest),
		"citing an unstored prev or auth event, or an event with a gap")

	storedIDs, prevStateIDs := citedEventIDs(evs, nil)
	assert.ElementsMatch(t,
		[]id.EventID{"$citesConnected", "$connected", "$create", "$gap", "$member", "$missing", "$outlier", "$power", "$tip"},
		storedIDs, "each cited event once")
	assert.Empty(t, prevStateIDs, "only state DAG events cite prev state events")
}

func stateDAGTestPDU(eventID id.EventID, prevIDs []id.EventID, prevStateIDs ...id.EventID) *types.Event {
	ev := &types.Event{ID: eventID, RoomVersion: string(gomatrixserverlib.RoomVersionStateDAGs), PrevEventIDs: prevIDs, PrevStateEventIDs: prevStateIDs}
	ev.RoomID, ev.Type = "!createhash", event.EventMessage
	return ev
}

// A state DAG event is connected once its prev state events, the create event among them, are in
// place here or are earlier connected events, and its prev events are stored or earlier connected
// events, unless it was pulled for another's prev events
func TestSplitConnectedStateDAG(t *testing.T) {
	inPlace := map[id.EventID]struct{}{"$createhash": {}, "$tip": {}, "$state": {}, "$otherRoom": {}}
	stored := map[id.EventID]struct{}{"$tip": {}, "$staged": {}}
	evs := rooms.SortEventsTopologically([]*types.Event{
		stateDAGTestPDU("$message", []id.EventID{"$tip"}, "$state"),
		stateDAGTestPDU("$citesStaged", []id.EventID{"$tip"}, "$staged"),
		stateDAGTestPDU("$citesStagedAsPrev", []id.EventID{"$staged"}, "$state"),
		stateDAGTestPDU("$unknownPrev", []id.EventID{"$missing"}, "$state"),
		stateDAGTestPDU("$pulledUnknownPrev", []id.EventID{"$missing"}, "$state"),
		stateDAGTestPDU("$citesMessage", []id.EventID{"$message"}, "$message"),
		stateDAGTestPDU("$citesOtherRoom", []id.EventID{"$tip"}, "$otherRoom"),
		stateDAGTestPDU("$citesGap", []id.EventID{"$tip"}, "$citesStaged"),
	})
	pulled := map[id.EventID]struct{}{"$pulledUnknownPrev": {}}
	connected, rest := splitConnected(evs, stored, inPlace, pulled)
	assert.Equal(t, []id.EventID{"$message", "$citesStagedAsPrev", "$pulledUnknownPrev", "$citesMessage", "$citesOtherRoom"}, util.EventsToIDs(connected),
		"a prev event stored without state is in place as a prev event, and an earlier connected event is in place")
	assert.Equal(t, []id.EventID{"$citesStaged", "$unknownPrev", "$citesGap"}, util.EventsToIDs(rest),
		"citing a prev state event stored without state, an unknown prev event when sent here, or an event with a gap")

	storedIDs, prevStateIDs := citedEventIDs(evs, pulled)
	assert.ElementsMatch(t, []id.EventID{"$tip", "$staged", "$missing", "$message"}, storedIDs,
		"the prev events, but for those of a pulled event")
	assert.ElementsMatch(t, []id.EventID{"$createhash", "$state", "$staged", "$message", "$otherRoom", "$citesStaged"}, prevStateIDs,
		"the prev state events with the implicit create event")

	connected, rest = splitConnected(evs[:1], stored, map[id.EventID]struct{}{"$state": {}}, nil)
	assert.Empty(t, connected)
	assert.Len(t, rest, 1, "the create event must be in place too")
}

func TestSplitConnectedNeedsTheImplicitCreateEvent(t *testing.T) {
	ev := testPDU("$message", []id.EventID{"$tip"}, "$power", "$member")
	ev.RoomID, ev.RoomVersion = "!createhash", "12"
	stored := map[id.EventID]struct{}{"$tip": {}, "$power": {}, "$member": {}}

	connected, rest := splitConnected([]*types.Event{ev}, stored, nil, nil)
	assert.Empty(t, connected)
	assert.Len(t, rest, 1, "a v12 event cites its room's create event without listing it")

	stored["$createhash"] = struct{}{}
	connected, _ = splitConnected([]*types.Event{ev}, stored, nil, nil)
	assert.Len(t, connected, 1)
	storedIDs, _ := citedEventIDs([]*types.Event{ev}, nil)
	assert.Contains(t, storedIDs, id.EventID("$createhash"))
}

func TestRefusedRequest(t *testing.T) {
	answered := func(code int, wrapped error) error {
		return fmt.Errorf("get_missing_events: %w", mautrix.HTTPError{Response: &http.Response{StatusCode: code}, WrappedError: wrapped})
	}
	assert.True(t, refusedRequest(answered(http.StatusNotFound, nil)))
	assert.True(t, refusedRequest(answered(http.StatusForbidden, nil)))
	assert.True(t, refusedRequest(answered(http.StatusOK, &json.SyntaxError{})), "a response that is not JSON")
	assert.True(t, refusedRequest(answered(http.StatusOK, &json.UnmarshalTypeError{})), "JSON that is not what was asked")
	assert.True(t, refusedRequest(answered(http.StatusOK, mautrix.ErrBodyReadReachedLimit)), "a response too large")
	assert.True(t, refusedRequest(answered(http.StatusOK, fmt.Errorf("%w (600 MiB)", mautrix.ErrResponseTooLong))))
	assert.False(t, refusedRequest(answered(http.StatusOK, context.DeadlineExceeded)),
		"a response whose body timed out on its way")
	assert.False(t, refusedRequest(answered(http.StatusOK, io.ErrUnexpectedEOF)), "a connection cut off mid-body")
	assert.False(t, refusedRequest(answered(http.StatusTooManyRequests, nil)))
	assert.False(t, refusedRequest(answered(http.StatusBadGateway, nil)))
	assert.False(t, refusedRequest(mautrix.HTTPError{Message: "request error", WrappedError: context.DeadlineExceeded}),
		"no response, as a timeout or a broken connection")
	assert.False(t, refusedRequest(errors.New("failed to verify")))
}

func TestChunkPDUs(t *testing.T) {
	const evSize = 100
	var pdus []*types.IngestTarget
	for i := range 7 {
		pdu := testTarget(types.IngestTargetPDU, id.EventID(fmt.Sprintf("$pdu%d", i)), int64(i))
		pdu.Size = evSize
		pdus = append(pdus, pdu)
	}
	big := testTarget(types.IngestTargetPDU, "$big", 7)
	big.Size = 5 * evSize
	pdus = append(pdus, big)

	chunks := chunkPDUs(pdus, 3, 4*evSize)
	var sizes []int
	var order []id.EventID
	for _, chunk := range chunks {
		sizes = append(sizes, len(chunk))
		order = append(order, targetIDs(chunk)...)
	}
	assert.Equal(t, []int{3, 3, 1, 1}, sizes, "by count, then a chunk the big event would overfill")
	assert.Equal(t, targetIDs(pdus), order, "enqueue order is kept")
	assert.Empty(t, chunkPDUs(nil, 3, 100))
	assert.Len(t, chunkPDUs(pdus[:1], 3, 1), 1, "an event past the byte bound goes alone")
}

func TestStrategyFor(t *testing.T) {
	for _, version := range []string{"10", "11", "12"} {
		strategy, err := strategyFor(version)
		require.NoError(t, err, version)
		assert.IsType(t, legacyStrategy{}, strategy)
	}
	strategy, err := strategyFor("org.matrix.msc4242.12")
	require.NoError(t, err)
	assert.IsType(t, stateDAGStrategy{}, strategy)
	_, err = strategyFor("unknown")
	assert.ErrorIs(t, err, errUnsupportedRoomVersion)
}

func TestGapFillServers(t *testing.T) {
	assert.Equal(t,
		[]string{"origin.example", "second.example", "resident.example"},
		gapFillServers(
			[]string{"origin.example", "second.example", "origin.example"},
			[]string{"self.example", "resident.example", "second.example"},
			"self.example",
		),
		"origins first, then the room's other servers, each once",
	)
	assert.Empty(t, gapFillServers(nil, []string{"self.example"}, "self.example"))

	var many []string
	for i := range 2 * maxGapFillServers {
		many = append(many, fmt.Sprintf("server%d.example", i))
	}
	servers := gapFillServers([]string{"origin.example"}, many, "self.example")
	assert.Len(t, servers, maxGapFillServers, "a large room's servers are not all asked")
	assert.Equal(t, "origin.example", servers[0])
	assert.Equal(t, many, gapFillServers(many, []string{"resident.example"}, "self.example"), "every origin is asked")
}

func TestHoldUnanswered(t *testing.T) {
	pdus := []*types.IngestTarget{
		gapTarget("$filled", "a.example", "$pulled"),
		gapTarget("$unanswered", "a.example", "$missing"),
		gapTarget("$citesUnanswered", "a.example", "$unanswered"),
		gapTarget("$citesFilled", "a.example", "$filled"),
	}
	sent, held := holdUnanswered(pdus, map[id.EventID]struct{}{"$unanswered": {}})
	assert.Equal(t, []id.EventID{"$filled", "$citesFilled"}, targetIDs(sent))
	assert.Equal(t, []id.EventID{"$unanswered", "$citesUnanswered"}, targetIDs(held),
		"a PDU citing one held back would be dropped for its missing prev")

	sent, held = holdUnanswered(pdus, nil)
	assert.Len(t, sent, 4)
	assert.Empty(t, held)
}

func TestHeldBackWithinTheRetryPause(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	failed := testTarget(types.IngestTargetPDU, "$failed", 1)
	failed.FailedAt = now.Add(-time.Second).UnixMilli()
	dependent := gapTarget("$dependent", "a.example", "$failed")
	other := testTarget(types.IngestTargetPDU, "$other", 2)

	held := make(map[id.EventID]struct{})
	assert.True(t, heldBack([]*types.IngestTarget{other, failed}, held, now),
		"work whose target failed seconds ago is not asked again, as each PDU enqueued would")
	hold([]*types.IngestTarget{other, failed}, held)
	assert.True(t, heldBack([]*types.IngestTarget{dependent}, held, now), "nor work citing it")

	assert.False(t, heldBack([]*types.IngestTarget{other}, map[id.EventID]struct{}{}, now))
	assert.False(t, heldBack([]*types.IngestTarget{failed}, map[id.EventID]struct{}{}, now.Add(jobRetryPause)),
		"after the pause it runs again")
}

func stateEvent(evType event.Type, stateKey, content string) *types.Event {
	ev := &types.Event{}
	ev.Type, ev.StateKey, ev.Content = evType, &stateKey, []byte(content)
	return ev
}

func TestRoomDisplayName(t *testing.T) {
	alias := stateEvent(event.StateCanonicalAlias, "", `{"alias":"#room:example.com"}`)
	name := stateEvent(event.StateRoomName, "", `{"name":"The Room"}`)
	keyedName := stateEvent(event.StateRoomName, "other", `{"name":"Not It"}`)
	emptyName := stateEvent(event.StateRoomName, "", `{"name":""}`)

	assert.Equal(t, "The Room", roomDisplayName([]*types.Event{alias, keyedName, name}))
	assert.Equal(t, "#room:example.com", roomDisplayName([]*types.Event{emptyName, alias}))
	assert.Empty(t, roomDisplayName([]*types.Event{keyedName}))
}

func TestSendJoinRetryPause(t *testing.T) {
	for attempt := range 20 {
		pause := sendJoinRetryPause(attempt)
		ceiling := min(firstSendJoinRetryPause<<min(attempt, 8), longestSendJoinRetryPause)
		assert.GreaterOrEqual(t, pause, ceiling/2, "attempt %d", attempt)
		assert.LessOrEqual(t, pause, ceiling, "attempt %d", attempt)
	}
}

func TestFailureReasonKeepsServerErrorsOutOfNotices(t *testing.T) {
	assert.Equal(t, "you are not allowed to join it", failureReason(&types.IngestFailure{ErrCode: "M_FORBIDDEN", Message: "failed to auth event (step 5): x"}))
	assert.Equal(t, "it is too large for this server", failureReason(tooLargeFailure("state DAG has", 10, 1)))
	reason := failureReason(&types.IngestFailure{ErrCode: "M_UNKNOWN", Message: "storage failed"})
	assert.NotContains(t, reason, "storage")
}

func TestNoticeDelay(t *testing.T) {
	now := time.UnixMilli(100_000)
	assert.Equal(t, 50*time.Second, noticeDelay(90_000, time.Minute, now))
	assert.Zero(t, noticeDelay(10_000, time.Minute, now), "a join pending past the threshold is told at once")
	assert.Equal(t, joinNoticeTxnID("$join"), joinNoticeTxnID("$join"))
	assert.NotEqual(t, joinNoticeTxnID("$join"), joinOutcomeTxnID("$join"))
}

func TestJobsForgetARunnerThatPanicked(t *testing.T) {
	var cfg config.BabbleConfig
	cfg.Rooms.Ingest.LeaseTimeout = time.Minute
	// No system database to take the lock in, so the runner panics
	jobs := NewJobs(cfg, zerolog.Nop(), &databases.Databases{}, nil, nil, nil, nil)
	jobs.Run("!room:example.com")
	jobs.wg.Wait()
	assert.Empty(t, jobs.running, "a later Run starts a runner again")

	jobs.Stop()
	jobs.Run("!room:example.com")
	jobs.wg.Wait()
	assert.Empty(t, jobs.running, "nothing runs once stopped")
}
