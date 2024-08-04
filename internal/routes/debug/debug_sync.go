package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugSyncUser(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	from, err := util.VersionFromRequestQuery(r, "since", "r")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	limit, err := util.IntFromRequestQuery(r, "limit", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	options := rooms.SyncOptions{
		From:  from,
		Limit: limit,
	}

	nextVersion, rooms, err := b.db.Rooms.SyncRoomEventsForUser(r.Context(), userID, options)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	nextBatch := util.Base64EncodeURLSafe(types.ValueForVersionstamp(nextVersion))

	util.ResponseJSON(w, r, http.StatusOK, struct {
		NextBatch string
		Rooms     map[types.MembershipTup][]*types.Event
	}{nextBatch, rooms})
}

func (b *DebugRoutes) DebugSyncServer(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")

	from, err := util.VersionFromRequestQuery(r, "since", "r")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	limit, err := util.IntFromRequestQuery(r, "limit", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	options := rooms.SyncOptions{
		From:  from,
		Limit: limit,
	}

	nextVersion, rooms, err := b.db.Rooms.SyncRoomEventsForServer(r.Context(), serverName, options)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	nextBatch := util.Base64EncodeURLSafe(types.ValueForVersionstamp(nextVersion))

	util.ResponseJSON(w, r, http.StatusOK, struct {
		NextBatch string
		Rooms     map[types.MembershipTup][]*types.Event
	}{nextBatch, rooms})
}
