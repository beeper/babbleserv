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

// MembershipTemplateEvent holds a make_* template to what was asked for and sets the membership
// asked for, as Synapse does
func MembershipTemplateEvent(
	userID id.UserID,
	roomID id.RoomID,
	membership event.Membership,
	template gomatrixserverlib.ProtoEvent,
	roomVersion gomatrixserverlib.RoomVersion,
) (*types.Event, error) {
	if _, err := gomatrixserverlib.GetRoomVersion(roomVersion); err != nil {
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
	} else if ev.Content, err = sjson.SetBytes(ev.Content, "membership", membership); err != nil {
		return nil, err
	}
	ev.Timestamp = time.Now().UTC().UnixMilli()
	ev.RoomVersion = string(roomVersion)
	return ev, nil
}
