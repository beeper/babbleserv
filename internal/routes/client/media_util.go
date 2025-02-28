package client

import (
	"errors"
	"net/http"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (c *ClientRoutes) redirectOrDownloadMedia(w http.ResponseWriter, r *http.Request, media *types.Media) {
	// if media.UploadedAt.IsZero() {
	// 	// TODO: wait for upload
	// 	util.ResponseErrorJSON(w, r, util.MNotYetUploaded)
	// 	return
	// }

	url, err := c.datastores.PresignedGetURLForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (c *ClientRoutes) generateAndSaveNewMedia(r *http.Request) (*types.Media, error) {
	userID := middleware.GetRequestUserID(r)
	mediaID := c.db.Media.GenerateMediaID()
	datastore := c.datastores.PickDatastoreForRequest(r)
	if datastore == nil {
		return nil, errors.New("no datastore")
	}

	media := types.NewMedia(c.config.ServerName, mediaID, datastore.Key(), userID)
	if err := c.db.Media.CreateMedia(r.Context(), media); err != nil {
		return nil, err
	}

	return media, nil
}
