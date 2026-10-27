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

// How long a send_join response's body may not go on, not counting the time each state DAG chunk
// takes to stage: a large room's response takes far longer than the timeout other federation
// requests have whole to read
const sendJoinStallTimeout = 30 * time.Second

// Events of a state DAG a send_join response is handed in, at most, and their bytes past the first
const (
	stateDAGChunkEvents = 1000
	stateDAGChunkBytes  = 4 << 20
)

// Most bytes a send_join response's decoder reads for one value: an event, at most 64 KiB in its
// canonical form, with room for other whitespace and escapes, or a token of a member skipped
const maxSendJoinValueBytes = 1 << 20

var (
	errSendJoinStalled = errors.New("the send_join response stalled")
	errJoinTimedOut    = errors.New("the join timed out")
	// A send_join response that is not the JSON of the room's version, which sending the join to the
	// same server again would not change
	errBadSendJoinResponse   = errors.New("the send_join response is not the JSON of the room's version")
	errSendJoinValueTooLarge = fmt.Errorf("%w: a value is over %d bytes", errBadSendJoinResponse, maxSendJoinValueBytes)
)

// sendJoin sends a join to a server and hands read its response's body as it arrives. The response
// may take SendJoinResponseTimeout to start, as the resident builds what it serves first, meanwhile
// reporting progress, then fails once its body does not go on for sendJoinStallTimeout. The request
// ends with errJoinTimedOut at timesOut. A server refusing the join is a gomatrix.HTTPError, as
// fclient gives.
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

// SendJoin sends a join to a room without a state DAG, room versions 1 to 12, to a server, and
// returns the state and auth chain its response gives, see sendJoin. Each read of the response
// reports progress, as it is held whole. A response of more than rooms.ingest.maxJoinEvents events,
// or more bytes than that many times 64 KiB, fails with a tooLargeFailure as soon as it passes either.
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

// SendJoinStateDAG sends a join to a state DAG room (MSC4242) to a server, see sendJoin, and hands
// each the state DAG its response carries in chunks as it arrives, so that a response of hundreds of
// megabytes is never held whole. A response not of a state DAG room is an errBadSendJoinResponse.
// An error each returns ends the request with it.
func (f *Fetcher) SendJoinStateDAG(ctx context.Context, server string, joinEv *types.Event, timesOut time.Time, each func([]json.RawMessage) error) error {
	return f.sendJoin(ctx, server, joinEv, timesOut, func(body *stallReader) error {
		return decodeStateDAG(body, stateDAGChunkEvents, stateDAGChunkBytes, func(raws []json.RawMessage) error {
			body.stall.Stop()
			defer body.stall.Reset(sendJoinStallTimeout)
			return each(raws)
		})
	})
}

// stalledOr is err wrapped in what ended its request, its stall timer or the join timing out, if
// either did
func stalledOr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); err != nil && (errors.Is(cause, errSendJoinStalled) || errors.Is(cause, errJoinTimedOut)) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// stallReader restarts a stall timer each time a read returns, and reports progress for each read
// giving bytes when progress is set
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

// limitedReader fails a read with err once limit bytes were read since read was last reset
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

// decodeSendJoin reads a send_join response as a stream, handing each element of the array members
// named in arrays to the member's function as it is decoded, and returns the members found. Other
// members are skipped, token by token, and the reads of each value are bounded, so that no one value
// grows the decoder's buffer past maxSendJoinValueBytes. A response that is not such JSON fails with
// errBadSendJoinResponse, as does a value over the bound; an error a function returns is returned as
// it is.
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

// decodeLegacySendJoin reads a send_join response of a room without a state DAG, {origin, event,
// state, auth_chain}, see decodeSendJoin, failing with a tooLargeFailure as soon as its state and
// auth chain pass maxEvents events
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

// decodeStateDAG reads a state DAG room's send_join response, {origin, event, state_dag}, see
// decodeSendJoin, handing each the state_dag's events in chunks of at most maxEvents and, past their
// first, maxBytes. A response without a state_dag fails with errBadSendJoinResponse.
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

// skipValue reads past the next value without holding it, a token at a time
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

// badResponse is an errBadSendJoinResponse for a decoding error the body itself caused, rather than
// reading it, as a body cut off does
func badResponse(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return fmt.Errorf("%w: %w", errBadSendJoinResponse, err)
	}
	return err
}
