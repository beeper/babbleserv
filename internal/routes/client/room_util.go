package client

import (
	"context"
	"net/http"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) sendLocalEventHandleResults(
	w http.ResponseWriter,
	r *http.Request,
	roomID id.RoomID,
	partialEv *types.PartialEvent,
	responseGen func(ev *types.Event) any,
) {
	res, err := c.db.Rooms.SendLocalEvents(r.Context(), roomID, []*types.PartialEvent{partialEv}, rooms.SendLocalEventsOptions{})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(res.Rejected) > 0 {
		err := res.Rejected[0].Error
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
		return
	} else {
		ev := res.Allowed[0]
		util.ResponseJSON(w, r, http.StatusOK, responseGen(ev))
		return
	}
}

// https://spec.matrix.org/v1.11/server-server-api/#inviting-to-a-room
func (c *ClientRoutes) prepareAndSendInviteForRemoteUser(
	ctx context.Context,
	roomID id.RoomID,
	otherUserID id.UserID,
	partialEv *types.PartialEvent,
) (*rooms.SendEventsResult, *mautrix.RespError, error) {
	// Start by preparing the event, this also auths it against local state
	evs, evErr, err := c.db.Rooms.PrepareLocalEvents(ctx, []*types.PartialEvent{partialEv})
	if err != nil {
		return nil, nil, err
	} else if evErr != nil {
		return nil, &mautrix.MForbidden, evErr
	}
	ev := evs[0]

	inviteStateEvs, err := c.db.Rooms.GetCurrentRoomInviteStateEvents(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	strippedPDUs := make([]gomatrixserverlib.InviteStrippedState, 0, len(inviteStateEvs))
	for _, stateEv := range inviteStateEvs {
		strippedPDUs = append(strippedPDUs, gomatrixserverlib.NewInviteStrippedState(stateEv.PDU()))
	}
	inviteReq, err := fclient.NewInviteV2Request(ev.PDU(), strippedPDUs)
	if err != nil {
		return nil, nil, err
	}

	// Switch to a background context here - if the client drops the request
	// we should still send/receive the invite so the state on the remote HS
	// and local don't end up diverged.
	backgroundCtx := zerolog.Ctx(ctx).With().
		Str("background_task", "SendFederatedInvite").
		Logger().
		WithContext(context.Background())

	otherHomeserver := otherUserID.Homeserver()
	inviteResp, err := c.fclient.SendInviteV2(
		backgroundCtx,
		spec.ServerName(c.config.ServerName),
		spec.ServerName(otherHomeserver),
		inviteReq,
	)
	if err != nil {
		return nil, nil, err
	}

	// Grab the signature, inject into our event for verification
	escapedHS := strings.Replace(otherHomeserver, ".", "\\.", -1)
	signatures := gjson.GetBytes(inviteResp.Event, "signatures."+escapedHS).Value()
	ev.Signatures[otherHomeserver] = make(map[string]string, 1)
	for k, v := range signatures.(map[string]any) {
		ev.Signatures[otherHomeserver][k] = v.(string)
	}

	verifyErr, err := util.VerifyEvent(backgroundCtx, ev, otherHomeserver, c.keyStore)
	if err != nil {
		return nil, nil, err
	} else if verifyErr != nil {
		return nil, &mautrix.MInvalidParam, verifyErr
	}

	// Now that we've prepared, other HS signed and we verified the event we
	// can send it. We send it as if it's a federated event which triggers
	// all the authorization checks, accounting for any state changes in
	// the room during the signing process above.
	results, err := c.db.Rooms.SendFederatedEvents(backgroundCtx, roomID, []*types.Event{ev}, rooms.SendFederatedEventsOptions{})
	if err != nil {
		return nil, nil, err
	}

	if len(results.Rejected) > 0 {
		err := results.Rejected[0].Error
		return nil, &mautrix.MForbidden, err
	}

	return results, nil, nil
}
