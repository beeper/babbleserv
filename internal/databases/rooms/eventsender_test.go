package rooms

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/eventsendutil"
	"github.com/beeper/babbleserv/internal/types"
)

const sendTestRoomID = id.RoomID("!room:example.com")

func sendTestMember(eventID id.EventID, userID id.UserID) *types.Event {
	ev := &types.Event{ID: eventID}
	ev.RoomID, ev.Type, ev.StateKey = sendTestRoomID, event.StateMember, new(userID.String())
	ev.Content = []byte(`{"membership":"join"}`)
	return ev
}

// Send attempts and publishing

// sendRecorder fakes the steps of a send, failing a step that runs with the room's mutex held when
// it must not be, or released when it must be held
type sendRecorder struct {
	t         *testing.T
	locked    bool
	prepares  []func() (preparationOutcome, error)
	publishes []func() (*SendEventsResult, error)
	built     []preparationWork
	pauses    []int
	after     []*SendEventsResult
}

func (s *sendRecorder) sender() *eventSender {
	return &eventSender{
		lock: func() func() {
			require.False(s.t, s.locked, "the mutex is taken twice")
			s.locked = true
			return func() { s.locked = false }
		},
		publish: func(context.Context, *publishPlan) (*SendEventsResult, error) {
			assert.True(s.t, s.locked, "publish holds the mutex")
			next := s.publishes[0]
			s.publishes = s.publishes[1:]
			return next()
		},
		build: func(_ context.Context, work preparationWork) error {
			assert.False(s.t, s.locked, "staging and artifact jobs run without the mutex")
			s.built = append(s.built, work)
			return nil
		},
		after: func(_ context.Context, res *SendEventsResult) {
			assert.False(s.t, s.locked, "after runs without the mutex")
			s.after = append(s.after, res)
		},
		pause: func(ctx context.Context, failures int) error {
			assert.False(s.t, s.locked, "the pause runs without the mutex")
			s.pauses = append(s.pauses, failures)
			return ctx.Err()
		},
	}
}

func (s *sendRecorder) prepare(context.Context) (preparationOutcome, error) {
	assert.True(s.t, s.locked, "prepare holds the mutex")
	next := s.prepares[0]
	s.prepares = s.prepares[1:]
	return next()
}

func preparedAs(outcome preparationOutcome) func() (preparationOutcome, error) {
	return func() (preparationOutcome, error) { return outcome, nil }
}

func plannedTo(plan *publishPlan) func() (preparationOutcome, error) {
	return preparedAs(readyToPublish{plan: plan})
}

func publishedAs(res *SendEventsResult, err error) func() (*SendEventsResult, error) {
	return func() (*SendEventsResult, error) { return res, err }
}

func TestSendStartsAgainAfterAGuardFailure(t *testing.T) {
	published := &SendEventsResult{}
	s := &sendRecorder{
		t:        t,
		prepares: []func() (preparationOutcome, error){plannedTo(&publishPlan{}), plannedTo(&publishPlan{}), plannedTo(&publishPlan{})},
		publishes: []func() (*SendEventsResult, error){
			publishedAs(nil, fmt.Errorf("failed to publish events: %w", errEventsStoredConcurrently)),
			publishedAs(nil, fmt.Errorf("%w: state revision 3, prepared at 2", errGuardFailed)),
			publishedAs(published, nil),
		},
	}
	res, err := s.sender().run(context.Background(), s.prepare)
	require.NoError(t, err)
	assert.Same(t, published, res)
	assert.Equal(t, []int{1, 2}, s.pauses, "each guard failure pauses longer")
	assert.Equal(t, []*SendEventsResult{published}, s.after)
	assert.Empty(t, s.prepares)
	assert.False(t, s.locked)
}

func TestAttemptEventsLeaveTheEventsGivenAsTheyAre(t *testing.T) {
	s := &eventSender{r: &RoomsDatabase{}}
	given := sendTestMember("$join", "@alice:example.com")
	given.Unsigned = map[string]any{"invite_room_state": []any{}}
	copies := s.attemptEvents([]*types.Event{given})
	copies[0].Rejected, copies[0].SoftFailed, copies[0].IsDuplicate = true, true, true
	copies[0].BeforeState, copies[0].AfterState = types.StateHash{1}, types.StateHash{2}
	copies[0].SetUnsigned("prev_content", map[string]any{})
	s.r.stripEventUnsigned(copies[0])

	assert.False(t, given.Rejected || given.SoftFailed || given.IsDuplicate)
	assert.True(t, given.BeforeState.IsZero() && given.AfterState.IsZero())
	assert.Equal(t, map[string]any{"invite_room_state": []any{}}, given.Unsigned)

	attempted := *given
	attempted.Rejected, attempted.AfterState = true, types.StateHash{2}
	again := s.attemptEvents([]*types.Event{&attempted})[0]
	assert.False(t, again.Rejected, "an attempt starts without another's outcome")
	assert.True(t, again.AfterState.IsZero())
}

// Staging

func TestChunkStatelessEvents(t *testing.T) {
	s := &federatedEventSender{eventSender: eventSender{r: &RoomsDatabase{}}}
	var evs []*types.Event
	for i := range 40 {
		ev := sendTestMember(id.EventID(fmt.Sprintf("$join%d", i)), id.UserID(fmt.Sprintf("@user%d:example.com", i)))
		ev.Content = []byte(`{"membership":"join"}`)
		if i%7 == 3 {
			ev.Content = []byte(`{"membership":"join","displayname":"` + strings.Repeat("x", 3000) + `"}`)
		}
		evs = append(evs, ev)
	}
	budget := 3 * s.stagedEventBytes(evs[0])
	require.Greater(t, s.stagedEventBytes(evs[3]), budget)

	s.r.config.Rooms.StateBudget.StagingBytes = budget
	chunks := s.chunkStatelessEvents(evs)
	assert.Equal(t, evs, slices.Concat(chunks...), "events keep their order")
	for i, chunk := range chunks {
		require.NotEmpty(t, chunk)
		size := 0
		for _, ev := range chunk {
			size += s.stagedEventBytes(ev)
		}
		if len(chunk) > 1 {
			assert.LessOrEqual(t, size, budget, "chunk %d", i)
		}
		if i+1 < len(chunks) {
			assert.Greater(t, size+s.stagedEventBytes(chunks[i+1][0]), budget, "chunk %d is not full", i)
		}
	}

	assert.Empty(t, s.chunkStatelessEvents(nil))
	s.r.config.Rooms.StateBudget.StagingBytes = 1 << 30
	assert.Len(t, s.chunkStatelessEvents(evs), 1)
}

// Artifact jobs

func TestSendRetriesAnArtifactWhoseBuildConflicted(t *testing.T) {
	work := resolveStateWork{request: &eventsendutil.ResolutionRequest{Contexts: []types.StateHash{{1}, {2}}}}
	published := &SendEventsResult{}
	s := &sendRecorder{
		t: t,
		prepares: []func() (preparationOutcome, error){
			preparedAs(needsWork{work: work}), preparedAs(needsWork{work: work}), plannedTo(&publishPlan{}),
		},
		publishes: []func() (*SendEventsResult, error){publishedAs(published, nil)},
	}
	sender := s.sender()
	build := sender.build
	attempts := 0
	sender.build = func(ctx context.Context, work preparationWork) error {
		assert.False(t, s.locked)
		attempts++
		if attempts == 1 {
			return errEventsStoredConcurrently
		}
		return build(ctx, work)
	}
	res, err := sender.run(context.Background(), s.prepare)
	require.NoError(t, err)
	assert.Same(t, published, res)
	assert.Equal(t, 2, attempts, "a failed job is not marked complete")
	assert.Equal(t, []int{1}, s.pauses)
	assert.Len(t, s.built, 1)
}
