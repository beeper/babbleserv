package federation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stripped create of a state DAG room lacks prev_state_events, which must not fail the request
func TestInviteRequestTakesStrippedState(t *testing.T) {
	var req inviteRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"event": {"type": "m.room.member", "state_key": "@bob:hs1", "sender": "@alice:hs2", "content": {"membership": "invite"}},
		"invite_room_state": [{"type": "m.room.create", "state_key": "", "sender": "@alice:hs2", "content": {"room_version": "org.matrix.msc4242.12"}}],
		"room_version": "org.matrix.msc4242.12"
	}`), &req))
	assert.Len(t, req.InviteState, 1)
}

// An origin served as many as it may waits for a turn, refused only once its queue is full too
func TestOriginLimiter(t *testing.T) {
	limiter := newOriginLimiter(1, 1)
	ctx := context.Background()
	first, err := limiter.acquire(ctx, "a.example")
	require.NoError(t, err)
	_, err = limiter.acquire(ctx, "b.example")
	require.NoError(t, err, "each origin has its own turns")

	waited := make(chan func(), 1)
	go func() {
		release, err := limiter.acquire(ctx, "a.example")
		assert.NoError(t, err)
		waited <- release
	}()
	require.Eventually(t, func() bool {
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		return limiter.origins["a.example"].waiting == 1
	}, 5*time.Second, time.Millisecond)
	_, err = limiter.acquire(ctx, "a.example")
	assert.ErrorIs(t, err, errTooManyStateDAGJoins, "past the queue")

	first()
	first()
	second := <-waited
	gaveUp, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	_, err = limiter.acquire(gaveUp, "a.example")
	assert.ErrorIs(t, err, context.DeadlineExceeded, "a request waits only while it lasts")

	second()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	assert.NotContains(t, limiter.origins, "a.example", "an origin no request holds or waits for is forgotten")
}
