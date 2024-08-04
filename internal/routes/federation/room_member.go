package federation

import (
	"encoding/json"
	"net/http"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type inviteRequest struct {
	Event       *types.Event   `json:"event"`
	InviteState []*types.Event `json:"invite_room_state"`
	RoomVersion string         `json:"room_version"`
}

// https://spec.matrix.org/v1.10/server-server-api/#put_matrixfederationv2inviteroomideventid
func (f *FederationRoutes) SignInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotJSON)
		return
	}

	req.Event.RoomVersion = req.RoomVersion
	req.Event.ID = util.EventIDFromRequestURLParam(r, "eventID")

	// Verify the event ID and signature
	verifyErr, err := util.VerifyEvent(r.Context(), req.Event, middleware.GetRequestServer(r), f.keyStore)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if verifyErr != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, verifyErr.Error())
		return
	}

	keyID, key := f.config.MustGetActiveSigningKey()
	signature, err := util.GetEventSignature(req.Event, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	req.Event.Signatures[f.config.ServerName] = map[string]string{
		keyID: signature,
	}

	// Store the event as an outlier membership, meaning we index it only for the
	// target of the invite not the room (if any) itself. If this server is in the
	// room already we'll get the full event over federation which will overwrite.
	if err := f.db.Rooms.SendFederatedOutlierMembershipEvent(r.Context(), req.Event); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Event *types.Event `json:"event"`
	}{req.Event})
}

func (f *FederationRoutes) MakeJoin(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

func (f *FederationRoutes) SendJoin(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
