package client

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidreceiptreceipttypeeventid
func (c *ClientRoutes) SendRoomReadReceipt(w http.ResponseWriter, r *http.Request) {
	roomID := util.RoomIDFromRequestURLParam(r, "roomID")
	eventID := util.EventIDFromRequestURLParam(r, "eventID")
	receiptType := chi.URLParam(r, "receiptType")

	rc := types.Receipt{
		UserID:  middleware.GetRequestUserID(r),
		RoomID:  roomID,
		Type:    event.ReceiptType(receiptType),
		EventID: eventID,
		// TODO: data
	}

	if res, err := c.db.Rooms.SendReceipts(r.Context(), roomID, []*types.Receipt{&rc}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if len(res.Rejected) > 0 {
		err := res.Rejected[0].Error
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, err.Error())
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
		return
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3roomsroomidread_markers
func (c *ClientRoutes) SendRoomReadMarkers(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}
