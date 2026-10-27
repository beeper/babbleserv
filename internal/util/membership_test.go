package util

import (
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
)

func TestMembershipTemplateRefusesRestrictedJoinsToStateDAGRooms(t *testing.T) {
	template := func(content string) gomatrixserverlib.ProtoEvent {
		return gomatrixserverlib.ProtoEvent{
			SenderID: "@alice:here",
			RoomID:   "!room:there",
			Type:     event.StateMember.Type,
			StateKey: new("@alice:here"),
			Content:  []byte(content),
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
