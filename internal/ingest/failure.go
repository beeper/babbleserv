package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/matrix-org/gomatrix"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// sendJoinOutcome is what a send_join that got no response tells of the join, see
// classifySendJoinError
type sendJoinOutcome int

const (
	// The join may have landed or not, so the same event is sent again
	sendJoinRetry sendJoinOutcome = iota
	// Not accepted, but nothing is wrong with the join, so the same event is sent again, the
	// attempt not having landed
	sendJoinRateLimited
	// The resident server refused the join
	sendJoinRefused
)

// classifySendJoinError tells what a failed send_join means for the join: refused by a 4xx other than
// a 429, else possibly landed, as with a 5xx, a timeout or a broken connection
func classifySendJoinError(err error) sendJoinOutcome {
	var httpErr gomatrix.HTTPError
	if !errors.As(err, &httpErr) {
		return sendJoinRetry
	} else if httpErr.Code == http.StatusTooManyRequests {
		return sendJoinRateLimited
	} else if httpErr.Code >= 400 && httpErr.Code < 500 {
		return sendJoinRefused
	}
	return sendJoinRetry
}

// failureFromError is how a /join answers a join failing with the error: a remote server's error as
// it gave it, as the handler did before joins ran as jobs, a refused join as forbidden. Errors of
// this server's own are not passed on.
func failureFromError(err error) *types.IngestFailure {
	var failure *types.IngestFailure
	var httpErr gomatrix.HTTPError
	switch {
	case errors.As(err, &failure):
		return failure
	case errors.As(err, &httpErr):
		var respErr mautrix.RespError
		if json.Unmarshal(httpErr.Contents, &respErr) == nil && respErr.ErrCode != "" {
			return &types.IngestFailure{Status: httpErr.Code, ErrCode: respErr.ErrCode, Message: respErr.Err}
		}
		return &types.IngestFailure{Status: httpErr.Code, ErrCode: mautrix.MUnknown.ErrCode, Message: httpErr.Message}
	case errors.Is(err, rooms.ErrAuthStage4), errors.Is(err, rooms.ErrAuthStage5):
		return rejectedFailure(err)
	case errors.Is(err, errUnsupportedRoomVersion):
		return &types.IngestFailure{
			Status:  http.StatusBadRequest,
			ErrCode: mautrix.MUnsupportedRoomVersion.ErrCode,
			Message: err.Error(),
		}
	case errors.Is(err, errNoProgress), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &types.IngestFailure{
			Status:  http.StatusInternalServerError,
			ErrCode: mautrix.MUnknown.ErrCode,
			Message: "Timed out joining the room",
		}
	}
	return &types.IngestFailure{Status: http.StatusInternalServerError, ErrCode: mautrix.MUnknown.ErrCode, Message: "Failed to join the room"}
}

// unreachableFailure is a join whose send_join kept failing: the last server's error, or bad gateway
// when no server answered
func unreachableFailure(err error) *types.IngestFailure {
	var httpErr gomatrix.HTTPError
	if errors.As(err, &httpErr) {
		return failureFromError(err)
	}
	return &types.IngestFailure{
		Status:  http.StatusBadGateway,
		ErrCode: mautrix.MUnknown.ErrCode,
		Message: "Failed to reach a server in the room",
	}
}

// rejectedFailure is a join refused by this server's checks of it
func rejectedFailure(err error) *types.IngestFailure {
	respErr := util.RejectedEventError(err)
	return &types.IngestFailure{Status: util.ErrorStatus(respErr), ErrCode: respErr.ErrCode, Message: err.Error()}
}

// incompleteStateDAGFailure is a join to a state DAG room that no server served a state DAG for that
// connects the join to the create event, as the MSC requires
func incompleteStateDAGFailure() *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusBadGateway,
		ErrCode: mautrix.MUnknown.ErrCode,
		Message: "No server in the room served a state DAG connecting the join to the room's create event",
	}
}

// tooLargeFailure is a join refused for the events the resident gives with it, of what is given
func tooLargeFailure(what string, events, limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the room's %s %d events, this server joins rooms of at most %d", what, events, limit),
	}
}

// tooManyTipsFailure is a join to a state DAG room whose state DAG has more forward extremities than
// a join publishes beside it
func tooManyTipsFailure(tips, limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the room's state DAG has %d forward extremities, this server joins rooms with at most %d", tips, limit),
	}
}

// unfilledGapFailure is a PDU of a state DAG room citing state events that no server served a path
// from to the create event, as the MSC requires of the server sending it
func unfilledGapFailure() *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusBadGateway,
		ErrCode: mautrix.MUnknown.ErrCode,
		Message: "no server in the room served the state DAG events the event cites",
	}
}

// tooLargeGapFailure is a PDU of a state DAG room whose gap fill fetched more than it may
func tooLargeGapFailure(limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the event's state DAG gap took over %d events", limit),
	}
}
