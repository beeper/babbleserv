package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/datastores"
)

const (
	defaultMaxUploadSize            = int64(50 * 1024 * 1024)
	defaultPendingUploadTimeout     = 24 * time.Hour
	defaultMaxPendingUploadWait     = time.Minute
	defaultMaxThumbnailPixels       = int64(20_000_000)
	defaultMaxThumbnailSourcePixels = int64(40_000_000)
)

var (
	errMediaTooLarge       = errors.New("media upload exceeds configured maximum size")
	errInvalidMediaTimeout = errors.New("timeout_ms must be a non-negative integer")
)

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
	media.ExpiresAt = media.CreatedAt.Add(c.pendingUploadTimeout())
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

func (c *ClientRoutes) maxThumbnailPixels() int64 {
	if c.config.Media.MaxThumbnailPixels > 0 {
		return c.config.Media.MaxThumbnailPixels
	}
	return defaultMaxThumbnailPixels
}

func (c *ClientRoutes) maxThumbnailSourcePixels() int64 {
	if c.config.Media.MaxThumbnailSourcePixels > 0 {
		return c.config.Media.MaxThumbnailSourcePixels
	}
	return defaultMaxThumbnailSourcePixels
}

func (c *ClientRoutes) thumbnailRequest(w http.ResponseWriter, r *http.Request) (int, int, string, bool) {
	width, widthErr := strconv.Atoi(r.URL.Query().Get("width"))
	height, heightErr := strconv.Atoi(r.URL.Query().Get("height"))
	method := r.URL.Query().Get("method")
	if method == "" {
		method = "scale"
	}
	animated := r.URL.Query().Get("animated")
	if animated != "" && animated != "true" && animated != "false" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid animated flag")
		return 0, 0, "", false
	}
	tooManyPixels := width > 0 && height > 0 && int64(width) > c.maxThumbnailPixels()/int64(height)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 ||
		(method != "crop" && method != "scale") || tooManyPixels {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid thumbnail dimensions or method")
		return 0, 0, "", false
	}
	return width, height, method, true
}

func thumbnailMediaID(mediaID string, width, height int, method string) string {
	return mediaID + "/thumbnail/" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + "-" + method
}

func (c *ClientRoutes) generateThumbnail(r *http.Request, original *types.Media, key string, width, height int, method string) (*types.Media, error) {
	input, err := c.datastores.GetObjectForMedia(r.Context(), original)
	if err != nil {
		return nil, err
	}
	if closer, ok := input.(io.Closer); ok {
		defer closer.Close()
	}
	encoded, err := util.GenerateThumbnail(
		input, c.maxUploadSize(), c.maxThumbnailSourcePixels(), width, height, method,
	)
	if err != nil {
		return nil, err
	}

	thumbnail := types.NewMedia(original.ServerName, key, original.StoreKey, original.Sender)
	// Keep derived objects next to the original object path. Treating the original
	// object's key as a directory works in S3, but cannot work in a filesystem store
	// where that path is already a regular file.
	thumbnail.StorePath = original.StorePath + ".thumbnails/" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + "-" + method + ".png"
	thumbnail.Size = int64(len(encoded))
	thumbnail.ContentType = "image/png"
	thumbnail.FileName = "thumbnail.png"
	thumbnail.UploadedAt = time.Now().UTC()
	if err = c.datastores.PutObjectForMedia(r.Context(), thumbnail, bytes.NewReader(encoded), datastores.ObjectInfo{
		Size: thumbnail.Size, ContentType: thumbnail.ContentType,
	}); err != nil {
		return nil, err
	}
	if err = c.db.Media.SetMedia(r.Context(), thumbnail); err != nil {
		return nil, err
	}
	return thumbnail, nil
}

func (c *ClientRoutes) pendingUploadTimeout() time.Duration {
	if c.config.Media.PendingUploadTimeout > 0 {
		return c.config.Media.PendingUploadTimeout
	}
	return defaultPendingUploadTimeout
}

func (c *ClientRoutes) maxPendingUploadWait() time.Duration {
	if c.config.Media.MaxPendingUploadWait > 0 {
		return c.config.Media.MaxPendingUploadWait
	}
	return defaultMaxPendingUploadWait
}

func (c *ClientRoutes) pendingWait(r *http.Request) (time.Duration, error) {
	wait := 20 * time.Second
	if raw := r.URL.Query().Get("timeout_ms"); raw != "" {
		milliseconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, errInvalidMediaTimeout
		}
		maximumMilliseconds := uint64(c.maxPendingUploadWait() / time.Millisecond)
		if milliseconds > maximumMilliseconds {
			return c.maxPendingUploadWait(), nil
		}
		wait = time.Duration(milliseconds) * time.Millisecond
	}
	if maximum := c.maxPendingUploadWait(); wait > maximum {
		return maximum, nil
	}
	return wait, nil
}

func (c *ClientRoutes) waitForMedia(r *http.Request, serverName, mediaID string, wait time.Duration) (*types.Media, error) {
	deadline := time.Now().Add(wait)
	for {
		media, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID)
		if err != nil || media == nil || !media.UploadedAt.IsZero() {
			return media, err
		}
		if !media.ExpiresAt.IsZero() && !time.Now().Before(media.ExpiresAt) {
			return nil, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return media, nil
		}
		pause := 50 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(pause):
		}
	}
}
