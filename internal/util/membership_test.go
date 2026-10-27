package util

import (
	"fmt"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/types"
)

func TestMembershipTemplateRefusesRestrictedJoinsToStateDAGRooms(t *testing.T) {
	template := func(content string) gomatrixserverlib.ProtoEvent {
		return gomatrixserverlib.ProtoEvent{
			SenderID:        "@alice:here",
			RoomID:          "!room:there",
			Type:            event.StateMember.Type,
			StateKey:        new("@alice:here"),
			Content:         []byte(content),
			PrevStateEvents: &[]string{"$create"},
		}
	}
	restricted := template(`{"membership":"join","join_authorised_via_users_server":"@charlie:there"}`)

	_, err := MembershipTemplateEvent("@alice:here", "!room:there", event.MembershipJoin, restricted, gomatrixserverlib.RoomVersionStateDAGs)
	assert.ErrorIs(t, err, ErrRestrictedJoinUnsupported)

	_, err = MembershipTemplateEvent("@alice:here", "!room:there", event.MembershipJoin, restricted, gomatrixserverlib.RoomVersionV12)
	assert.NoError(t, err, "other versions keep taking the template")

	ev, err := MembershipTemplateEvent("@alice:here", "!room:there", event.MembershipJoin, template(`{"membership":"join"}`), gomatrixserverlib.RoomVersionStateDAGs)
	require.NoError(t, err)
	assert.Equal(t, event.MembershipJoin, ev.Membership())
}

func TestMembershipTemplateRefusesStateDAGTemplatesOverTheCap(t *testing.T) {
	tooMany := make([]string, types.MaxStateDAGPrevEvents+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("$state%d", i)
	}
	template := gomatrixserverlib.ProtoEvent{
		SenderID:        "@alice:here",
		RoomID:          "!room:there",
		Type:            event.StateMember.Type,
		StateKey:        new("@alice:here"),
		Content:         []byte(`{"membership":"join"}`),
		PrevStateEvents: &tooMany,
	}

	_, err := MembershipTemplateEvent("@alice:here", "!room:there", event.MembershipJoin, template, gomatrixserverlib.RoomVersionStateDAGs)
	assert.Error(t, err)

	template.PrevStateEvents = nil
	_, err = MembershipTemplateEvent("@alice:here", "!room:there", event.MembershipJoin, template, gomatrixserverlib.RoomVersionStateDAGs)
	assert.Error(t, err, "a state DAG template must cite prev_state_events")
}
