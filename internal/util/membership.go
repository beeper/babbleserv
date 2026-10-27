package util

import (
	"errors"
	"fmt"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// The resident co-signs a restricted join in send_join, but this server would publish its own copy
// without that signature, which other servers fetching the state DAG from here drop
var ErrRestrictedJoinUnsupported = errors.New("restricted joins to state DAG rooms are not supported")

// MembershipTemplateEvent checks a make_* template is what was asked for, as Synapse does
func MembershipTemplateEvent(
	userID id.UserID,
	roomID id.RoomID,
	membership event.Membership,
	template gomatrixserverlib.ProtoEvent,
	roomVersion gomatrixserverlib.RoomVersion,
) (*types.Event, error) {
	roomSpec, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return nil, err
	}
	ev, err := types.EventFromProtoEvent(template)
	if err != nil {
		return nil, err
	} else if ev.Type != event.StateMember || ev.StateKey == nil || *ev.StateKey != userID.String() ||
		ev.Sender != userID || ev.RoomID != roomID {
		return nil, fmt.Errorf("not a member event of %s in %s", userID, roomID)
	}
	if !gjson.ParseBytes(ev.Content).IsObject() {
		return nil, errors.New("content is not an object")
	} else if membership == event.MembershipJoin && roomSpec.StateDAGs() && gjson.GetBytes(ev.Content, "join_authorised_via_users_server").Exists() {
		return nil, ErrRestrictedJoinUnsupported
	} else if ev.Content, err = sjson.SetBytes(ev.Content, "membership", membership); err != nil {
		return nil, err
	}
	ev.Timestamp = time.Now().UTC().UnixMilli()
	ev.RoomVersion = string(roomVersion)
	return ev, nil
}
