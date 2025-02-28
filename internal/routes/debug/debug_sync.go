package debug

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/util"
)

func (b *DebugRoutes) DebugInitUser(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	if sync, err := b.db.InitForUser(r.Context(), userID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, sync)
		return
	}
}

func (b *DebugRoutes) DebugSyncUser(w http.ResponseWriter, r *http.Request) {
	userID := id.UserID(chi.URLParam(r, "userID"))

	versions, err := util.VersionMapFromRequestQuery(r, "since")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	limit, err := util.IntFromRequestQuery(r, "limit", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	if sync, err := b.db.SyncForUser(r.Context(), userID, versions, databases.SyncOptions{
		Limit: limit,
	}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, sync)
		return
	}
}

func (b *DebugRoutes) DebugSyncServer(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")

	versions, err := util.VersionMapFromRequestQuery(r, "since")
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	limit, err := util.IntFromRequestQuery(r, "limit", 10)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}

	if sync, err := b.db.SyncForServer(r.Context(), serverName, versions, databases.SyncOptions{
		Limit: limit,
	}); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else {
		util.ResponseJSON(w, r, http.StatusOK, sync)
		return
	}
}
