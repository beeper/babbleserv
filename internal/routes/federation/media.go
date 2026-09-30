package federation

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/datastores"
)

const (
	defaultFederationMediaSize       = int64(50 * 1024 * 1024)
	defaultFederationPendingWait     = time.Minute
	defaultFederationThumbnailPixels = int64(20_000_000)
	defaultFederationSourcePixels    = int64(40_000_000)
)

var federationInlineMediaTypes = map[string]struct{}{
	"text/css": {}, "text/plain": {}, "text/csv": {},
	"application/json": {}, "application/ld+json": {},
	"image/jpeg": {}, "image/gif": {}, "image/png": {}, "image/apng": {}, "image/webp": {}, "image/avif": {},
	"video/mp4": {}, "video/webm": {}, "video/ogg": {}, "video/quicktime": {},
	"audio/mp4": {}, "audio/webm": {}, "audio/aac": {}, "audio/mpeg": {}, "audio/ogg": {},
	"audio/wave": {}, "audio/wav": {}, "audio/x-wav": {}, "audio/x-pn-wav": {}, "audio/flac": {}, "audio/x-flac": {},
}

// https://spec.matrix.org/v1.16/server-server-api/#get_matrixfederationv1mediadownloadmediaid
func (f *FederationRoutes) DownloadMedia(w http.ResponseWriter, r *http.Request) {
	media, ok := f.waitForLocalMedia(w, r, federationMediaID(r))
	if ok {
		f.writeFederationMedia(w, r, media)
	}
}

// https://spec.matrix.org/v1.16/server-server-api/#get_matrixfederationv1mediathumbnailmediaid
func (f *FederationRoutes) DownloadThumbnail(w http.ResponseWriter, r *http.Request) {
	if _, err := f.federationPendingWait(r); err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return
	}
	if !util.ValidMediaID(federationMediaID(r)) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}
	width, widthErr := strconv.Atoi(r.URL.Query().Get("width"))
	height, heightErr := strconv.Atoi(r.URL.Query().Get("height"))
	method := r.URL.Query().Get("method")
	if method == "" {
		method = "scale"
	}
	if raw := r.URL.Query().Get("animated"); raw != "" && raw != "true" && raw != "false" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "animated must be true or false")
		return
	}
	maxPixels := f.config.Media.MaxThumbnailPixels
	if maxPixels <= 0 {
		maxPixels = defaultFederationThumbnailPixels
	}
	tooManyPixels := width > 0 && height > 0 && int64(width) > maxPixels/int64(height)
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 ||
		(method != "crop" && method != "scale") || tooManyPixels {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Invalid thumbnail dimensions or method")
		return
	}
	mediaID := federationMediaID(r)
	key := federationThumbnailMediaID(mediaID, width, height, method)
	thumbnail, err := f.db.Media.GetMedia(r.Context(), f.config.ServerName, key)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if thumbnail == nil {
		original, ok := f.waitForLocalMedia(w, r, mediaID)
		if !ok {
			return
		}
		thumbnail, err = f.generateFederationThumbnail(r, original, key, width, height, method)
		if err != nil {
			switch {
			case errors.Is(err, util.ErrThumbnailTooLarge):
				util.ResponseErrorJSON(w, r, mautrix.MTooLarge)
			case errors.Is(err, util.ErrThumbnailUnsupported):
				util.ResponseJSON(w, r, http.StatusBadRequest, map[string]string{"errcode": mautrix.MUnknown.ErrCode, "error": err.Error()})
			default:
				util.ResponseErrorUnknownJSON(w, r, err)
			}
			return
		}
	}
	f.writeFederationMedia(w, r, thumbnail)
}

func (f *FederationRoutes) federationMediaSizeLimit() int64 {
	if f.config.Media.MaxRemoteDownloadSize > 0 {
		return f.config.Media.MaxRemoteDownloadSize
	}
	if f.config.Media.MaxUploadSize > 0 {
		return f.config.Media.MaxUploadSize
	}
	return defaultFederationMediaSize
}

func (f *FederationRoutes) federationPendingWait(r *http.Request) (time.Duration, error) {
	maximum := f.config.Media.MaxPendingUploadWait
	if maximum <= 0 {
		maximum = defaultFederationPendingWait
	}
	wait := 20 * time.Second
	if raw := r.URL.Query().Get("timeout_ms"); raw != "" {
		milliseconds, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return 0, errors.New("timeout_ms must be a non-negative integer")
		}
		maximumMilliseconds := uint64(maximum / time.Millisecond)
		if milliseconds > maximumMilliseconds {
			return maximum, nil
		}
		wait = time.Duration(milliseconds) * time.Millisecond
	}
	return min(wait, maximum), nil
}

func (f *FederationRoutes) waitForLocalMedia(w http.ResponseWriter, r *http.Request, mediaID string) (*types.Media, bool) {
	wait, err := f.federationPendingWait(r)
	if err != nil {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, err.Error())
		return nil, false
	}
	if !util.ValidMediaID(mediaID) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return nil, false
	}
	deadline := time.Now().Add(wait)
	for {
		media, err := f.db.Media.GetMedia(r.Context(), f.config.ServerName, mediaID)
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return nil, false
		}
		if media == nil || (media.UploadedAt.IsZero() && !media.ExpiresAt.IsZero() && !time.Now().Before(media.ExpiresAt)) {
			util.ResponseErrorJSON(w, r, mautrix.MNotFound)
			return nil, false
		}
		if !media.UploadedAt.IsZero() {
			if media.Size > f.federationMediaSizeLimit() {
				writeFederationMediaError(w, r, http.StatusBadGateway, "M_TOO_LARGE", "Content is too large to serve")
				return nil, false
			}
			return media, true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			writeFederationMediaError(w, r, http.StatusGatewayTimeout, "M_NOT_YET_UPLOADED", "Media has not been uploaded yet")
			return nil, false
		}
		select {
		case <-r.Context().Done():
			return nil, false
		case <-time.After(min(50*time.Millisecond, remaining)):
		}
	}
}

func (f *FederationRoutes) generateFederationThumbnail(r *http.Request, original *types.Media, key string, width, height int, method string) (*types.Media, error) {
	input, err := f.datastores.GetObjectForMedia(r.Context(), original)
	if err != nil {
		return nil, err
	}
	if closer, ok := input.(io.Closer); ok {
		defer closer.Close()
	}
	maxSourcePixels := f.config.Media.MaxThumbnailSourcePixels
	if maxSourcePixels <= 0 {
		maxSourcePixels = defaultFederationSourcePixels
	}
	encoded, err := util.GenerateThumbnail(input, f.federationMediaSizeLimit(), maxSourcePixels, width, height, method)
	if err != nil {
		return nil, err
	}
	thumbnail := types.NewMedia(original.ServerName, key, original.StoreKey, original.Sender)
	thumbnail.StorePath = original.StorePath + ".thumbnails/" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + "-" + method + ".png"
	thumbnail.Size = int64(len(encoded))
	thumbnail.ContentType = "image/png"
	thumbnail.FileName = "thumbnail.png"
	thumbnail.UploadedAt = time.Now().UTC()
	if err = f.datastores.PutObjectForMedia(r.Context(), thumbnail, bytes.NewReader(encoded), datastores.ObjectInfo{
		Size: thumbnail.Size, ContentType: thumbnail.ContentType,
	}); err != nil {
		return nil, err
	}
	if err = f.db.Media.SetMedia(r.Context(), thumbnail); err != nil {
		return nil, err
	}
	return thumbnail, nil
}

func federationThumbnailMediaID(mediaID string, width, height int, method string) string {
	return mediaID + "/thumbnail/" + strconv.Itoa(width) + "x" + strconv.Itoa(height) + "-" + method
}

func (f *FederationRoutes) writeFederationMedia(w http.ResponseWriter, r *http.Request, media *types.Media) {
	if media.Size > f.federationMediaSizeLimit() {
		writeFederationMediaError(w, r, http.StatusBadGateway, "M_TOO_LARGE", "Content is too large to serve")
		return
	}
	info, err := f.datastores.GetObjectInfoForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	} else if info.Size != media.Size {
		util.ResponseErrorUnknownJSON(w, r, errors.New("media datastore size does not match published metadata"))
		return
	}
	input, err := f.datastores.GetObjectForMedia(r.Context(), media)
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if closer, ok := input.(io.Closer); ok {
		defer closer.Close()
	}
	contentType := media.ContentType
	if len(contentType) > 255 {
		contentType = "application/octet-stream"
	} else if parsed, params, parseErr := mime.ParseMediaType(contentType); parseErr != nil || parsed == "" {
		contentType = "application/octet-stream"
	} else if formatted := mime.FormatMediaType(strings.ToLower(parsed), params); formatted == "" {
		contentType = "application/octet-stream"
	} else {
		contentType = formatted
	}
	disposition := "attachment"
	baseType := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if _, inline := federationInlineMediaTypes[baseType]; inline {
		disposition = "inline"
	}
	if validFederationMediaFilename(media.FileName) {
		disposition = mime.FormatMediaType(disposition, map[string]string{"filename": media.FileName})
	}

	multipartWriter := multipart.NewWriter(w)
	w.Header().Set("Content-Type", strings.Replace(multipartWriter.FormDataContentType(), "form-data", "mixed", 1))
	metadataPart, err := multipartWriter.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json"}})
	if err != nil {
		return
	}
	if _, err = metadataPart.Write([]byte("{}")); err != nil {
		return
	}
	dataPart, err := multipartWriter.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {contentType},
		"Content-Disposition": {disposition},
	})
	if err != nil {
		return
	}
	if _, err = io.CopyN(dataPart, input, media.Size); err != nil {
		f.log.Warn().Err(err).Msg("Failed to stream federation media response")
		return
	}
	if err = multipartWriter.Close(); err != nil {
		f.log.Warn().Err(err).Msg("Failed to close federation media response")
	}
}

func validFederationMediaFilename(fileName string) bool {
	if fileName == "" || len(fileName) > 1024 || !utf8.ValidString(fileName) {
		return false
	}
	return !strings.ContainsAny(fileName, "\x00\r\n")
}

func writeFederationMediaError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	util.ResponseJSON(w, r, status, map[string]string{"errcode": code, "error": message})
}

func federationMediaID(r *http.Request) string {
	mediaID := chi.URLParam(r, "mediaID")
	if r.URL.RawPath != "" {
		mediaID, _ = url.PathUnescape(mediaID)
	}
	return mediaID
}
