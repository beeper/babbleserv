package eventsendutil

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/stateres"
	"github.com/beeper/babbleserv/internal/types"
)

func TestResolutionCandidatesAreLimitedOnceEach(t *testing.T) {
	contexts := []types.StateHash{{1}, {2}}
	cr := NewResolver(nil, nil, nil, ResolverOptions{MaxCandidates: 3})
	conflicted := []id.EventID{"$a", "$b", "$a", "$b"}
	candidates, request := cr.limitCandidates(contexts, conflicted, []id.EventID{"$c"})
	require.Nil(t, request)
	assert.Equal(t, []id.EventID{"$a", "$b", "$c"}, candidates)
	candidates, request = cr.limitCandidates(contexts, candidates, conflicted)
	require.Nil(t, request)
	assert.Len(t, candidates, 3, "a v12 subgraph holds the conflicted events again")
	candidates, request = cr.limitCandidates(contexts, conflicted, []id.EventID{"$c", "$d"})
	require.NotNil(t, request)
	assert.Nil(t, candidates)
	assert.Equal(t, contexts, request.Contexts)
	assert.Equal(t, 4, request.Candidates)
	cr.maxCandidates = 0
	candidates, request = cr.limitCandidates(contexts, conflicted, []id.EventID{"$c", "$d"})
	require.Nil(t, request)
	assert.Len(t, candidates, 4)
}

func TestResolutionOfLargeStatesIsLeftToAJob(t *testing.T) {
	contexts := []types.StateHash{{1}, {2}}
	counts := map[types.StateHash]int{{1}: 10, {2}: 11}
	count := func(ctx types.StateHash) (int, error) { return counts[ctx], nil }
	cr := NewResolver(nil, nil, nil, ResolverOptions{MaxStateTuples: 11})
	request, err := cr.checkStateTuples(contexts, count)
	require.NoError(t, err)
	assert.Nil(t, request)
	cr.maxStateTuples = 10
	request, err = cr.checkStateTuples(contexts, count)
	require.NoError(t, err)
	require.NotNil(t, request)
	assert.Equal(t, contexts, request.Contexts)
	assert.Equal(t, 11, request.StateTuples)
	assert.Equal(t, (&ResolutionRequest{Contexts: contexts, Candidates: 3}).Key(), request.Key(), "the job's stored result serves either limit")
	cr.maxStateTuples = 0
	request, err = cr.checkStateTuples(contexts, count)
	require.NoError(t, err)
	assert.Nil(t, request, "a job reads states of any size")
	failed := errors.New("count failed")
	cr.maxStateTuples = 10
	request, err = cr.checkStateTuples(contexts, func(types.StateHash) (int, error) { return 0, failed })
	assert.ErrorIs(t, err, failed)
	assert.Nil(t, request)
}

func TestResolvingWithoutStateResV2FailsBeforeReading(t *testing.T) {
	contexts := []types.StateHash{{1}, {2}}
	cr := NewResolver(nil, nil, nil, ResolverOptions{RoomVersion: "1", MaxCandidates: 1})
	result, request, err := cr.resolveConflicts(contexts, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, request)
	assert.Equal(t, contexts[0], result)
	_, request, err = cr.resolveContexts(contexts)
	assert.ErrorIs(t, err, stateres.ErrUnsupportedAlgorithm)
	assert.Nil(t, request)
}

func TestResolutionRequestsWorkBeforeReadingCandidateEvents(t *testing.T) {
	contexts := []types.StateHash{{1}, {2}}
	cr := NewResolver(nil, nil, nil, ResolverOptions{RoomVersion: "11", MaxCandidates: 1})
	result, request, err := cr.resolveConflicts(contexts, map[types.StateTup][]id.EventID{
		{Type: event.StateTopic}: {"$a", "$b"},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, request, "resolution can defer without any transaction or event provider")
	assert.True(t, result.IsZero(), "an unfinished resolution has no state result")
	assert.Equal(t, contexts, request.Contexts)
	assert.Equal(t, 2, request.Candidates)
}
