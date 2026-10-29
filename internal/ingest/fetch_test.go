package ingest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func noDeadline() time.Time {
	return time.Now().Add(time.Hour)
}

func gapTarget(eventID id.EventID, origin string, prevIDs ...id.EventID) *types.IngestTarget {
	return &types.IngestTarget{Kind: types.IngestTargetPDU, Event: testPDU(eventID, prevIDs), Servers: []string{origin}}
}

type gapRequest struct {
	server    string
	latestIDs []id.EventID
	pulledIDs []id.EventID
}

// gapServers answers get_missing_events as Synapse does, ignoring latest events it does not know
type gapServers struct {
	before   map[string]map[id.EventID][]id.EventID
	errs     map[string]error
	requests []gapRequest
}

func (s *gapServers) missingEvents(_ context.Context, server string, latestIDs, pulledIDs []id.EventID) ([]*types.Event, error) {
	s.requests = append(s.requests, gapRequest{server, latestIDs, pulledIDs})
	if err := s.errs[server]; err != nil {
		return nil, err
	}
	var evs []*types.Event
	for _, latestID := range latestIDs {
		for _, eventID := range s.before[server][latestID] {
			evs = append(evs, testPDU(eventID, nil))
		}
	}
	return evs, nil
}

func pulledIDsOf(plan *gapPlan, server string) []id.EventID {
	var ids []id.EventID
	for _, ev := range plan.pulledFrom[server] {
		ids = append(ids, ev.ID)
	}
	return ids
}

func TestGapPlanAsksEachOriginForItsOwnPDUs(t *testing.T) {
	servers := &gapServers{before: map[string]map[id.EventID][]id.EventID{
		"a.example": {"$a": {"$beforeA"}},
		"b.example": {"$b": {"$beforeB"}},
	}}
	plan := newGapPlan([]*types.IngestTarget{
		gapTarget("$a", "a.example", "$beforeA"),
		gapTarget("$b", "b.example", "$beforeB"),
	}, []id.EventID{"$beforeA", "$beforeB"}, noDeadline())
	require.NoError(t, plan.run(context.Background(), []string{"a.example", "b.example", "c.example"}, servers.missingEvents))

	assert.Equal(t, []gapRequest{
		{"a.example", []id.EventID{"$a"}, nil},
		{"b.example", []id.EventID{"$b"}, []id.EventID{"$beforeA"}},
	}, servers.requests, "each origin is asked for its own PDUs, knowing what was pulled already")
	assert.Equal(t, []id.EventID{"$beforeA"}, pulledIDsOf(plan, "a.example"))
	assert.Equal(t, []id.EventID{"$beforeB"}, pulledIDsOf(plan, "b.example"))
	assert.Empty(t, plan.unanswered())
}

// Each origin is asked for its own PDUs' gaps, and only the first few for the others'
func TestGapPlanAsksEveryOrigin(t *testing.T) {
	servers := &gapServers{}
	var targets []*types.IngestTarget
	var origins []string
	for i := range maxGapFillServers + 2 {
		origin := fmt.Sprintf("server%d.example", i)
		origins = append(origins, origin)
		targets = append(targets, gapTarget(id.EventID(fmt.Sprintf("$pdu%d", i)), origin, "$missing"))
	}
	plan := newGapPlan(targets, []id.EventID{"$missing"}, noDeadline())
	require.NoError(t, plan.run(context.Background(), gapFillServers(origins, nil, "self.example"), servers.missingEvents))

	asked := make(map[string]int)
	for _, req := range servers.requests {
		asked[req.server]++
	}
	assert.Len(t, asked, maxGapFillServers+2, "every origin is asked")
	assert.Equal(t, 1, asked[origins[maxGapFillServers]], "an origin past the first few is asked for its own PDU alone")
	assert.Greater(t, asked[origins[0]], 1, "the first few are asked for the others' gaps too")
}

func TestGapPlanAsksOtherServersForGapsStillOpen(t *testing.T) {
	servers := &gapServers{before: map[string]map[id.EventID][]id.EventID{
		"b.example": {"$a": {"$beforeA"}},
	}}
	plan := newGapPlan([]*types.IngestTarget{gapTarget("$a", "a.example", "$beforeA")}, []id.EventID{"$beforeA"}, noDeadline())
	require.NoError(t, plan.run(context.Background(), []string{"a.example", "b.example"}, servers.missingEvents))

	assert.Equal(t, []gapRequest{
		{"a.example", []id.EventID{"$a"}, nil},
		{"b.example", []id.EventID{"$a"}, nil},
	}, servers.requests, "the origin answering without the events, the next server is asked")
	assert.Equal(t, []id.EventID{"$beforeA"}, pulledIDsOf(plan, "b.example"))
}

func TestGapPlanSplitsRequests(t *testing.T) {
	servers := &gapServers{before: map[string]map[id.EventID][]id.EventID{
		"a.example": {"$p1": {"$m1"}, "$p2": {"$m2"}, "$p4": {"$m4"}, "$p5": {"$m5"}},
	}}
	plan := newGapPlan([]*types.IngestTarget{
		gapTarget("$p1", "a.example", "$m1"),
		gapTarget("$p2", "a.example", "$m2"),
		gapTarget("$p3", "a.example", "$m1"),
		gapTarget("$p4", "a.example", "$m4"),
		gapTarget("$p5", "a.example", "$m5"),
	}, []id.EventID{"$m1", "$m2", "$m4", "$m5"}, noDeadline())
	require.NoError(t, plan.run(context.Background(), []string{"a.example"}, servers.missingEvents))

	var latest [][]id.EventID
	for _, request := range servers.requests {
		latest = append(latest, request.latestIDs)
	}
	assert.Equal(t, [][]id.EventID{{"$p1", "$p2"}, {"$p4", "$p5"}}, latest,
		"a few PDUs a request, skipping one an earlier request filled the gap of")
}

func TestGapPlanUnanswered(t *testing.T) {
	timeout := mautrix.HTTPError{Message: "request error", WrappedError: context.DeadlineExceeded}
	refused := mautrix.HTTPError{Response: &http.Response{StatusCode: http.StatusNotFound}}
	targets := []*types.IngestTarget{
		gapTarget("$a", "a.example", "$beforeA"),
		gapTarget("$a2", "a.example", "$beforeA2"),
	}
	run := func(servers *gapServers) *gapPlan {
		plan := newGapPlan(targets, []id.EventID{"$beforeA", "$beforeA2"}, noDeadline())
		require.NoError(t, plan.run(context.Background(), []string{"a.example", "b.example"}, servers.missingEvents))
		return plan
	}

	servers := &gapServers{errs: map[string]error{"a.example": timeout}}
	plan := run(servers)
	assert.Equal(t, map[id.EventID]struct{}{"$a": {}, "$a2": {}}, plan.unanswered(),
		"the origin timing out leaves the gaps for a later round, though the other server answered")
	assert.Len(t, servers.requests, 2, "a server failing is not asked again")
	assert.Len(t, plan.errs, 1)

	servers = &gapServers{
		errs:   map[string]error{"a.example": timeout},
		before: map[string]map[id.EventID][]id.EventID{"b.example": {"$a": {"$beforeA"}}},
	}
	assert.Equal(t, map[id.EventID]struct{}{"$a2": {}}, run(servers).unanswered(), "a gap filled elsewhere is done")

	servers = &gapServers{errs: map[string]error{"a.example": fmt.Errorf("wrapped: %w", refused), "b.example": refused}}
	assert.Empty(t, run(servers).unanswered(), "gaps every server refused are not asked again")
}

func TestGapPlanStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	plan := newGapPlan([]*types.IngestTarget{gapTarget("$a", "a.example", "$beforeA")}, []id.EventID{"$beforeA"}, noDeadline())
	err := plan.run(ctx, []string{"a.example", "b.example"}, func(context.Context, string, []id.EventID, []id.EventID) ([]*types.Event, error) {
		cancel()
		return nil, context.Canceled
	})
	assert.ErrorIs(t, err, context.Canceled)
}

// PDUs reaching an unfetched prev event through pulled events are held back, as those events would be dropped
func TestReachingPrevs(t *testing.T) {
	pulled := []*types.Event{
		testPDU("$pulledOld", []id.EventID{"$unfetched"}),
		testPDU("$pulledNew", []id.EventID{"$pulledOld"}),
		testPDU("$pulledFetched", []id.EventID{"$fetched"}),
	}
	pdus := []*types.Event{
		testPDU("$pdu", []id.EventID{"$pulledNew"}),
		testPDU("$citesPDU", []id.EventID{"$pdu"}),
		testPDU("$other", []id.EventID{"$pulledFetched"}),
	}
	assert.Equal(t, map[id.EventID]struct{}{"$pdu": {}, "$citesPDU": {}}, reachingPrevs(pdus, pulled, map[id.EventID]struct{}{"$unfetched": {}}))
	assert.Empty(t, reachingPrevs(pdus, pulled, nil))
}

// A fill past its deadline, or a server asked as often as it may be, leaves the open PDUs held back
func TestGapPlanBounds(t *testing.T) {
	var targets []*types.IngestTarget
	var missing []id.EventID
	for i := range 2 * (maxGapRequestsPerServer + 1) * gapPDUsPerRequest {
		missingID := id.EventID(fmt.Sprintf("$missing%d", i))
		missing = append(missing, missingID)
		targets = append(targets, gapTarget(id.EventID(fmt.Sprintf("$pdu%d", i)), "a.example", missingID))
	}

	servers := &gapServers{}
	plan := newGapPlan(targets, missing, noDeadline())
	require.NoError(t, plan.run(context.Background(), []string{"a.example", "b.example"}, servers.missingEvents))
	asked := make(map[string]int)
	for _, req := range servers.requests {
		asked[req.server]++
	}
	assert.Equal(t, map[string]int{"a.example": maxGapRequestsPerServer, "b.example": maxGapRequestsPerServer}, asked)
	unanswered := plan.unanswered()
	assert.Len(t, unanswered, len(targets)-maxGapRequestsPerServer*gapPDUsPerRequest, "PDUs no server was asked about are held back")
	assert.NotContains(t, unanswered, id.EventID("$pdu0"), "one every server asked answered for is not")
	assert.ErrorIs(t, errors.Join(plan.errs...), errGapFillBounds)

	servers = &gapServers{}
	plan = newGapPlan(targets[:1], missing[:1], time.Now())
	require.NoError(t, plan.run(context.Background(), []string{"a.example"}, servers.missingEvents))
	assert.Empty(t, servers.requests, "no request starts past the deadline")
	assert.Equal(t, map[id.EventID]struct{}{"$pdu0": {}}, plan.unanswered())
}
