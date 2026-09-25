package client

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const defaultMaxUploadSize = int64(50 * 1024 * 1024)

var errMediaTooLarge = errors.New("media upload exceeds configured maximum size")

var inlineMediaTypes = map[string]struct{}{
	"text/css": {}, "text/plain": {}, "text/csv": {},
	"application/json": {}, "application/ld+json": {},
	"image/jpeg": {}, "image/gif": {}, "image/png": {}, "image/apng": {}, "image/webp": {}, "image/avif": {},
	"video/mp4": {}, "video/webm": {}, "video/ogg": {}, "video/quicktime": {},
	"audio/mp4": {}, "audio/webm": {}, "audio/aac": {}, "audio/mpeg": {}, "audio/ogg": {},
	"audio/wave": {}, "audio/wav": {}, "audio/x-wav": {}, "audio/x-pn-wav": {}, "audio/flac": {}, "audio/x-flac": {},
}

func (c *ClientRoutes) maxUploadSize() int64 {
	if c.config.Media.MaxUploadSize > 0 {
		return c.config.Media.MaxUploadSize
	}
	return defaultMaxUploadSize
}

func (c *ClientRoutes) downloadMedia(w http.ResponseWriter, r *http.Request, media *types.Media) {
	if media == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if media.UploadedAt.IsZero() {
		util.ResponseErrorJSON(w, r, util.MNotYetUploaded)
		return
	}
	input, err := c.datastores.GetObjectForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if closer, ok := input.(io.Closer); ok {
		defer closer.Close()
	}

	contentType := media.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(media.Size, 10))
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; script-src 'none'; plugin-types application/pdf; style-src 'unsafe-inline'; media-src 'self'; object-src 'self'")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disposition := "attachment"
	baseContentType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if _, ok := inlineMediaTypes[baseContentType]; ok {
		disposition = "inline"
	}
	filename := decodedURLParam(r, "filename")
	if filename == "" {
		filename = media.FileName
	}
	if filename != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}

	if _, err = io.Copy(w, input); err != nil {
		c.log.Warn().Err(err).Msg("Failed to stream media response")
	}
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

func uploadObjectPath(media *types.Media, uploadID string) string {
	return fmt.Sprintf("%s/uploads/%s", media.StorePath, uploadID)
}

type maxBytesReader struct {
	reader    io.Reader
	remaining int64
}

func (r *maxBytesReader) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errMediaTooLarge
	}
	if r.remaining < int64(len(p)) {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return 0, errMediaTooLarge
	}
	return n, err
}
