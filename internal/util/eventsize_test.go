package util_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func TestCheckEventSize(t *testing.T) {
	newEvent := func(roomVersion, stateKey, body string) *types.Event {
		content, err := json.Marshal(map[string]string{"body": body})
		require.NoError(t, err)
		return &types.Event{
			PartialEvent: types.PartialEvent{
				RoomID:   "!room:example.com",
				Sender:   "@alice:example.com",
				Type:     event.NewEventType("m.test"),
				StateKey: &stateKey,
				Content:  content,
			},
			RoomVersion: roomVersion,
		}
	}
	multiByteKey := strings.Repeat("é", util.MaxPDUFieldSize)

	for name, tc := range map[string]struct {
		ev       *types.Event
		tooLarge bool
	}{
		"small event":                             {newEvent("11", "", "hello"), false},
		"content at the limit's edge":             {newEvent("11", "", strings.Repeat("x", util.MaxPDUBytes-400)), false},
		"content over the limit":                  {newEvent("11", "", strings.Repeat("x", util.MaxPDUBytes)), true},
		"long state key in codepoints before v11": {newEvent("10", multiByteKey, ""), false},
		"long state key in bytes from v11":        {newEvent("11", multiByteKey, ""), true},
		"state key over the limit in any version": {newEvent("10", strings.Repeat("k", util.MaxPDUFieldSize+1), ""), true},
	} {
		err := util.CheckEventSize(tc.ev)
		if !tc.tooLarge {
			assert.NoError(t, err, name)
			continue
		}
		var invalid *gomatrixserverlib.EventValidationError
		require.ErrorAs(t, err, &invalid, name)
		assert.Equal(t, http.StatusRequestEntityTooLarge, invalid.Code, name)
		assert.Equal(t, mautrix.MTooLarge.ErrCode, util.RejectedEventError(err).ErrCode, name)
	}
}
