package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/matrix-org/gomatrix"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Longest a send_join body may pause, not counting state DAG chunk staging: a large room's response
// takes far longer to read than the whole-request timeout of other federation requests
const sendJoinStallTimeout = 30 * time.Second

// A chunk's first event may alone pass the bytes bound
const (
	stateDAGChunkEvents = 1000
	stateDAGChunkBytes  = 4 << 20
)

// Per decoded value: an event is at most 64 KiB canonical, leaving room for whitespace and escapes
const maxSendJoinValueBytes = 1 << 20

var (
	errSendJoinStalled = errors.New("the send_join response stalled")
	errJoinTimedOut    = errors.New("the join timed out")
	// Sending the join to the same server again would not change this
	errBadSendJoinResponse   = errors.New("the send_join response is not the JSON of the room's version")
	errSendJoinValueTooLarge = fmt.Errorf("%w: a value is over %d bytes", errBadSendJoinResponse, maxSendJoinValueBytes)
)

// sendJoin allows SendJoinResponseTimeout for the response to start, as the resident builds it first,
// then sendJoinStallTimeout per pause in the body. A refusal is a gomatrix.HTTPError, as fclient gives.
func (f *Fetcher) sendJoin(ctx context.Context, server string, joinEv *types.Event, timesOut time.Time, read func(*stallReader) error) error {
	ctx, cancelTimeout := context.WithDeadlineCause(ctx, timesOut, errJoinTimedOut)
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(f.config.Rooms.Ingest.SendJoinResponseTimeout, func() { cancel(errSendJoinStalled) })
	defer stall.Stop()

	waited := reportProgressEvery(ctx, f.config)
	body, resp, err := f.streamClient.MakeFullRequest(ctx, federation.RequestParams{
		ServerName:   server,
		Method:       http.MethodPut,
		Path:         federation.URLPath{"v2", "send_join", joinEv.RoomID.String(), joinEv.ID.String()},
		Authenticate: true,
		RequestJSON:  joinEv.PDU(),
		DontReadBody: true,
	})
	waited()
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.Response != nil {
		return gomatrix.HTTPError{Code: httpErr.Response.StatusCode, Message: httpErr.Error(), Contents: body, WrappedError: err}
	} else if err != nil {
		return stalledOr(ctx, err)
	}
	defer resp.Body.Close()
	stall.Reset(sendJoinStallTimeout)
	return stalledOr(ctx, read(&stallReader{r: resp.Body, stall: stall}))
}

// SendJoin is for rooms without a state DAG, whose response is held whole. A response over
// rooms.ingest.maxJoinEvents events, or that many times 64 KiB, fails with a tooLargeFailure.
func (f *Fetcher) SendJoin(ctx context.Context, server string, joinEv *types.Event, timesOut time.Time) (state, authChain []json.RawMessage, err error) {
	limit := f.config.Rooms.Ingest.MaxJoinEvents
	err = f.sendJoin(ctx, server, joinEv, timesOut, func(body *stallReader) error {
		body.progress = func() { util.ReportProgress(ctx) }
		capped := &limitedReader{
			r:     body,
			limit: limit * util.MaxPDUBytes,
			err:   tooLargeFailure("state and auth chain have over the bytes of", limit, limit),
		}
		state, authChain, err = decodeLegacySendJoin(capped, limit)
		return err
	})
	return state, authChain, err
}

// SendJoinStateDAG hands each the MSC4242 state DAG in chunks as it arrives, so a response of
// hundreds of megabytes is never held whole. An error each returns ends the request.
func (f *Fetcher) SendJoinStateDAG(ctx context.Context, server string, joinEv *types.Event, timesOut time.Time, each func([]json.RawMessage) error) error {
	return f.sendJoin(ctx, server, joinEv, timesOut, func(body *stallReader) error {
		return decodeStateDAG(body, stateDAGChunkEvents, stateDAGChunkBytes, func(raws []json.RawMessage) error {
			body.stall.Stop()
			defer body.stall.Reset(sendJoinStallTimeout)
			return each(raws)
		})
	})
}

func stalledOr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); err != nil && (errors.Is(cause, errSendJoinStalled) || errors.Is(cause, errJoinTimedOut)) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

type stallReader struct {
	r        io.Reader
	stall    *time.Timer
	progress func()
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.stall.Reset(sendJoinStallTimeout)
	if n > 0 && s.progress != nil {
		s.progress()
	}
	return n, err
}

type limitedReader struct {
	r           io.Reader
	limit, read int
	err         error
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.read >= l.limit {
		return 0, l.err
	}
	n, err := l.r.Read(p[:min(len(p), l.limit-l.read)])
	l.read += n
	return n, err
}

// decodeSendJoin streams the elements of the named array members and skips the others token by
// token, bounding each value's reads so none grows the decoder's buffer past maxSendJoinValueBytes.
func decodeSendJoin(r io.Reader, arrays map[string]func(json.RawMessage) error) (map[string]bool, error) {
	values := &limitedReader{r: r, limit: maxSendJoinValueBytes, err: errSendJoinValueTooLarge}
	dec := json.NewDecoder(values)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	found := make(map[string]bool, len(arrays))
	for dec.More() {
		values.read = 0
		key, err := dec.Token()
		if err != nil {
			return nil, badResponse(err)
		}
		name, _ := key.(string)
		element, wanted := arrays[name]
		if !wanted {
			if err := skipValue(dec, values); err != nil {
				return nil, err
			}
			continue
		} else if err := expectDelim(dec, '['); err != nil {
			return nil, err
		}
		found[name] = true
		for dec.More() {
			values.read = 0
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, badResponse(err)
			} else if err := element(raw); err != nil {
				return nil, err
			}
		}
		if err := expectDelim(dec, ']'); err != nil {
			return nil, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	return found, nil
}

func decodeLegacySendJoin(r io.Reader, maxEvents int) (state, authChain []json.RawMessage, err error) {
	appendTo := func(evs *[]json.RawMessage) func(json.RawMessage) error {
		return func(raw json.RawMessage) error {
			if len(state)+len(authChain) == maxEvents {
				return tooLargeFailure("state and auth chain have over", maxEvents, maxEvents)
			}
			*evs = append(*evs, raw)
			return nil
		}
	}
	_, err = decodeSendJoin(r, map[string]func(json.RawMessage) error{"state": appendTo(&state), "auth_chain": appendTo(&authChain)})
	return state, authChain, err
}

func decodeStateDAG(r io.Reader, maxEvents, maxBytes int, each func([]json.RawMessage) error) error {
	var chunk []json.RawMessage
	size := 0
	found, err := decodeSendJoin(r, map[string]func(json.RawMessage) error{"state_dag": func(raw json.RawMessage) error {
		if len(chunk) == maxEvents || len(chunk) > 0 && size+len(raw) > maxBytes {
			if err := each(chunk); err != nil {
				return err
			}
			chunk, size = nil, 0
		}
		chunk = append(chunk, raw)
		size += len(raw)
		return nil
	}})
	if err != nil {
		return err
	} else if !found["state_dag"] {
		return fmt.Errorf("%w: it has no state_dag", errBadSendJoinResponse)
	} else if len(chunk) > 0 {
		return each(chunk)
	}
	return nil
}

func expectDelim(dec *json.Decoder, delim json.Delim) error {
	if token, err := dec.Token(); err != nil {
		return badResponse(err)
	} else if token != delim {
		return fmt.Errorf("%w: %v where %v belongs", errBadSendJoinResponse, token, delim)
	}
	return nil
}

func skipValue(dec *json.Decoder, values *limitedReader) error {
	for depth := 0; ; {
		values.read = 0
		token, err := dec.Token()
		if err != nil {
			return badResponse(err)
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}

// Only errors the body itself caused are bad responses, not a body cut off while reading
func badResponse(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return fmt.Errorf("%w: %w", errBadSendJoinResponse, err)
	}
	return err
}
