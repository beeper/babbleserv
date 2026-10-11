package client

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/routes/shared"
	"github.com/beeper/babbleserv/internal/util"
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
	notFound := mautrix.HTTPError{
		Response:  &http.Response{StatusCode: http.StatusNotFound},
		RespError: &mautrix.RespError{ErrCode: mautrix.MNotFound.ErrCode, Err: "no such user", StatusCode: http.StatusNotFound},
	}
	unreadable := mautrix.HTTPError{
		Response:     &http.Response{StatusCode: http.StatusOK},
		Message:      "failed to read response body",
		WrappedError: mautrix.ErrBodyReadReachedLimit,
	}
	unreachable := mautrix.HTTPError{Message: "request error", WrappedError: errors.New("connection refused")}
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"http error", notFound, http.StatusNotFound},
		{"wrapped http error", fmt.Errorf("query keys: %w", notFound), http.StatusNotFound},
		{"unreadable success", unreadable, http.StatusServiceUnavailable},
		{"request error", unreachable, http.StatusServiceUnavailable},
		{"timeout", context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"connection error", errors.New("connection refused"), http.StatusServiceUnavailable},
	} {
		assert.Equal(t, map[string]any{"status": tc.status, "message": tc.err.Error()}, remoteKeyQueryFailure(tc.err), tc.name)
	}
}

func TestRemoteKeyQueryBatches(t *testing.T) {
	missing := shared.MissingUserKeysByServer{
		"big.example":   {},
		"small.example": {"@alice:small.example": {"ALICEDEVICE"}},
	}
	for i := range 2*maxRemoteKeyQueryBatchUsers + 1 {
		missing["big.example"][id.UserID(fmt.Sprintf("@user%d:big.example", i))] = nil
	}

	batches := remoteKeyQueryBatches(missing)
	require.Len(t, batches, 4)

	assert.ElementsMatch(t, []string{"big.example", "small.example"}, []string{batches[0].server, batches[1].server})
	got := make(shared.MissingUserKeysByServer)
	for i, batch := range batches {
		assert.LessOrEqual(t, len(batch.users), maxRemoteKeyQueryBatchUsers)
		if i > 0 {
			assert.LessOrEqual(t, batches[i-1].round, batch.round)
		}
		if got[batch.server] == nil {
			got[batch.server] = make(map[id.UserID]mautrix.DeviceIDList)
		}
		for userID, deviceIDs := range batch.users {
			assert.NotContains(t, got[batch.server], userID)
			got[batch.server][userID] = deviceIDs
		}
	}
	assert.Equal(t, missing, got)
}

func parseTestSignatureUpload(body string) (map[id.UserID]map[string]signatureUploadTarget, *mautrix.RespError) {
	return util.ParseRequestJSON[map[id.UserID]map[string]signatureUploadTarget](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
}

func TestSignatureUploadTargetKeepsRawJSON(t *testing.T) {
	const target = `{"user_id":"@bob:remote.example","usage":["master"],"keys":{"ed25519:K":"K"},"org.example.extra":"signed","signatures":{"@alice:local.example":{"ed25519:USK":"sig"}}}`
	req, respErr := parseTestSignatureUpload(`{"@bob:remote.example":{"K":` + target + `}}`)
	require.Nil(t, respErr)

	parsed := req["@bob:remote.example"]["K"]
	assert.Equal(t, target, string(parsed.raw))
	assert.Equal(t, []id.CrossSigningUsage{id.XSUsageMaster}, parsed.object.Usage)
	assert.Equal(t, signatures.Signatures{"@alice:local.example": {"ed25519:USK": "sig"}}, parsed.object.Signatures)

	_, respErr = parseTestSignatureUpload(`{"@bob:remote.example":{"K":{"keys":"not an object"}}}`)
	require.NotNil(t, respErr)
	assert.Equal(t, mautrix.MBadJSON.ErrCode, respErr.ErrCode)
}
