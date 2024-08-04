package types_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestEventSerializationNoPrevState(t *testing.T) {
	// Check empty event has nil PrevState
	ev := &types.Event{}
	assert.Nil(t, ev.PrevState)

	// And this is maintained when doing msgpack marshal/unmarshal
	b, err := msgpack.Marshal(ev)
	require.NoError(t, err)
	ev = &types.Event{}
	err = msgpack.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.Nil(t, ev.PrevState)

	// And also when doing JSON
	b, err = json.Marshal(ev)
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(b, "prev_state").Exists())
	ev = &types.Event{}
	err = json.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.Nil(t, ev.PrevState)
}

func TestEventSerializationWithPrevState(t *testing.T) {
	// Check empty event has nil PrevState
	ev := &types.Event{
		PrevState: []id.EventID{},
	}
	assert.NotNil(t, ev.PrevState)

	// And this is maintained when doing msgpack marshal/unmarshal
	b, err := msgpack.Marshal(ev)
	require.NoError(t, err)
	ev = &types.Event{}
	err = msgpack.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.NotNil(t, ev.PrevState)

	// And also when doing JSON
	b, err = json.Marshal(ev)
	require.NoError(t, err)
	assert.True(t, gjson.GetBytes(b, "prev_state").Exists())
	ev = &types.Event{}
	err = json.Unmarshal(b, &ev)
	require.NoError(t, err)
	assert.NotNil(t, ev.PrevState)
}
