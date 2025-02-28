package client

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediaconfig
func (c *ClientRoutes) GetMediaConfig(w http.ResponseWriter, r *http.Request) {
	util.ResponseErrorJSON(w, r, util.MNotImplemented)
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediadownloadservernamemediaid
// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediadownloadservernamemediaidfilename
func (c *ClientRoutes) DownloadMedia(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")
	mediaID := chi.URLParam(r, "mediaID")

	if media, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if media == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else {
		c.redirectOrDownloadMedia(w, r, media)
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediathumbnailservernamemediaid
func (c *ClientRoutes) DownloadThumbnail(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")
	mediaID := chi.URLParam(r, "mediaID")

	query := r.URL.Query()
	method := query.Get("method")

	requestWidth := query.Get("width")
	requestHeight := query.Get("height")

	// Find matching size from: https://spec.matrix.org/v1.11/client-server-api/#thumbnails

	key := fmt.Sprintf("%s/%s-%s-%s", mediaID, requestWidth, requestHeight, method)

	media, err := c.db.Media.GetMedia(r.Context(), serverName, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if media == nil {
		// TODO: generate thumbnail using image processor
	}

	c.redirectOrDownloadMedia(w, r, media)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixmediav1create
func (c *ClientRoutes) CreateMedia(w http.ResponseWriter, r *http.Request) {
	media, err := c.generateAndSaveNewMedia(r)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	presignedURL, err := c.datastores.PresignedPutURLForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	mxc := media.ToContentURI().String()

	util.ResponseJSON(w, r, http.StatusOK, struct {
		ContentURI      string `json:"content_uri"`
		UnusedExpiresAt int    `json:"unused_expires_at"`
		UploadURL       string `json:"upload_url"`
		UploadMethod    string `json:"upload_method"`
	}{mxc, 0, presignedURL, http.MethodPut})
}

// https://github.com/matrix-org/matrix-spec-proposals/pull/3870
func (c *ClientRoutes) CompleteMedia(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")
	mediaID := chi.URLParam(r, "mediaID")

	media, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if media == nil || media.Sender != middleware.GetRequestUserID(r) {
		// Change from spec: 404 wrong user so we don't leak that this media exists
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}

	// Get object info from datastore
	info, err := c.datastores.GetObjectInfoForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	// TODO: validate size/content-type allowed

	media.Size = info.Size
	media.ContentType = info.ContentType
	media.UploadedAt = time.Now().UTC()

	if err := c.db.Media.SetMedia(r.Context(), media); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixmediav3upload
// https://spec.matrix.org/v1.11/client-server-api/#put_matrixmediav3uploadservernamemediaid
func (c *ClientRoutes) UploadMedia(w http.ResponseWriter, r *http.Request) {
	serverName := chi.URLParam(r, "serverName")
	mediaID := chi.URLParam(r, "mediaID")

	var media *types.Media
	var err error

	if serverName != "" || mediaID != "" {
		media, err = c.db.Media.GetMedia(r.Context(), serverName, mediaID)
		if media == nil || media.Sender != middleware.GetRequestUserID(r) {
			// Change from spec: 404 wrong user so we don't leak that this media exists
			util.ResponseErrorJSON(w, r, mautrix.MNotFound)
			return
		}
	} else {
		media, err = c.generateAndSaveNewMedia(r)
	}

	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	if err := c.datastores.PutObjectForMedia(r.Context(), media, r.Body); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	util.ResponseJSON(w, r, http.StatusCreated, util.EmptyJSON)
}
