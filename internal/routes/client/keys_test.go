package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/stretchr/testify/assert"
)

func TestRemoteKeyQueryTimeout(t *testing.T) {
	for _, tc := range []struct {
		timeoutMillis int64
		expected      time.Duration
	}{
		{0, 10 * time.Second},
		{-1, 10 * time.Second},
		{1, time.Second},
		{999, time.Second},
		{1000, time.Second},
		{2500, 2500 * time.Millisecond},
		{10_000, 10 * time.Second},
		{60_000, 10 * time.Second},
		{math.MaxInt64, 10 * time.Second},
	} {
		assert.Equal(t, tc.expected, remoteKeyQueryTimeout(tc.timeoutMillis), "timeout %d", tc.timeoutMillis)
	}
}

func TestRemoteKeyQueryFailure(t *testing.T) {
	notFound := gomatrix.HTTPError{Code: http.StatusNotFound, Message: "no such user"}
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"http error", notFound, http.StatusNotFound},
		{"wrapped http error", fmt.Errorf("query keys: %w", notFound), http.StatusNotFound},
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"connection error", errors.New("connection refused"), http.StatusServiceUnavailable},
	} {
		assert.Equal(t, map[string]any{"status": tc.status, "message": tc.err.Error()}, remoteKeyQueryFailure(tc.err), tc.name)
	}
}
