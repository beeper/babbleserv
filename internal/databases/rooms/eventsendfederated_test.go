package rooms

import (
	"encoding/json"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func outlierMemberEvent(eventID id.EventID, roomVersion string, sender id.UserID, membership event.Membership, prevStateEventIDs ...id.EventID) *types.Event {
	ev := &types.Event{ID: eventID, RoomVersion: roomVersion, PrevStateEventIDs: prevStateEventIDs}
	ev.RoomID, ev.Type, ev.Sender, ev.StateKey = sendTestRoomID, event.StateMember, sender, new("@alice:here")
	ev.Content, _ = json.Marshal(map[string]event.Membership{"membership": membership})
	return ev
}

func TestEndsHeldMembership(t *testing.T) {
	stateDAGs := string(gomatrixserverlib.RoomVersionStateDAGs)
	for name, test := range map[string]struct {
		held, leave *types.Event
		ends        bool
	}{
		"the inviter rescinds an invite": {
			outlierMemberEvent("$invite", "12", "@charlie:there", event.MembershipInvite),
			outlierMemberEvent("$leave", "12", "@charlie:there", event.MembershipLeave),
			true,
		},
		"another user rescinds an invite": {
			outlierMemberEvent("$invite", "12", "@charlie:there", event.MembershipInvite),
			outlierMemberEvent("$leave", "12", "@mallory:there", event.MembershipLeave),
			false,
		},
		"a knock is rejected outside a state DAG room": {
			outlierMemberEvent("$knock", "12", "@alice:here", event.MembershipKnock),
			outlierMemberEvent("$leave", "12", "@charlie:there", event.MembershipLeave),
			false,
		},
		"the inviter rescinds an invite citing it in a state DAG room": {
			outlierMemberEvent("$invite", stateDAGs, "@charlie:there", event.MembershipInvite, "$power"),
			outlierMemberEvent("$leave", stateDAGs, "@charlie:there", event.MembershipLeave, "$topic", "$invite"),
			true,
		},
		"the inviter rescinds an invite not citing it in a state DAG room": {
			outlierMemberEvent("$invite", stateDAGs, "@charlie:there", event.MembershipInvite, "$power"),
			outlierMemberEvent("$leave", stateDAGs, "@charlie:there", event.MembershipLeave, "$power"),
			false,
		},
		"another user rescinds an invite citing it in a state DAG room": {
			outlierMemberEvent("$invite", stateDAGs, "@charlie:there", event.MembershipInvite, "$power"),
			outlierMemberEvent("$leave", stateDAGs, "@mallory:there", event.MembershipLeave, "$invite"),
			false,
		},
		"a knock rejected citing it in a state DAG room, as Synapse takes none": {
			outlierMemberEvent("$knock", stateDAGs, "@alice:here", event.MembershipKnock, "$power"),
			outlierMemberEvent("$leave", stateDAGs, "@charlie:there", event.MembershipLeave, "$knock"),
			false,
		},
		"a leave the user holds already ends nothing": {
			outlierMemberEvent("$rejected", stateDAGs, "@alice:here", event.MembershipLeave, "$invite"),
			outlierMemberEvent("$leave", stateDAGs, "@charlie:there", event.MembershipLeave, "$rejected"),
			false,
		},
	} {
		assert.Equal(t, test.ends, endsHeldMembership(test.leave, test.held), name)
	}
}
