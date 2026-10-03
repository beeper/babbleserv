package stateres

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// fixtureEvent decodes an event of a Synapse fixture, written as
// [event_id, type, state_key, sender, content, auth_events, origin_server_ts, rejected?].
type fixtureEvent struct {
	*types.Event
}

func (e *fixtureEvent) UnmarshalJSON(b []byte) error {
	var fields []json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	ev := &types.Event{PartialEvent: types.PartialEvent{StateKey: new(string)}}
	var evType string
	targets := []any{&ev.ID, &evType, ev.StateKey, &ev.Sender, &ev.Content, &ev.AuthEventIDs, &ev.Timestamp, &ev.Rejected}
	if len(fields) < len(targets)-1 || len(fields) > len(targets) {
		return fmt.Errorf("fixture event has %d fields", len(fields))
	}
	for i, field := range fields {
		if err := json.Unmarshal(field, targets[i]); err != nil {
			return fmt.Errorf("fixture event field %d: %w", i, err)
		}
	}
	ev.Type = event.NewEventType(evType)
	e.Event = ev
	return nil
}

type synapseFork struct {
	Name        string         `json:"name"`
	RoomID      id.RoomID      `json:"room_id"`
	Tags        []string       `json:"tags"`
	Events      []fixtureEvent `json:"events"`
	States      [][]id.EventID `json:"states"`
	Resolved    []id.EventID   `json:"resolved"`
	roomVersion string
}

// loadForks reads the forks of a Synapse fixture as events of the room version. A fork without a
// room ID is in testRoomID.
func loadForks(t *testing.T, path, roomVersion string) []synapseFork {
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var fixture struct {
		Forks []synapseFork `json:"forks"`
	}
	require.NoError(t, json.Unmarshal(b, &fixture))
	for i := range fixture.Forks {
		fork := &fixture.Forks[i]
		fork.roomVersion = roomVersion
		for _, ev := range fork.Events {
			ev.RoomID, ev.RoomVersion = cmp.Or(fork.RoomID, testRoomID), roomVersion
		}
	}
	return fixture.Forks
}

func (f synapseFork) requireResolved(t *testing.T) {
	r := newTestRoom(t)
	r.roomVersion = f.roomVersion
	for _, ev := range f.Events {
		r.events[ev.ID] = ev.Event
	}
	states := make([]types.StateMap, len(f.States))
	for i, eventIDs := range f.States {
		states[i] = r.stateMap(eventIDs)
	}
	assert.Equal(t, r.stateMap(f.Resolved), r.resolve(states...))
}

// testdata/synapse_forks.json holds random forks with origin_server_ts values between 0 and 5 and
// power events from any member, some with an uncited conflicted power event marked rejected in
// one input. Each expected result is what Synapse 1.161.0 resolves, run as room version 2 so it
// keeps the fixture's event IDs and their tiebreaks. Forks where a leave after a leave changes the
// result were left out when the fixture was generated. A tiebreak tag means inverting that
// comparison changes the fork's result.
func TestResolveSynapseFixture(t *testing.T) {
	tagged := make(map[string]int)
	for _, fork := range loadForks(t, "testdata/synapse_forks.json", testRoomVersion) {
		for _, tag := range fork.Tags {
			tagged[tag]++
		}
		t.Run(fork.Name, fork.requireResolved)
	}

	for _, tag := range []string{"power_tiebreak", "timestamp_tiebreak", "event_id_tiebreak", "absence_conflict", "rejected_power_event"} {
		assert.Positive(t, tagged[tag], tag)
	}
}

// testdata/synapse_v12.json holds the same forks converted to room version 12 by generate_v12.py,
// with what Synapse 1.161.0 resolves for them.
func TestResolveV12SynapseFixture(t *testing.T) {
	for _, fork := range loadForks(t, "testdata/synapse_v12.json", "12") {
		t.Run(fork.Name, fork.requireResolved)
	}
}
