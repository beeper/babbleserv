package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/databases/rooms"
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
		"state DAG room":        {fmt.Errorf("%w: !room", rooms.ErrStateDAGFederation), http.StatusBadRequest, mautrix.MIncompatibleRoomVersion.ErrCode},
		"template of a version": {fmt.Errorf("%w: 1", errIncompatibleRoomVersion), http.StatusBadRequest, mautrix.MIncompatibleRoomVersion.ErrCode},
		"failed by the job":     {&types.IngestFailure{Status: http.StatusForbidden, ErrCode: mautrix.MForbidden.ErrCode}, http.StatusForbidden, mautrix.MForbidden.ErrCode},
		"invalid template":      {fmt.Errorf("%w from other", errInvalidMembershipTemplate), http.StatusBadGateway, mautrix.MUnknown.ErrCode},
	} {
		w := httptest.NewRecorder()
		responseRemoteMembershipError(w, httptest.NewRequest(http.MethodPost, "/join", nil), test.err)
		assert.Equal(t, test.status, w.Code, name)
		var body map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), name)
		assert.Equal(t, test.errcode, body["errcode"], name)
	}
}
