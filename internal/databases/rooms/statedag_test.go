package rooms

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func stateDAGTestEvent(eventID id.EventID, state bool, prevStateEventIDs ...id.EventID) *types.Event {
	ev := &types.Event{ID: eventID, PrevStateEventIDs: prevStateEventIDs}
	ev.RoomID, ev.Type = sendTestRoomID, event.EventMessage
	if state {
		ev.Type, ev.StateKey = event.StateTopic, new("")
	}
	return ev
}

func TestAdvanceStateExtremities(t *testing.T) {
	rejected := stateDAGTestEvent("$rejected", true, "$power")
	rejected.Rejected = true
	softFailed := stateDAGTestEvent("$soft", true, "$power")
	softFailed.SoftFailed = true

	for name, test := range map[string]struct {
		extremities []id.EventID
		ev          *types.Event
		expected    []id.EventID
	}{
		"create starts the state DAG":             {nil, stateDAGTestEvent("$create", true), []id.EventID{"$create"}},
		"state event replaces what it cites":      {[]id.EventID{"$power"}, stateDAGTestEvent("$topic", true, "$power"), []id.EventID{"$topic"}},
		"state event merges a fork":               {[]id.EventID{"$a", "$b"}, stateDAGTestEvent("$merge", true, "$a", "$b"), []id.EventID{"$merge"}},
		"state event citing some leaves the rest": {[]id.EventID{"$a", "$b", "$c"}, stateDAGTestEvent("$d", true, "$a", "$c"), []id.EventID{"$b", "$d"}},
		"message changes nothing":                 {[]id.EventID{"$power"}, stateDAGTestEvent("$message", false, "$power"), []id.EventID{"$power"}},
		"rejected event changes nothing":          {[]id.EventID{"$power"}, rejected, []id.EventID{"$power"}},
		"soft failed event changes nothing":       {[]id.EventID{"$power"}, softFailed, []id.EventID{"$power"}},
	} {
		assert.Equal(t, test.expected, advanceStateExtremities(test.extremities, test.ev), name)
	}
}

// Events sent in a line advance the state extremities each cites, as the publish advances events/srx
func TestStateExtremitiesAdvanceAlongABatch(t *testing.T) {
	batch := []*types.Event{
		stateDAGTestEvent("$create", true),
		stateDAGTestEvent("$join", true, "$create"),
		stateDAGTestEvent("$message", false, "$join"),
		stateDAGTestEvent("$power", true, "$join"),
		stateDAGTestEvent("$reply", false, "$power"),
	}
	var extremities []id.EventID
	for _, ev := range batch {
		assert.Equal(t, extremities, ev.PrevStateEventIDs, ev.ID)
		extremities = advanceStateExtremities(extremities, ev)
	}
	assert.Equal(t, []id.EventID{"$power"}, extremities)
}

func TestCitedExtremitiesAreThoseStoredLast(t *testing.T) {
	var extremities []id.EventID
	for i := range types.MaxStateDAGPrevEvents + 2 {
		extremities = append(extremities, id.EventID(fmt.Sprintf("$%02d", i)))
	}
	assert.Equal(t, extremities[:3], citedExtremities(extremities[:3]))
	cited := citedExtremities(extremities)
	assert.Equal(t, extremities[2:], cited)
	cited[0] = "$changed"
	assert.Equal(t, id.EventID("$02"), extremities[2], "the extremities are left as they are")
	assert.Equal(t, []id.EventID{}, citedExtremities(nil), "the create cites an empty list")
}
