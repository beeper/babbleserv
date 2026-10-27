package ingest

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func gapTarget(eventID id.EventID, origin string, prevIDs ...id.EventID) *types.IngestTarget {
	return &types.IngestTarget{Kind: types.IngestTargetPDU, Event: testPDU(eventID, prevIDs), Servers: []string{origin}}
}

type gapRequest struct {
	server    string
	latestIDs []id.EventID
	pulledIDs []id.EventID
}

// gapServers answers get_missing_events as Synapse does, with the events it holds before the latest
// events it knows, ignoring the others
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
	}, []id.EventID{"$beforeA", "$beforeB"})
	require.NoError(t, plan.run(context.Background(), []string{"a.example", "b.example", "c.example"}, servers.missingEvents))

	assert.Equal(t, []gapRequest{
		{"a.example", []id.EventID{"$a"}, nil},
		{"b.example", []id.EventID{"$b"}, []id.EventID{"$beforeA"}},
	}, servers.requests, "each origin is asked for its own PDUs, knowing what was pulled already")
	assert.Equal(t, []id.EventID{"$beforeA"}, pulledIDsOf(plan, "a.example"))
	assert.Equal(t, []id.EventID{"$beforeB"}, pulledIDsOf(plan, "b.example"))
	assert.Empty(t, plan.unanswered())
}

func TestGapPlanAsksOtherServersForGapsStillOpen(t *testing.T) {
	servers := &gapServers{before: map[string]map[id.EventID][]id.EventID{
		"b.example": {"$a": {"$beforeA"}},
	}}
	plan := newGapPlan([]*types.IngestTarget{gapTarget("$a", "a.example", "$beforeA")}, []id.EventID{"$beforeA"})
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
	}, []id.EventID{"$m1", "$m2", "$m4", "$m5"})
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
		plan := newGapPlan(targets, []id.EventID{"$beforeA", "$beforeA2"})
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
	plan := newGapPlan([]*types.IngestTarget{gapTarget("$a", "a.example", "$beforeA")}, []id.EventID{"$beforeA"})
	err := plan.run(ctx, []string{"a.example", "b.example"}, func(context.Context, string, []id.EventID, []id.EventID) ([]*types.Event, error) {
		cancel()
		return nil, context.Canceled
	})
	assert.ErrorIs(t, err, context.Canceled)
}
