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

// stateDAGServers serve a state DAG, of which this server publishes some events and stores others
// without state, walking it as a babbleserv answers get_missing_events with the MSC's flag. Events
// cited that it does not know are withheld.
type stateDAGServers struct {
	cites     map[id.EventID][]id.EventID
	published map[id.EventID]bool
	stored    map[id.EventID]bool
	pdus      []*types.Event
	// Other targets of the job, which the walk does not walk into
	pending map[id.EventID]bool
	// The most events a server answers with
	maxEvents int
	// State events a server gives with each response that nothing cites
	junk     int
	errs     map[string]error
	requests []stateDAGRequestMade
}

func newStateDAGServers(cites map[id.EventID][]id.EventID, published ...id.EventID) *stateDAGServers {
	s := &stateDAGServers{cites: cites, published: make(map[id.EventID]bool), stored: make(map[id.EventID]bool), maxEvents: maxStateDAGLimit}
	for _, eventID := range append(published, "$createhash") {
		s.published[eventID] = true
	}
	return s
}

// chain is a state DAG of n events, each citing the one before, the first citing $base
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
	fill := newStateDAGFill(s.pdus, gap, maxPerPDU, budget)
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
		assert.LessOrEqual(t, len(req.latestIDs), maxStateDAGRequestIDs)
	}
	assert.Empty(t, fill.open(), "the limit, which counts events in all, grows until the gaps are filled")
}

// A server capping its responses gives the events nearest the PDUs again, so the requests walk on
// from the events reached that cite missing ones
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

// A server with nothing more to give is left once the frontier gives nothing either, and the next
// asked: a PDU's origin first, then the room's other servers
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

// A PDU fails once the requests for it staged more than a PDU's gap fill may, and a run's gap fills
// stop once they staged all it may. No request stages more than one past what they may.
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
	err = newStateDAGFill(servers.pdus, gap, 10_000, &budget).run(context.Background(), []string{"origin.example"}, servers.request, servers.walk)
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

// The served events citing the join, or another left out so, are newer than the join and arrive as
// PDUs instead of being staged
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

// A server refusing a join another server answered, so accepted, neither fails nor replaces it: the
// join is sent again after a pause
func TestSendFailedAfterAServerAnswered(t *testing.T) {
	var cfg config.BabbleConfig
	cfg.Rooms.Ingest.JoinTimeout = time.Hour
	w := &joinWork{
		run:    &jobRun{jobs: &Jobs{config: cfg}},
		target: &types.IngestTarget{Kind: types.IngestTargetJoin, Event: testTarget(types.IngestTargetJoin, "$join", 0).Event, Enqueued: time.Now().UnixMilli()},
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
	large := strings.Repeat("x", 2*maxStateDAGValueBytes)
	_, err := decodeChunks(t, `{"state_dag":[{"event_id":"$0","content":{"body":"`+large+`"}}]}`, 10, 1<<30)
	assert.ErrorIs(t, err, errBadStateDAGResponse, "an event")
	_, err = decodeChunks(t, `{"origin":"`+large+`","state_dag":[]}`, 10, 1<<30)
	assert.ErrorIs(t, err, errBadStateDAGResponse, "a member skipped")

	half := large[:maxStateDAGValueBytes/2]
	chunks, err := decodeChunks(t, `{"origin":"`+half+`","state_dag":[{"event_id":"$0","content":{"body":"`+half+`"}},{"event_id":"$1"}]}`, 10, 1<<30)
	require.NoError(t, err, "each value is bounded on its own")
	assert.Equal(t, [][]string{{"$0", "$1"}}, chunks)
}

// Events a server gives that do not fill the gap, which it could give endlessly, count against the
// bounds but give it no more requests
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

// A server giving the gap an event at a time is asked a bounded number of rounds, then left
func TestStateDAGFillBoundsTheRoundsAServerIsAsked(t *testing.T) {
	servers := newStateDAGServers(chain(99), "$base")
	servers.maxEvents = 1
	servers.pdu("$pdu", "$s99")
	fill := servers.fill(t, 1000, unlimited(), "origin.example")
	assert.Len(t, servers.requests, stateDAGRounds(1000))
	assert.Equal(t, 10, stateDAGRounds(1000), "7 growing the limit, 2 at 1,000, 1 from the frontier")
	assert.NotEmpty(t, fill.open())
	failures, held := fill.outcome()
	assert.Equal(t, unfilledGapFailure(), failures["$pdu"])
	assert.Empty(t, held)
}

// A PDU reaching another target of the job, through events served, is held back for it to publish
// first, rather than taking the target for missing
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
