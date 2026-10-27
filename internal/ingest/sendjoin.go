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
)

// How long a state DAG send_join waits for its response to start, or for its body to go on, the
// timeout other federation requests have whole: a large room's response takes longer than that to
// read, and reading it stops while each chunk is verified and staged
const sendJoinStallTimeout = 30 * time.Second

// Events of a state DAG a send_join response is handed in, at most, and their bytes past the first
const (
	stateDAGChunkEvents = 1000
	stateDAGChunkBytes  = 4 << 20
)

// Most bytes a send_join response's decoder reads for one value: an event, at most 64 KiB in its
// canonical form, with room for other whitespace and escapes, or a token of a member skipped
const maxStateDAGValueBytes = 1 << 20

var (
	errSendJoinStalled = errors.New("the send_join response stalled")
	// A send_join response that is not the JSON of a state DAG room's, which sending the join again
	// would not change
	errBadStateDAGResponse   = errors.New("the send_join response is not a state DAG")
	errStateDAGValueTooLarge = fmt.Errorf("%w: a value is over %d bytes", errBadStateDAGResponse, maxStateDAGValueBytes)
)

// SendJoinStateDAG sends a join to a state DAG room (MSC4242) to a server, and hands each the state
// DAG its response carries in chunks as it arrives, so that a response of hundreds of megabytes is
// never held whole. A server refusing the join is a gomatrix.HTTPError, as fclient gives, and a
// response not of a state DAG room is an errBadStateDAGResponse. An error each returns ends the
// request with it.
func (f *Fetcher) SendJoinStateDAG(ctx context.Context, server string, joinEv *types.Event, each func([]json.RawMessage) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(sendJoinStallTimeout, func() { cancel(errSendJoinStalled) })
	defer stall.Stop()

	body, resp, err := f.streamClient.MakeFullRequest(ctx, federation.RequestParams{
		ServerName:   server,
		Method:       http.MethodPut,
		Path:         federation.URLPath{"v2", "send_join", joinEv.RoomID.String(), joinEv.ID.String()},
		Authenticate: true,
		RequestJSON:  joinEv.PDU(),
		DontReadBody: true,
	})
	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.Response != nil {
		return gomatrix.HTTPError{Code: httpErr.Response.StatusCode, Message: httpErr.Error(), Contents: body, WrappedError: err}
	} else if err != nil {
		return stalledOr(ctx, err)
	}
	defer resp.Body.Close()
	stall.Reset(sendJoinStallTimeout)
	return stalledOr(ctx, decodeStateDAG(&stallReader{resp.Body, stall}, stateDAGChunkEvents, stateDAGChunkBytes, func(raws []json.RawMessage) error {
		stall.Stop()
		defer stall.Reset(sendJoinStallTimeout)
		return each(raws)
	}))
}

// stalledOr is errSendJoinStalled for a request ended by its stall timer, else err
func stalledOr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); err != nil && errors.Is(cause, errSendJoinStalled) {
		return fmt.Errorf("%w: %w", cause, err)
	}
	return err
}

// stallReader restarts a stall timer each time a read returns
type stallReader struct {
	r     io.Reader
	stall *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.stall.Reset(sendJoinStallTimeout)
	return n, err
}

// valueReader fails a read once more than limit bytes were read since it was reset, ahead of each
// value a decoder reads, so that no one value grows the decoder's buffer past it
type valueReader struct {
	r           io.Reader
	limit, read int
}

func (v *valueReader) Read(p []byte) (int, error) {
	if v.read >= v.limit {
		return 0, errStateDAGValueTooLarge
	}
	n, err := v.r.Read(p[:min(len(p), v.limit-v.read)])
	v.read += n
	return n, err
}

// decodeStateDAG reads a state DAG room's send_join response, {origin, event, state_dag}, as a
// stream, handing each the state_dag's events in chunks of at most maxEvents and, past their first,
// maxBytes. Other members are skipped, token by token. A response that is not such JSON fails with
// errBadStateDAGResponse, as does a value over maxStateDAGValueBytes; an error each returns is
// returned as it is.
func decodeStateDAG(r io.Reader, maxEvents, maxBytes int, each func([]json.RawMessage) error) error {
	values := &valueReader{r: r, limit: maxStateDAGValueBytes}
	dec := json.NewDecoder(values)
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	found := false
	for dec.More() {
		values.read = 0
		key, err := dec.Token()
		if err != nil {
			return badStateDAG(err)
		} else if key != "state_dag" {
			if err := skipValue(dec, values); err != nil {
				return err
			}
			continue
		} else if err := expectDelim(dec, '['); err != nil {
			return err
		}
		found = true
		var chunk []json.RawMessage
		size := 0
		for dec.More() {
			values.read = 0
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return badStateDAG(err)
			} else if len(chunk) == maxEvents || len(chunk) > 0 && size+len(raw) > maxBytes {
				if err := each(chunk); err != nil {
					return err
				}
				chunk, size = nil, 0
			}
			chunk = append(chunk, raw)
			size += len(raw)
		}
		if len(chunk) > 0 {
			if err := each(chunk); err != nil {
				return err
			}
		}
		if err := expectDelim(dec, ']'); err != nil {
			return err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("%w: it has no state_dag", errBadStateDAGResponse)
	}
	return nil
}

func expectDelim(dec *json.Decoder, delim json.Delim) error {
	if token, err := dec.Token(); err != nil {
		return badStateDAG(err)
	} else if token != delim {
		return fmt.Errorf("%w: %v where %v belongs", errBadStateDAGResponse, token, delim)
	}
	return nil
}

// skipValue reads past the next value without holding it, a token at a time
func skipValue(dec *json.Decoder, values *valueReader) error {
	for depth := 0; ; {
		values.read = 0
		token, err := dec.Token()
		if err != nil {
			return badStateDAG(err)
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

// badStateDAG is an errBadStateDAGResponse for a decoding error the body itself caused, rather than
// reading it, as a body cut off does
func badStateDAG(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return fmt.Errorf("%w: %w", errBadStateDAGResponse, err)
	}
	return err
}
