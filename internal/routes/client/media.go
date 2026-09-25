package client

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"

	mediadb "github.com/beeper/babbleserv/internal/databases/media"
	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/datastores"
)

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediaconfig
func (c *ClientRoutes) GetMediaConfig(w http.ResponseWriter, r *http.Request) {
	util.ResponseJSON(w, r, http.StatusOK, map[string]int64{
		"m.upload.size": c.maxUploadSize(),
	})
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediadownloadservernamemediaid
// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediadownloadservernamemediaidfilename
func (c *ClientRoutes) DownloadMedia(w http.ResponseWriter, r *http.Request) {
	serverName := decodedURLParam(r, "serverName")
	mediaID := decodedURLParam(r, "mediaID")

	if media, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID); err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if media == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else {
		c.downloadMedia(w, r, media)
	}
}

// https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediathumbnailservernamemediaid
func (c *ClientRoutes) DownloadThumbnail(w http.ResponseWriter, r *http.Request) {
	width, height, method, ok := c.thumbnailRequest(w, r)
	if !ok {
		return
	}
	serverName := decodedURLParam(r, "serverName")
	mediaID := decodedURLParam(r, "mediaID")
	key := thumbnailMediaID(mediaID, width, height, method)
	thumbnail, err := c.db.Media.GetMedia(r.Context(), serverName, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if thumbnail == nil {
		original, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if original == nil {
			util.ResponseErrorJSON(w, r, mautrix.MNotFound)
			return
		} else if original.UploadedAt.IsZero() {
			util.ResponseErrorJSON(w, r, util.MNotYetUploaded)
			return
		}
		thumbnail, err = c.generateThumbnail(r, original, key, width, height, method)
		if err != nil {
			switch {
			case errors.Is(err, util.ErrThumbnailTooLarge):
				util.ResponseErrorJSON(w, r, mautrix.MTooLarge)
			case errors.Is(err, util.ErrThumbnailUnsupported):
				util.ResponseJSON(w, r, http.StatusBadRequest, map[string]string{
					"errcode": mautrix.MUnknown.ErrCode, "error": err.Error(),
				})
			default:
				util.ResponseErrorUnknownJSON(w, r, err)
			}
			return
		}
	}
	c.downloadMedia(w, r, thumbnail)
}

// https://spec.matrix.org/v1.11/client-server-api/#post_matrixmediav1create
func (c *ClientRoutes) CreateMedia(w http.ResponseWriter, r *http.Request) {
	media, err := c.generateAndSaveNewMedia(r)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	response := map[string]any{"content_uri": media.ToContentURI().String()}
	presignedURL, err := c.datastores.PresignedPutURLForMedia(r.Context(), media)
	if err == nil {
		response["upload_url"] = presignedURL
		response["upload_method"] = http.MethodPut
	} else if !errors.Is(err, datastores.ErrPresignedURLUnsupported) {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, response)
}

// https://github.com/matrix-org/matrix-spec-proposals/pull/3870
func (c *ClientRoutes) CompleteMedia(w http.ResponseWriter, r *http.Request) {
	serverName := decodedURLParam(r, "serverName")
	mediaID := decodedURLParam(r, "mediaID")

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

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixmediav3upload
// https://spec.matrix.org/v1.16/client-server-api/#put_matrixmediav3uploadservernamemediaid
func (c *ClientRoutes) UploadMedia(w http.ResponseWriter, r *http.Request) {
	maximum := c.maxUploadSize()
	if r.ContentLength > maximum {
		util.ResponseErrorJSON(w, r, mautrix.MTooLarge)
		return
	}
	serverName, mediaID := decodedURLParam(r, "serverName"), decodedURLParam(r, "mediaID")
	var media *types.Media
	var err error
	if serverName != "" || mediaID != "" {
		if serverName != c.config.ServerName || mediaID == "" {
			util.ResponseErrorJSON(w, r, mautrix.MNotFound)
			return
		}
		media, err = c.db.Media.GetMedia(r.Context(), serverName, mediaID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if media == nil {
			util.ResponseErrorJSON(w, r, mautrix.MNotFound)
			return
		} else if media.Sender != middleware.GetRequestUserID(r) {
			util.ResponseErrorJSON(w, r, mautrix.MForbidden)
			return
		} else if !media.UploadedAt.IsZero() {
			util.ResponseErrorJSON(w, r, util.MCannotOverwriteMedia)
			return
		}
	} else {
		media, err = c.generateAndSaveNewMedia(r)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		}
	}

	candidate := *media
	candidate.StorePath = uploadObjectPath(media, c.db.Media.GenerateMediaID())
	candidate.ContentType = strings.TrimSpace(r.Header.Get("Content-Type"))
	if candidate.ContentType == "" {
		candidate.ContentType = "application/octet-stream"
	}
	candidate.FileName = r.URL.Query().Get("filename")
	reader := &maxBytesReader{reader: r.Body, remaining: maximum}
	if err = c.datastores.PutObjectForMedia(r.Context(), &candidate, reader, datastores.ObjectInfo{
		Size: -1, ContentType: candidate.ContentType,
	}); err != nil {
		if errors.Is(err, errMediaTooLarge) {
			util.ResponseErrorJSON(w, r, mautrix.MTooLarge)
		} else {
			util.ResponseErrorUnknownJSON(w, r, err)
		}
		return
	}
	info, err := c.datastores.GetObjectInfoForMedia(r.Context(), &candidate)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	candidate.Size, candidate.UploadedAt = info.Size, time.Now().UTC()
	if err = c.db.Media.CompleteMediaUpload(r.Context(), &candidate, middleware.GetRequestUserID(r)); !c.handleMediaCompletionError(w, r, err) {
		return
	}

	if r.Method == http.MethodPost {
		util.ResponseJSON(w, r, http.StatusOK, map[string]string{"content_uri": candidate.ToContentURI().String()})
	} else {
		util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
	}
}

func (c *ClientRoutes) handleMediaCompletionError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, mediadb.ErrMediaNotFound):
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
	case errors.Is(err, mediadb.ErrMediaForbidden):
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
	case errors.Is(err, mediadb.ErrMediaAlreadyUploaded):
		util.ResponseErrorJSON(w, r, util.MCannotOverwriteMedia)
	default:
		util.ResponseErrorUnknownJSON(w, r, err)
	}
	return false
}
