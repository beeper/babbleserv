package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/types"
)

// fakeWatch fires once its channel closes, with err
type fakeWatch struct {
	fire      chan struct{}
	err       error
	cancelled chan struct{}
}

func newFakeWatch(err error) *fakeWatch {
	return &fakeWatch{fire: make(chan struct{}), err: err, cancelled: make(chan struct{})}
}

func (w *fakeWatch) Get() error {
	select {
	case <-w.fire:
		return w.err
	case <-w.cancelled:
		return errors.New("operation cancelled")
	}
}

func (w *fakeWatch) MustGet()         {}
func (w *fakeWatch) BlockUntilReady() {}
func (w *fakeWatch) IsReady() bool    { return false }
func (w *fakeWatch) Cancel() {
	select {
	case <-w.cancelled:
	default:
		close(w.cancelled)
	}
}

func TestWaitForChangeReturnsOnceTheWatchFires(t *testing.T) {
	watch := newFakeWatch(nil)
	close(watch.fire)
	start := time.Now()
	require.NoError(t, waitForChange(context.Background(), watch, time.Hour))
	assert.Less(t, time.Since(start), time.Minute)
	select {
	case <-watch.cancelled:
	default:
		t.Error("the watch is cancelled")
	}
}

func TestWaitForChangePollsAWatchThatFailed(t *testing.T) {
	watch := newFakeWatch(errors.New("too many watches"))
	close(watch.fire)
	start := time.Now()
	require.NoError(t, waitForChange(context.Background(), watch, 50*time.Millisecond))
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "a failed watch waits for the poll")
}

func TestWaitForChangePollsWithoutAWatch(t *testing.T) {
	require.NoError(t, waitForChange(context.Background(), nil, time.Millisecond))
}

func TestWaitForChangeEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	watch := newFakeWatch(nil)
	assert.ErrorIs(t, waitForChange(ctx, watch, time.Hour), context.Canceled)
}

func TestRemoteMembershipErrors(t *testing.T) {
	for name, test := range map[string]struct {
		err     error
		status  int
		errcode string
	}{
		"template of a version": {fmt.Errorf("%w: 1", errIncompatibleRoomVersion), http.StatusBadRequest, mautrix.MIncompatibleRoomVersion.ErrCode},
		"failed by the job":     {&types.IngestFailure{Status: http.StatusForbidden, ErrCode: mautrix.MForbidden.ErrCode}, http.StatusForbidden, mautrix.MForbidden.ErrCode},
		"invalid template":      {fmt.Errorf("%w from other", errInvalidMembershipTemplate), http.StatusBadGateway, mautrix.MUnknown.ErrCode},
		"refused make_join": {
			gomatrix.HTTPError{Code: http.StatusForbidden, Contents: []byte(`{"errcode":"M_FORBIDDEN","error":"not invited"}`)},
			http.StatusForbidden, mautrix.MForbidden.ErrCode,
		},
		"failed make_join": {gomatrix.HTTPError{Code: http.StatusInternalServerError}, http.StatusBadGateway, mautrix.MUnknown.ErrCode},
		"unreachable make_join": {
			fmt.Errorf("%w via other: %w", errMakeRequestFailed, errors.New("connection refused")),
			http.StatusBadGateway, mautrix.MUnknown.ErrCode,
		},
		"database failure": {errors.New("transaction too old"), http.StatusInternalServerError, mautrix.MUnknown.ErrCode},
	} {
		w := httptest.NewRecorder()
		responseRemoteMembershipError(w, httptest.NewRequest(http.MethodPost, "/join", nil), test.err)
		assert.Equal(t, test.status, w.Code, name)
		var body map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), name)
		assert.Equal(t, test.errcode, body["errcode"], name)
	}
}

func TestRemoteMembershipErrorKeepsTheRoomVersion(t *testing.T) {
	w := httptest.NewRecorder()
	responseRemoteMembershipError(w, httptest.NewRequest(http.MethodPost, "/join", nil), gomatrix.HTTPError{
		Code:     http.StatusBadRequest,
		Contents: []byte(`{"errcode":"M_INCOMPATIBLE_ROOM_VERSION","error":"unsupported","room_version":"org.matrix.msc4242.12"}`),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, `{"errcode":"M_INCOMPATIBLE_ROOM_VERSION","error":"unsupported","room_version":"org.matrix.msc4242.12"}`, w.Body.String())
}

func TestOtherServersSkipsDomainlessRoomIDs(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/join?server_name=a.example.com", nil)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, getOtherServers(request, "!room:b.example.com"))
	assert.Equal(t, []string{"a.example.com"}, getOtherServers(request, "!Rj0GwmnxPwQdujbQoFNSGbRnXTcwYq1b4eMimk4k3EE"))
}

func TestKnocksOfferTheVersionsThisServerJoins(t *testing.T) {
	c := &ClientRoutes{}
	versions := c.joinedRoomVersions()
	assert.True(t, slices.IsSorted(versions))
	assert.Contains(t, versions, gomatrixserverlib.RoomVersionV12)
	assert.NotContains(t, versions, gomatrixserverlib.RoomVersionV1, "a version of another event format")
	assert.NotContains(t, versions, gomatrixserverlib.RoomVersionStateDAGs)

	c.config.Rooms.UnstableStateDAGs = true
	assert.Contains(t, c.joinedRoomVersions(), gomatrixserverlib.RoomVersionStateDAGs)
}
