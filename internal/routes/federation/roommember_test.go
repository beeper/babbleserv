package federation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stripped state is no PDU, so a stripped create of a state DAG room, without prev_state_events,
// does not fail the request
func TestInviteRequestTakesStrippedState(t *testing.T) {
	var req inviteRequest
	require.NoError(t, json.Unmarshal([]byte(`{
		"event": {"type": "m.room.member", "state_key": "@bob:hs1", "sender": "@alice:hs2", "content": {"membership": "invite"}},
		"invite_room_state": [{"type": "m.room.create", "state_key": "", "sender": "@alice:hs2", "content": {"room_version": "org.matrix.msc4242.12"}}],
		"room_version": "org.matrix.msc4242.12"
	}`), &req))
	assert.Len(t, req.InviteState, 1)
}
