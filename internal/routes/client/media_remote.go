package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maunium.net/go/mautrix"
	maufederation "maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/datastores"
)

const (
	defaultRemoteDownloadTimeout = 2 * time.Minute
	maxFederationMediaMetadata   = 64 * 1024
	maxFederationMediaOverhead   = 256 * 1024
	maxRemoteMediaFilenameBytes  = 1024
	maxRemoteMediaTypeBytes      = 255
	maxRemoteMediaRedirects      = 3
)

var (
	errRemoteMediaDisabled        = errors.New("remote media acquisition is disabled for this request")
	errRemoteMediaMalformed       = errors.New("remote media response is malformed")
	errRemoteMediaNotFound        = errors.New("remote media was not found")
	errRemoteMediaNotYetUploaded  = errors.New("remote media has not yet been uploaded")
	errRemoteMediaTooLarge        = errors.New("remote media exceeds configured maximum size")
	errRemoteMediaCannotThumbnail = errors.New("remote media cannot be thumbnailed")
	errRemoteMediaUnrecognized    = errors.New("remote federation media endpoint is unrecognized")
	errRemoteMediaFetch           = errors.New("remote media request failed")
)

func (c *ClientRoutes) respondRemoteMediaError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errRemoteMediaDisabled), errors.Is(err, errRemoteMediaNotFound):
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
	case errors.Is(err, errRemoteMediaNotYetUploaded):
		writeMediaError(w, r, http.StatusGatewayTimeout, "M_NOT_YET_UPLOADED", "Media has not been uploaded yet")
	case errors.Is(err, errRemoteMediaTooLarge):
		writeMediaError(w, r, http.StatusBadGateway, "M_TOO_LARGE", "Content is too large to serve")
	case errors.Is(err, errRemoteMediaCannotThumbnail):
		writeMediaError(w, r, http.StatusBadRequest, "M_UNKNOWN", "Cannot generate thumbnail for remote content")
	case errors.Is(err, errRemoteMediaMalformed):
		writeMediaError(w, r, http.StatusBadGateway, "M_UNKNOWN", "Remote media response is invalid")
	case errors.Is(err, errRemoteMediaFetch):
		writeMediaError(w, r, http.StatusBadGateway, "M_UNKNOWN", "Remote media request failed")
	default:
		return false
	}
	return true
}

func writeMediaError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	util.ResponseJSON(w, r, status, map[string]string{"errcode": code, "error": message})
}

type remoteMediaSource struct {
	body        io.ReadCloser
	contentType string
	fileName    string
}

func (c *ClientRoutes) maxRemoteDownloadSize() int64 {
	if c.config.Media.MaxRemoteDownloadSize > 0 {
		return c.config.Media.MaxRemoteDownloadSize
	}
	return c.maxUploadSize()
}

func (c *ClientRoutes) remoteDownloadTimeout() time.Duration {
	if c.config.Media.RemoteDownloadTimeout > 0 {
		return c.config.Media.RemoteDownloadTimeout
	}
	return defaultRemoteDownloadTimeout
}

func mediaAllowsRemote(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("allow_remote")
	if raw == "" {
		return true, nil
	}
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	return false, errors.New("allow_remote must be true or false")
}

func validMediaReference(serverName, mediaID string) bool {
	if !util.ValidMediaID(mediaID) {
		return false
	}
	_, _, ok := maufederation.ParseServerName(serverName)
	return ok
}

func (c *ClientRoutes) getOrFetchMedia(
	r *http.Request,
	serverName, mediaID string,
	allowRemote bool,
) (*types.Media, error) {
	if !validMediaReference(serverName, mediaID) {
		return nil, errRemoteMediaNotFound
	}
	media, err := c.db.Media.GetMedia(r.Context(), serverName, mediaID)
	if err != nil || media != nil || serverName == c.config.ServerName {
		return media, err
	}
	if !allowRemote {
		return nil, errRemoteMediaDisabled
	}
	return c.fetchAndCacheRemoteMedia(r, serverName, mediaID)
}

func (c *ClientRoutes) fetchAndCacheRemoteMedia(
	r *http.Request,
	serverName, mediaID string,
) (*types.Media, error) {
	datastore := c.datastores.PickDatastoreForRequest(r)
	if datastore == nil {
		return nil, errors.New("no enabled media datastore")
	}
	wait, err := c.pendingWait(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.remoteDownloadTimeout())
	defer cancel()

	source, err := c.fetchRemoteMediaSource(ctx, serverName, mediaID, wait)
	if err != nil {
		return nil, err
	}
	return c.cacheRemoteMediaSource(ctx, datastore.Key(), serverName, mediaID, source)
}

func (c *ClientRoutes) fetchAndCacheRemoteThumbnail(
	r *http.Request,
	serverName, mediaID, cacheID string,
	width, height int,
	method, animated string,
) (*types.Media, error) {
	datastore := c.datastores.PickDatastoreForRequest(r)
	if datastore == nil {
		return nil, errors.New("no enabled media datastore")
	}
	wait, err := c.pendingWait(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.remoteDownloadTimeout())
	defer cancel()

	source, err := c.fetchRemoteThumbnailSource(ctx, serverName, mediaID, wait, width, height, method, animated)
	if err != nil {
		return nil, err
	}
	return c.cacheRemoteMediaSource(ctx, datastore.Key(), serverName, cacheID, source)
}

func (c *ClientRoutes) cacheRemoteMediaSource(
	ctx context.Context,
	storeKey, serverName, cacheID string,
	source *remoteMediaSource,
) (*types.Media, error) {
	defer source.body.Close()

	now := time.Now().UTC()
	candidate := types.NewMedia(serverName, cacheID, storeKey, id.UserID(""))
	candidate.CreatedAt = now
	candidate.UploadedAt = now
	candidate.ContentType = source.contentType
	candidate.FileName = source.fileName
	// Never derive a datastore path from attacker-controlled server or media
	// identifiers. Every fetch writes a unique candidate object.
	candidate.StorePath = "remote/" + c.db.Media.GenerateMediaID() + "/" + c.db.Media.GenerateMediaID()

	counter := &countingReader{reader: source.body}
	limited := &maxBytesReader{reader: counter, remaining: c.maxRemoteDownloadSize()}
	if err := c.datastores.PutObjectForMedia(ctx, candidate, limited, datastores.ObjectInfo{
		Size: -1, ContentType: candidate.ContentType,
	}); err != nil {
		c.deleteRemoteMediaCandidate(ctx, candidate)
		if errors.Is(err, errMediaTooLarge) || errors.Is(err, errRemoteMediaTooLarge) {
			return nil, errRemoteMediaTooLarge
		}
		return nil, err
	}
	candidate.Size = counter.read
	info, err := c.datastores.GetObjectInfoForMedia(ctx, candidate)
	if err != nil {
		c.deleteRemoteMediaCandidate(ctx, candidate)
		return nil, err
	} else if info.Size != candidate.Size {
		c.deleteRemoteMediaCandidate(ctx, candidate)
		return nil, fmt.Errorf("remote media datastore size mismatch: wrote %d bytes, found %d", candidate.Size, info.Size)
	}
	published, err := c.db.Media.PublishRemoteMedia(ctx, candidate)
	if err != nil {
		// The transaction may have committed even if its result is unknown.
		// Retain the candidate because it could be the published object.
		return nil, err
	}
	if published.StoreKey != candidate.StoreKey || published.StorePath != candidate.StorePath {
		// The returned metadata identifies a different immutable winner, so
		// this unique candidate is safe to remove.
		c.deleteRemoteMediaCandidate(ctx, candidate)
	}
	return published, nil
}

func (c *ClientRoutes) deleteRemoteMediaCandidate(ctx context.Context, candidate *types.Media) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.datastores.DeleteObjectForMedia(cleanupCtx, candidate); err != nil {
		c.log.Warn().Err(err).
			Str("store_key", candidate.StoreKey).
			Str("store_path", candidate.StorePath).
			Msg("Failed to delete unused remote media candidate")
	}
}

type countingReader struct {
	reader io.Reader
	read   int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (c *ClientRoutes) fetchRemoteMediaSource(
	ctx context.Context,
	serverName, mediaID string,
	wait time.Duration,
) (*remoteMediaSource, error) {
	query := url.Values{"timeout_ms": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	_, resp, err := c.mediaClient.MakeFullRequest(ctx, maufederation.RequestParams{
		ServerName:   serverName,
		Method:       http.MethodGet,
		Path:         maufederation.URLPath{"v1", "media", "download", mediaID},
		Query:        query,
		Authenticate: true,
		DontReadBody: true,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		classified := classifyRemoteMediaError(err)
		if !errors.Is(classified, errRemoteMediaUnrecognized) {
			return nil, classified
		}
		return c.fetchLegacyRemoteMediaSource(ctx, serverName, mediaID, wait)
	}
	return c.parseFederationMediaResponse(ctx, resp)
}

func classifyRemoteMediaError(err error) error {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return fmt.Errorf("%w: %v", errRemoteMediaFetch, err)
	}
	if httpErr.Response.StatusCode == http.StatusNotFound && httpErr.RespError != nil &&
		httpErr.RespError.ErrCode == mautrix.MUnrecognized.ErrCode {
		return errRemoteMediaUnrecognized
	}
	switch httpErr.Response.StatusCode {
	case http.StatusBadRequest:
		return errRemoteMediaCannotThumbnail
	case http.StatusNotFound:
		return errRemoteMediaNotFound
	case http.StatusRequestEntityTooLarge:
		return errRemoteMediaTooLarge
	case http.StatusBadGateway:
		if httpErr.RespError != nil && httpErr.RespError.ErrCode == mautrix.MTooLarge.ErrCode {
			return errRemoteMediaTooLarge
		}
		return fmt.Errorf("%w: %v", errRemoteMediaFetch, err)
	case http.StatusGatewayTimeout:
		return errRemoteMediaNotYetUploaded
	default:
		return fmt.Errorf("%w: %v", errRemoteMediaFetch, err)
	}
}

func (c *ClientRoutes) fetchRemoteThumbnailSource(
	ctx context.Context,
	serverName, mediaID string,
	wait time.Duration,
	width, height int,
	method, animated string,
) (*remoteMediaSource, error) {
	query := remoteThumbnailQuery(wait, width, height, method, animated)
	_, resp, err := c.mediaClient.MakeFullRequest(ctx, maufederation.RequestParams{
		ServerName:   serverName,
		Method:       http.MethodGet,
		Path:         maufederation.URLPath{"v1", "media", "thumbnail", mediaID},
		Query:        query,
		Authenticate: true,
		DontReadBody: true,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		classified := classifyRemoteMediaError(err)
		if !errors.Is(classified, errRemoteMediaUnrecognized) {
			return nil, classified
		}
		return c.fetchLegacyRemoteThumbnailSource(ctx, serverName, mediaID, query)
	}
	source, err := c.parseFederationMediaResponse(ctx, resp)
	return validateRemoteThumbnailSource(source, err)
}

func remoteThumbnailQuery(wait time.Duration, width, height int, method, animated string) url.Values {
	if animated == "" {
		animated = "false"
	}
	return url.Values{
		"timeout_ms": {strconv.FormatInt(wait.Milliseconds(), 10)},
		"width":      {strconv.Itoa(width)},
		"height":     {strconv.Itoa(height)},
		"method":     {method},
		"animated":   {animated},
	}
}

func (c *ClientRoutes) fetchLegacyRemoteThumbnailSource(
	ctx context.Context,
	serverName, mediaID string,
	query url.Values,
) (*remoteMediaSource, error) {
	query.Set("allow_remote", "false")
	_, resp, err := c.mediaClient.MakeFullRequest(ctx, maufederation.RequestParams{
		ServerName: serverName,
		Method:     http.MethodGet,
		Path: mautrix.BaseURLPath{
			"_matrix", "media", "v3", "thumbnail", serverName, mediaID,
		},
		Query:        query,
		DontReadBody: true,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, classifyRemoteMediaError(err)
	}
	contentType, fileName := sanitizedRemoteMediaHeaders(resp.Header)
	if resp.ContentLength > c.maxRemoteDownloadSize() {
		_ = resp.Body.Close()
		return nil, errRemoteMediaTooLarge
	}
	return validateRemoteThumbnailSource(
		&remoteMediaSource{body: resp.Body, contentType: contentType, fileName: fileName}, nil,
	)
}

func validateRemoteThumbnailSource(source *remoteMediaSource, err error) (*remoteMediaSource, error) {
	if err != nil || source == nil {
		return source, err
	}
	baseType, _, parseErr := mime.ParseMediaType(source.contentType)
	switch strings.ToLower(baseType) {
	case "image/png", "image/apng", "image/jpeg", "image/gif", "image/webp":
		return source, nil
	default:
		_ = source.body.Close()
		if parseErr != nil {
			return nil, fmt.Errorf("%w: invalid thumbnail content type", errRemoteMediaMalformed)
		}
		return nil, fmt.Errorf("%w: unsupported thumbnail content type %q", errRemoteMediaMalformed, baseType)
	}
}

func (c *ClientRoutes) fetchLegacyRemoteMediaSource(
	ctx context.Context,
	serverName, mediaID string,
	wait time.Duration,
) (*remoteMediaSource, error) {
	_, resp, err := c.mediaClient.MakeFullRequest(ctx, maufederation.RequestParams{
		ServerName: serverName,
		Method:     http.MethodGet,
		Path: mautrix.BaseURLPath{
			"_matrix", "media", "v3", "download", serverName, mediaID,
		},
		Query: url.Values{
			"allow_remote": {"false"},
			"timeout_ms":   {strconv.FormatInt(wait.Milliseconds(), 10)},
		},
		DontReadBody: true,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, classifyRemoteMediaError(err)
	}
	contentType, fileName := sanitizedRemoteMediaHeaders(resp.Header)
	if resp.ContentLength > c.maxRemoteDownloadSize() {
		_ = resp.Body.Close()
		return nil, errRemoteMediaTooLarge
	}
	return &remoteMediaSource{body: resp.Body, contentType: contentType, fileName: fileName}, nil
}

func (c *ClientRoutes) parseFederationMediaResponse(
	ctx context.Context,
	resp *http.Response,
) (*remoteMediaSource, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("%w: empty HTTP response", errRemoteMediaMalformed)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = resp.Body.Close()
		}
	}()
	// Bound the whole MIME envelope as well as the returned data part. Without
	// this wrapper, oversized part headers or a trailing part could be consumed
	// internally by multipart.NextPart without counting toward the media limit.
	maxInt64 := int64(^uint64(0) >> 1)
	responseLimit := c.maxRemoteDownloadSize()
	if responseLimit > maxInt64-maxFederationMediaOverhead {
		responseLimit = maxInt64
	} else {
		responseLimit += maxFederationMediaOverhead
	}
	resp.Body = &limitedRemoteResponseBody{ReadCloser: resp.Body, remaining: responseLimit}

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "multipart/mixed") || params["boundary"] == "" {
		return nil, fmt.Errorf("%w: expected multipart/mixed with a boundary", errRemoteMediaMalformed)
	}
	reader := multipart.NewReader(resp.Body, params["boundary"])
	metadataPart, err := reader.NextPart()
	if err != nil {
		if errors.Is(err, errRemoteMediaTooLarge) {
			return nil, errRemoteMediaTooLarge
		}
		return nil, fmt.Errorf("%w: read metadata part: %v", errRemoteMediaMalformed, err)
	}
	metadataType, _, typeErr := mime.ParseMediaType(metadataPart.Header.Get("Content-Type"))
	if typeErr != nil || !strings.EqualFold(metadataType, "application/json") {
		return nil, fmt.Errorf("%w: metadata part is not application/json", errRemoteMediaMalformed)
	}
	metadataBytes, err := io.ReadAll(io.LimitReader(metadataPart, maxFederationMediaMetadata+1))
	if errors.Is(err, errRemoteMediaTooLarge) {
		return nil, errRemoteMediaTooLarge
	}
	if err != nil || len(metadataBytes) > maxFederationMediaMetadata {
		return nil, fmt.Errorf("%w: metadata part exceeds limit", errRemoteMediaMalformed)
	}
	var metadata map[string]any
	if err = json.Unmarshal(metadataBytes, &metadata); err != nil || metadata == nil {
		return nil, fmt.Errorf("%w: metadata part is not a JSON object", errRemoteMediaMalformed)
	}

	dataPart, err := reader.NextPart()
	if err != nil {
		if errors.Is(err, errRemoteMediaTooLarge) {
			return nil, errRemoteMediaTooLarge
		}
		return nil, fmt.Errorf("%w: read data part: %v", errRemoteMediaMalformed, err)
	}
	if location := dataPart.Header.Get("Location"); location != "" {
		if err = requireEmptyFinalMultipartPart(reader, dataPart); err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		closeOnError = false
		return c.fetchRemoteMediaRedirect(ctx, location)
	}
	contentType, fileName := sanitizedRemoteMediaHeaders(http.Header(dataPart.Header))
	closeOnError = false
	return &remoteMediaSource{
		body: &exactMultipartMediaReader{
			part: dataPart, reader: reader, responseBody: resp.Body,
		},
		contentType: contentType,
		fileName:    fileName,
	}, nil
}

type limitedRemoteResponseBody struct {
	io.ReadCloser
	remaining int64
}

func (r *limitedRemoteResponseBody) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errRemoteMediaTooLarge
	}
	if r.remaining < int64(len(p)) {
		p = p[:r.remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return 0, errRemoteMediaTooLarge
	}
	return n, err
}

func requireEmptyFinalMultipartPart(reader *multipart.Reader, part *multipart.Part) error {
	body, err := io.ReadAll(io.LimitReader(part, 1))
	if err != nil || len(body) != 0 {
		return fmt.Errorf("%w: redirect part must have an empty body", errRemoteMediaMalformed)
	}
	if _, nextErr := reader.NextPart(); nextErr != io.EOF {
		return fmt.Errorf("%w: multipart response must contain exactly two parts", errRemoteMediaMalformed)
	}
	return nil
}

type exactMultipartMediaReader struct {
	part         *multipart.Part
	reader       *multipart.Reader
	responseBody io.ReadCloser
	validated    bool
	pendingErr   error
}

func (r *exactMultipartMediaReader) Read(p []byte) (int, error) {
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err
	}
	n, err := r.part.Read(p)
	if err != nil && err != io.EOF && !errors.Is(err, errRemoteMediaTooLarge) {
		return n, fmt.Errorf("%w: read media part: %v", errRemoteMediaMalformed, err)
	}
	if err != io.EOF {
		return n, err
	}
	validationErr := r.validateEnd()
	if n > 0 {
		r.pendingErr = validationErr
		return n, nil
	}
	return 0, validationErr
}

func (r *exactMultipartMediaReader) validateEnd() error {
	if r.validated {
		return io.EOF
	}
	r.validated = true
	_, err := r.reader.NextPart()
	_ = r.responseBody.Close()
	if err == io.EOF {
		return io.EOF
	}
	if errors.Is(err, errRemoteMediaTooLarge) {
		return errRemoteMediaTooLarge
	}
	if err != nil {
		return fmt.Errorf("%w: trailing multipart data: %v", errRemoteMediaMalformed, err)
	}
	return fmt.Errorf("%w: multipart response contains more than two parts", errRemoteMediaMalformed)
}

func (r *exactMultipartMediaReader) Close() error {
	return r.responseBody.Close()
}

func sanitizedRemoteMediaHeaders(header http.Header) (contentType, fileName string) {
	contentType = "application/octet-stream"
	if raw := header.Get("Content-Type"); len(raw) <= maxRemoteMediaTypeBytes {
		if parsed, params, err := mime.ParseMediaType(raw); err == nil && parsed != "" {
			contentType = mime.FormatMediaType(strings.ToLower(parsed), params)
		}
	}
	if raw := header.Get("Content-Disposition"); len(raw) <= maxRemoteMediaFilenameBytes*4 {
		if _, params, err := mime.ParseMediaType(raw); err == nil {
			candidate := params["filename"]
			if validRemoteMediaFilename(candidate) {
				fileName = candidate
			}
		}
	}
	return
}

func validRemoteMediaFilename(fileName string) bool {
	if fileName == "" || len(fileName) > maxRemoteMediaFilenameBytes || !utf8.ValidString(fileName) {
		return false
	}
	for _, char := range fileName {
		if char == 0 || char == '\r' || char == '\n' {
			return false
		}
	}
	return true
}

func (c *ClientRoutes) fetchRemoteMediaRedirect(ctx context.Context, rawURL string) (*remoteMediaSource, error) {
	transport := c.remoteMediaRedirectTransport()
	redirectClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRemoteMediaRedirects {
				return errors.New("too many media redirects")
			}
			return validateRemoteMediaRedirectURL(req.URL)
		},
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid redirect URL", errRemoteMediaMalformed)
	} else if err = validateRemoteMediaRedirectURL(parsed); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := redirectClient.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("%w: %v", errRemoteMediaFetch, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		transport.CloseIdleConnections()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errRemoteMediaNotFound
		}
		return nil, fmt.Errorf("%w: redirect returned HTTP %d", errRemoteMediaFetch, resp.StatusCode)
	}
	if resp.ContentLength > c.maxRemoteDownloadSize() {
		_ = resp.Body.Close()
		transport.CloseIdleConnections()
		return nil, errRemoteMediaTooLarge
	}
	contentType, fileName := sanitizedRemoteMediaHeaders(resp.Header)
	return &remoteMediaSource{
		body:        &closeIdleReadCloser{ReadCloser: resp.Body, transport: transport},
		contentType: contentType,
		fileName:    fileName,
	}, nil
}

func validateRemoteMediaRedirectURL(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return fmt.Errorf("%w: media redirect must be an HTTPS URL without user info", errRemoteMediaMalformed)
	}
	return nil
}

type closeIdleReadCloser struct {
	io.ReadCloser
	transport *http.Transport
}

func (c *closeIdleReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.transport.CloseIdleConnections()
	return err
}

func (c *ClientRoutes) remoteMediaRedirectTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Resolution and address filtering happen below. A configured HTTP proxy
	// would move DNS and connection policy outside this process.
	transport.Proxy = nil
	transport.MaxResponseHeaderBytes = 64 * 1024
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, resolved := range addresses {
			if !c.config.Media.AllowPrivateMediaRedirects && !isPublicMediaRedirectIP(resolved.IP) {
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			err = dialErr
		}
		if err == nil {
			err = errors.New("media redirect resolved only to disallowed network addresses")
		}
		return nil, err
	}
	return transport
}

func isPublicMediaRedirectIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil && ipv4[0] == 100 && ipv4[1]&0xc0 == 64 {
		// RFC 6598 shared address space is commonly routed inside provider or
		// deployment networks even though net.IP.IsPrivate does not include it.
		return false
	}
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}
