package ingest

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
)

type stateDAGRequestMade struct {
	server        string
	latestIDs     []id.EventID
	limit, staged int
}

// stateDAGServers answer get_missing_events as a babbleserv does with the MSC's flag, withholding
// cited events they do not know
type stateDAGServers struct {
	cites     map[id.EventID][]id.EventID
	published map[id.EventID]bool
	stored    map[id.EventID]bool
	pdus      []*types.Event
	// Other targets of the job, which the walk stops at
	pending   map[id.EventID]bool
	maxEvents int
	// State events nothing cites, given with each response
	junk     int
	errs     map[string]error
	requests []stateDAGRequestMade
}

func newStateDAGServers(cites map[id.EventID][]id.EventID, published ...id.EventID) *stateDAGServers {
	s := &stateDAGServers{cites: cites, published: make(map[id.EventID]bool), stored: make(map[id.EventID]bool), maxEvents: MaxStateDAGMissingEvents}
	for _, eventID := range append(published, "$createhash") {
		s.published[eventID] = true
	}
	return s
}

func chain(n int) map[id.EventID][]id.EventID {
	cites := map[id.EventID][]id.EventID{"$base": nil}
	prev := id.EventID("$base")
	for i := 1; i <= n; i++ {
		eventID := id.EventID(fmt.Sprintf("$s%02d", i))
		cites[eventID] = []id.EventID{prev}
		prev = eventID
	}
	return cites
}

func (s *stateDAGServers) pdu(eventID id.EventID, prevStateIDs ...id.EventID) *types.Event {
	ev := stateDAGTestPDU(eventID, nil, prevStateIDs...)
	s.cites[eventID] = prevStateIDs
	s.pdus = append(s.pdus, ev)
	return ev
}

func (s *stateDAGServers) request(_ context.Context, server string, latestIDs []id.EventID, limit, maxStaged int) (int, error) {
	s.requests = append(s.requests, stateDAGRequestMade{server, latestIDs, limit, maxStaged})
	if err := s.errs[server]; err != nil {
		return 0, err
	}
	hops := make(map[id.EventID]int)
	frontier := slices.Clone(latestIDs)
	for _, eventID := range latestIDs {
		hops[eventID] = 0
	}
	var walked []id.EventID
	for hop := 1; len(frontier) > 0; hop++ {
		var next []id.EventID
		for _, eventID := range frontier {
			for _, citedID := range s.cites[eventID] {
				if _, known := s.cites[citedID]; !known {
					continue
				} else if _, seen := hops[citedID]; !seen && !s.published[citedID] {
					hops[citedID] = hop
					next = append(next, citedID)
				}
			}
		}
		walked = append(walked, next...)
		frontier = next
	}
	slices.SortFunc(walked, func(a, b id.EventID) int { return cmp.Or(cmp.Compare(hops[a], hops[b]), cmp.Compare(a, b)) })
	staged := 0
	for _, eventID := range walked[:min(len(walked), limit, s.maxEvents)] {
		if !s.stored[eventID] && staged < maxStaged {
			s.stored[eventID] = true
			staged++
		}
	}
	return min(staged+s.junk, maxStaged), nil
}

func (s *stateDAGServers) walk(context.Context) (*rooms.StateDAGGap, error) {
	gap := &rooms.StateDAGGap{
		Cites:   make(map[id.EventID][]id.EventID),
		Missing: make(map[id.EventID]struct{}),
		Pending: make(map[id.EventID]struct{}),
	}
	seen := make(map[id.EventID]bool)
	var frontier []id.EventID
	for _, ev := range s.pdus {
		seen[ev.ID] = true
		frontier = append(frontier, ev.AuthDependencyIDs()...)
	}
	for len(frontier) > 0 {
		eventID := frontier[0]
		frontier = frontier[1:]
		if seen[eventID] || s.published[eventID] {
			continue
		}
		seen[eventID] = true
		if s.pending[eventID] {
			gap.Pending[eventID] = struct{}{}
			continue
		} else if !s.stored[eventID] {
			gap.Missing[eventID] = struct{}{}
			continue
		}
		gap.Cites[eventID] = s.cites[eventID]
		frontier = append(frontier, s.cites[eventID]...)
	}
	return gap, nil
}

func (s *stateDAGServers) fill(t *testing.T, maxPerPDU int, budget *int, servers ...string) *stateDAGFill {
	t.Helper()
	gap, err := s.walk(context.Background())
	require.NoError(t, err)
	fill := newStateDAGFill(s.pdus, gap, maxPerPDU, budget, time.Now().Add(time.Hour))
	require.NoError(t, fill.run(context.Background(), servers, s.request, s.walk))
	return fill
}

func (s *stateDAGServers) limits() []int {
	var limits []int
	for _, req := range s.requests {
		limits = append(limits, req.limit)
	}
	return limits
}

func unlimited() *int {
	budget := 1 << 30
	return &budget
}

// The limit doubles from 10 until no prev state event is missing, as the MSC recommends
func TestStateDAGFillGrowsTheLimit(t *testing.T) {
	servers := newStateDAGServers(chain(25), "$base")
	servers.pdu("$pdu", "$s25")
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	assert.Equal(t, []int{10, 20, 40}, servers.limits())
	for _, req := range servers.requests {
		assert.Equal(t, []id.EventID{"$pdu"}, req.latestIDs, "the latest events are the PDUs")
	}
	assert.Empty(t, fill.open())
	failures, held := fill.outcome()
	assert.Empty(t, failures)
	assert.Empty(t, held)
	assert.Equal(t, 25, fill.fetched["$pdu"], "the events staged for the PDU")
}

// A request names at most 100 events in latest_events, as the MSC bounds them
func TestStateDAGFillSplitsLatestEvents(t *testing.T) {
	servers := newStateDAGServers(map[id.EventID][]id.EventID{"$base": nil}, "$base")
	for i := range 150 {
		missing := id.EventID(fmt.Sprintf("$m%03d", i))
		servers.cites[missing] = []id.EventID{"$base"}
		servers.pdu(id.EventID(fmt.Sprintf("$pdu%03d", i)), missing)
	}
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	require.Greater(t, len(servers.requests), 2)
	assert.Len(t, servers.requests[0].latestIDs, 100)
	assert.Len(t, servers.requests[1].latestIDs, 50)
	for _, req := range servers.requests {
		assert.LessOrEqual(t, len(req.latestIDs), MaxStateDAGRequestIDs)
	}
	assert.Empty(t, fill.open(), "the limit, which counts events in all, grows until the gaps are filled")
}

// A server capping its responses repeats the events nearest the PDUs, so requests walk on from the frontier
func TestStateDAGFillWalksFromTheFrontier(t *testing.T) {
	servers := newStateDAGServers(chain(25), "$base")
	servers.maxEvents = 10
	servers.pdu("$pdu", "$s25")
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	require.Len(t, servers.requests, 4)
	assert.Equal(t, []id.EventID{"$pdu"}, servers.requests[1].latestIDs, "nothing new, as capped")
	assert.Equal(t, []id.EventID{"$s16"}, servers.requests[2].latestIDs, "then from the event citing the first missing one")
	assert.Equal(t, []id.EventID{"$s06"}, servers.requests[3].latestIDs)
	assert.Empty(t, fill.open())
}

// The PDU's origin is asked first, then the room's other servers, each left once its frontier gives nothing
func TestStateDAGFillAsksTheNextServer(t *testing.T) {
	servers := newStateDAGServers(chain(5), "$base")
	servers.pdu("$pdu", "$s05")
	servers.errs = map[string]error{"origin.example": mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusNotFound}}}
	fill := servers.fill(t, 10_000, unlimited(), "origin.example", "other.example")
	assert.Equal(t, "other.example", servers.requests[len(servers.requests)-1].server)
	assert.Empty(t, fill.open())
	failures, held := fill.outcome()
	assert.Empty(t, failures)
	assert.Empty(t, held, "a gap filled after a server failed is not held back")
}

func TestStateDAGFillOutcome(t *testing.T) {
	refused := mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	failed := mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusBadGateway}}

	servers := newStateDAGServers(map[id.EventID][]id.EventID{"$base": nil}, "$base")
	servers.pdu("$withheld", "$never")
	servers.pdu("$citesWithheld", "$withheld")
	servers.pdu("$connected", "$base")
	servers.errs = map[string]error{"origin.example": refused}
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	failures, held := fill.outcome()
	assert.Equal(t, unfilledGapFailure(), failures["$withheld"], "every server asked answered or refused")
	assert.Equal(t, unfilledGapFailure(), failures["$citesWithheld"], "a PDU citing one fails with it")
	assert.NotContains(t, failures, id.EventID("$connected"))
	assert.Empty(t, held)

	servers.requests = nil
	servers.errs = map[string]error{"origin.example": refused, "other.example": failed}
	fill = servers.fill(t, 10_000, unlimited(), "origin.example", "other.example")
	failures, held = fill.outcome()
	assert.Empty(t, failures)
	assert.Equal(t, map[id.EventID]struct{}{"$withheld": {}, "$citesWithheld": {}}, held, "a server failed to answer, so asking again may fill it")
}

// A PDU fails past its gap fill's staging bound and a run's fills stop past the run's; no request
// stages more than one event past either
func TestStateDAGFillBounds(t *testing.T) {
	servers := newStateDAGServers(chain(25), "$base")
	servers.pdu("$pdu", "$s25")
	fill := servers.fill(t, 15, unlimited(), "origin.example")
	assert.Equal(t, []int{10, 20}, servers.limits(), "no request once past the bound")
	assert.Equal(t, 6, servers.requests[1].staged, "the 5 the PDU may fetch still, and one to show the gap is larger")
	failures, _ := fill.outcome()
	assert.Equal(t, tooLargeGapFailure(15), failures["$pdu"])
	assert.Equal(t, 16, fill.fetched["$pdu"])

	servers = newStateDAGServers(chain(25), "$base")
	servers.pdu("$pdu", "$s25")
	budget := 12
	gap, err := servers.walk(context.Background())
	require.NoError(t, err)
	err = newStateDAGFill(servers.pdus, gap, 10_000, &budget, time.Now().Add(time.Hour)).run(context.Background(), []string{"origin.example"}, servers.request, servers.walk)
	assert.ErrorIs(t, err, errGapBudgetSpent)
}

func TestPublishChunks(t *testing.T) {
	ev := func(eventID id.EventID) *types.Event { return stateDAGTestPDU(eventID, nil) }
	targets := map[id.EventID]*types.IngestTarget{
		"$t1": {Event: ev("$t1"), Size: 10},
		"$t2": {Event: ev("$t2"), Size: 10},
	}
	stateless := map[id.EventID]struct{}{"$s1": {}, "$s0": {}}
	evs := []*types.Event{ev("$s0"), ev("$r1"), targets["$t1"].Event, ev("$s1"), ev("$r2"), targets["$t2"].Event}
	ids := func(chunks [][]*types.Event) [][]id.EventID {
		var out [][]id.EventID
		for _, chunk := range chunks {
			var chunkIDs []id.EventID
			for _, ev := range chunk {
				chunkIDs = append(chunkIDs, ev.ID)
			}
			out = append(out, chunkIDs)
		}
		return out
	}
	assert.Equal(t, [][]id.EventID{{"$s0", "$r1", "$t1"}, {"$s1", "$r2", "$t2"}}, ids(publishChunks(evs, targets, stateless, 10, 1<<20)),
		"an event without state goes in a chunk after any target, which it may cite")
	assert.Equal(t, [][]id.EventID{{"$s0", "$r1"}, {"$t1"}, {"$s1", "$r2"}, {"$t2"}}, ids(publishChunks(evs, targets, stateless, 2, 1<<20)))
	assert.Equal(t, [][]id.EventID{{"$t1"}, {"$t2"}}, ids(publishChunks([]*types.Event{targets["$t1"].Event, targets["$t2"].Event}, targets, nil, 10, 15)),
		"past its first event a chunk holds at most the bytes")
}

// Events citing the join, directly or through skipped ones, arrive as PDUs instead of being staged
func TestStateDAGJoinSkipsEventsNewerThanTheJoin(t *testing.T) {
	w := &stateDAGJoin{skipped: map[id.EventID]struct{}{"$join": {}}}
	served := []*types.Event{
		stateDAGTestPDU("$power", nil, "$create"),
		stateDAGTestPDU("$join", nil, "$power"),
		stateDAGTestPDU("$citesJoin", nil, "$join"),
		stateDAGTestPDU("$citesThat", nil, "$power", "$citesJoin"),
		stateDAGTestPDU("$fork", nil, "$power"),
	}
	kept := w.skipNewerThanJoin(served)
	assert.Equal(t, []id.EventID{"$power", "$fork"}, []id.EventID{kept[0].ID, kept[1].ID})
	assert.Len(t, kept, 2)
	assert.Len(t, w.skipped, 3)
}

// A refusal after another server accepted the join neither fails nor replaces it; it is resent after a pause
func TestSendFailedAfterAServerAnswered(t *testing.T) {
	var cfg config.BabbleConfig
	cfg.Rooms.Ingest.JoinTimeout = time.Hour
	w := &joinWork{
		run: &jobRun{jobs: &Jobs{config: cfg}},
		// Its timeout counts from its first send, not from when it was enqueued
		target:    &types.IngestTarget{Kind: types.IngestTargetJoin, Event: testTarget(types.IngestTargetJoin, "$join", 0).Event, Enqueued: time.Now().Add(-2 * time.Hour).UnixMilli()},
		firstSent: time.Now(),
	}
	before := &types.IngestTarget{MayHaveLanded: true}
	refused := gomatrix.HTTPError{Code: http.StatusForbidden}
	assert.ErrorIs(t, w.sendFailed(context.Background(), before, "other.example", 0, refused), errResendRefused)

	w.answered = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, w.sendFailed(ctx, before, "other.example", 0, refused), context.DeadlineExceeded, "pausing before the next attempt")
}

func TestDecodeStateDAGBoundsEachValue(t *testing.T) {
	// Over the bound by more than what the decoder may have read ahead of the value
	large := strings.Repeat("x", 2*maxSendJoinValueBytes)
	_, err := decodeChunks(t, `{"state_dag":[{"event_id":"$0","content":{"body":"`+large+`"}}]}`, 10, 1<<30)
	assert.ErrorIs(t, err, errBadSendJoinResponse, "an event")
	_, err = decodeChunks(t, `{"origin":"`+large+`","state_dag":[]}`, 10, 1<<30)
	assert.ErrorIs(t, err, errBadSendJoinResponse, "a member skipped")

	half := large[:maxSendJoinValueBytes/2]
	chunks, err := decodeChunks(t, `{"origin":"`+half+`","state_dag":[{"event_id":"$0","content":{"body":"`+half+`"}},{"event_id":"$1"}]}`, 10, 1<<30)
	require.NoError(t, err, "each value is bounded on its own")
	assert.Equal(t, [][]string{{"$0", "$1"}}, chunks)
}

// Events outside the gap, which a server could give endlessly, count against the bounds but earn no more requests
func TestStateDAGFillIgnoresEventsOutsideTheGap(t *testing.T) {
	servers := newStateDAGServers(map[id.EventID][]id.EventID{"$base": nil}, "$base")
	servers.pdu("$pdu", "$never")
	servers.junk = 1
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	assert.Len(t, servers.requests, 2, "the PDUs, then the frontier, both filling nothing")
	assert.Equal(t, 2, fill.fetched["$pdu"])
	failures, _ := fill.outcome()
	assert.Equal(t, unfilledGapFailure(), failures["$pdu"])
}

// A server giving the gap an event at a time is left after bounded rounds, and the PDU held back
// rather than failed, as Synapse caps its answers at 20 events
func TestStateDAGFillBoundsTheRoundsAServerIsAsked(t *testing.T) {
	servers := newStateDAGServers(chain(99), "$base")
	servers.maxEvents = 1
	servers.pdu("$pdu", "$s99")
	fill := servers.fill(t, 1000, unlimited(), "origin.example")
	assert.Len(t, servers.requests, stateDAGRounds(1000))
	assert.Equal(t, 10, stateDAGRounds(1000), "7 growing the limit, 2 at 1,000, 1 from the frontier")
	assert.NotEmpty(t, fill.open())
	failures, held := fill.outcome()
	assert.Empty(t, failures)
	assert.Equal(t, map[id.EventID]struct{}{"$pdu": {}}, held)
}

func TestStateDAGFillHoldsPDUsPastItsTime(t *testing.T) {
	servers := newStateDAGServers(chain(5), "$base")
	servers.pdu("$pdu", "$s05")
	servers.pdu("$connected", "$base")
	gap, err := servers.walk(context.Background())
	require.NoError(t, err)
	fill := newStateDAGFill(servers.pdus, gap, 10_000, unlimited(), time.Now())
	require.NoError(t, fill.run(context.Background(), []string{"origin.example"}, servers.request, servers.walk))
	assert.Empty(t, servers.requests)
	failures, held := fill.outcome()
	assert.Empty(t, failures)
	assert.Equal(t, map[id.EventID]struct{}{"$pdu": {}}, held)
}

func TestStateDAGFillNamesPartOfALargeFrontier(t *testing.T) {
	servers := newStateDAGServers(map[id.EventID][]id.EventID{"$base": nil}, "$base")
	servers.maxEvents = 1
	for i := range 150 {
		deep, near := id.EventID(fmt.Sprintf("$d%03d", i)), id.EventID(fmt.Sprintf("$n%03d", i))
		servers.cites[deep] = []id.EventID{"$base"}
		servers.cites[near] = []id.EventID{deep}
		servers.pdu(id.EventID(fmt.Sprintf("$pdu%03d", i)), near)
	}
	servers.fill(t, 10_000, unlimited(), "origin.example")
	frontierRequests := 0
	for _, req := range servers.requests {
		assert.LessOrEqual(t, len(req.latestIDs), MaxStateDAGRequestIDs)
		if !strings.HasPrefix(req.latestIDs[0].String(), "$pdu") {
			frontierRequests++
		}
	}
	require.NotZero(t, frontierRequests)
	assert.LessOrEqual(t, len(servers.requests)-frontierRequests, 2*stateDAGRounds(10_000), "each round names the open PDUs, 100 a request")
}

// The PDU waits for the other target to publish first rather than taking it for missing
func TestStateDAGFillHoldsPDUsReachingOtherTargets(t *testing.T) {
	servers := newStateDAGServers(map[id.EventID][]id.EventID{"$base": nil, "$other": {"$base"}, "$served": {"$other"}}, "$base")
	servers.pending = map[id.EventID]bool{"$other": true}
	servers.pdu("$pdu", "$served")
	servers.pdu("$citing", "$pdu")
	servers.pdu("$connected", "$base")
	fill := servers.fill(t, 10_000, unlimited(), "origin.example")
	assert.Empty(t, fill.open(), "the other target is not missing")
	failures, held := fill.outcome()
	assert.Empty(t, failures)
	assert.Equal(t, map[id.EventID]struct{}{"$pdu": {}, "$citing": {}}, held)
}

// Hostile frontier events with the lowest IDs do not keep the others from being named
func TestStateDAGFillRotatesThroughTheFrontier(t *testing.T) {
	gap := &rooms.StateDAGGap{Cites: make(map[id.EventID][]id.EventID), Missing: map[id.EventID]struct{}{"$missing": {}}}
	for i := range MaxStateDAGRequestIDs + 50 {
		gap.Cites[id.EventID(fmt.Sprintf("$f%03d", i))] = []id.EventID{"$missing"}
	}
	fill := newStateDAGFill(nil, gap, 10_000, unlimited(), time.Now().Add(time.Hour))

	first := fill.nameFrontier()
	require.Len(t, first, MaxStateDAGRequestIDs)
	assert.Equal(t, id.EventID("$f000"), first[0])
	second := fill.nameFrontier()
	require.Len(t, second, MaxStateDAGRequestIDs)
	assert.Equal(t, id.EventID("$f100"), second[0], "the events not named yet go first")
	assert.Equal(t, id.EventID("$f000"), second[50], "then those named least")
	assert.Equal(t, id.EventID("$f050"), fill.nameFrontier()[0])
}
