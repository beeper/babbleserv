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

type sendJoinOutcome int

const (
	// The join may have landed, so the same event is sent again
	sendJoinRetry sendJoinOutcome = iota
	// Not landed, but nothing is wrong with the join, so the same event is sent again
	sendJoinRateLimited
	sendJoinRefused
)

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

// failureFromError passes on a remote server's error as given, as /join did before joins ran as
// jobs, but not this server's own errors
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
	case errors.Is(err, errNoProgress), errors.Is(err, errJoinTimedOut), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &types.IngestFailure{
			Status:  http.StatusInternalServerError,
			ErrCode: mautrix.MUnknown.ErrCode,
			Message: "Timed out joining the room",
		}
	}
	return &types.IngestFailure{Status: http.StatusInternalServerError, ErrCode: mautrix.MUnknown.ErrCode, Message: "Failed to join the room"}
}

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

func rejectedFailure(err error) *types.IngestFailure {
	respErr := util.RejectedEventError(err)
	return &types.IngestFailure{Status: util.ErrorStatus(respErr), ErrCode: respErr.ErrCode, Message: err.Error()}
}

func incompleteStateDAGFailure() *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusBadGateway,
		ErrCode: mautrix.MUnknown.ErrCode,
		Message: "No server in the room served a state DAG connecting the join to the room's create event",
	}
}

func tooLargeFailure(what string, events, limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the room's %s %d events, this server joins rooms of at most %d", what, events, limit),
	}
}

func tooManyTipsFailure(tips, limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the room's state DAG has %d forward extremities, this server joins rooms with at most %d", tips, limit),
	}
}

// The MSC requires a PDU's sender to serve a path from the state events it cites to the create event
func unfilledGapFailure() *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusBadGateway,
		ErrCode: mautrix.MUnknown.ErrCode,
		Message: "no server in the room served the state DAG events the event cites",
	}
}

func tooLargeGapFailure(limit int) *types.IngestFailure {
	return &types.IngestFailure{
		Status:  http.StatusRequestEntityTooLarge,
		ErrCode: mautrix.MTooLarge.ErrCode,
		Message: fmt.Sprintf("the event's state DAG gap took over %d events", limit),
	}
}
