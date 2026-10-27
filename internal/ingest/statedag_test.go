package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestStateDAGWorksRunJoinsOldestFirst(t *testing.T) {
	joins, pdus := splitTargets([]*types.IngestTarget{
		testTarget(types.IngestTargetJoin, "$join2", 3),
		testTarget(types.IngestTargetPDU, "$pdu", 2),
		testTarget(types.IngestTargetJoin, "$join1", 1),
		testTarget(types.IngestTargetPDU, "$gap", 2),
	})
	citesJoin := testTarget(types.IngestTargetPDU, "$citesJoin", 4)
	citesJoin.Event.PrevStateEventIDs = []id.EventID{"$join1"}
	citesThat := testTarget(types.IngestTargetPDU, "$citesThat", 5)
	citesThat.Event.PrevEventIDs = []id.EventID{"$citesJoin"}
	for _, target := range slices.Concat(joins, pdus, []*types.IngestTarget{citesJoin, citesThat}) {
		target.Event.RoomVersion = string(gomatrixserverlib.RoomVersionStateDAGs)
	}
	connected, gaps := pdus[1:], sortTargetsTopologically([]*types.IngestTarget{citesThat, citesJoin, pdus[0]})

	kinds := stateDAGWorkKinds(&jobRun{})
	works := roundWorks(kinds, joins, connected, gaps, false)
	require.Len(t, works, 2, "while this server is not in the room, PDUs wait for its joins")
	assert.Equal(t, []id.EventID{"$join1"}, targetIDs(works[0].targets()), "oldest first")
	assert.IsType(t, &stateDAGJoin{}, works[0])

	works = roundWorks(kinds, joins, connected, gaps, true)
	require.Len(t, works, 5)
	assert.Equal(t, []id.EventID{"$pdu"}, targetIDs(works[0].targets()), "in a room this server is in, connected PDUs go first")
	assert.False(t, works[0].(*stateDAGPDUs).gap)
	assert.Equal(t, []id.EventID{"$gap"}, targetIDs(works[1].targets()), "then those with a gap")
	assert.True(t, works[1].(*stateDAGPDUs).gap)
	assert.Equal(t, []id.EventID{"$join1"}, targetIDs(works[2].targets()))
	assert.Equal(t, []id.EventID{"$join2"}, targetIDs(works[3].targets()))
	assert.Equal(t, []id.EventID{"$citesJoin", "$citesThat"}, targetIDs(works[4].targets()),
		"PDUs citing a pending join, or one citing it, go after the joins, in topological order")
}

// The tips of a served state DAG are its events none of its events cites: the state extremities the
// resident has, which the join may not cite
func TestStateDAGJoinTips(t *testing.T) {
	w := &stateDAGJoin{served: make(map[id.EventID]bool)}
	for _, ev := range []*types.Event{
		{ID: "$create"},
		{ID: "$power", PrevStateEventIDs: []id.EventID{"$create"}},
		{ID: "$cited", PrevStateEventIDs: []id.EventID{"$power"}},
		{ID: "$tipB", PrevStateEventIDs: []id.EventID{"$power"}},
		{ID: "$tipA", PrevStateEventIDs: []id.EventID{"$cited", "$unserved"}},
		{ID: "$power", PrevStateEventIDs: []id.EventID{"$create"}},
	} {
		w.cite(ev)
	}
	assert.Equal(t, []id.EventID{"$tipA", "$tipB"}, w.tips(), "sorted, an event served twice counted once")
}

// stateDAGResponse is a send_join response with a state DAG of n events, between other members
func stateDAGResponse(n int) string {
	events := make([]string, n)
	for i := range n {
		events[i] = fmt.Sprintf(`{"event_id":"$%d","content":{"body":"%s"}}`, i, strings.Repeat("x", i))
	}
	return `{"origin":"resident.example","event":{"type":"m.room.member","content":{"membership":"join"}},` +
		`"state_dag":[` + strings.Join(events, ",") + `],"auth_chain":[{"nested":[[1,2],{"a":null}]}]}`
}

func decodeChunks(t *testing.T, body string, maxEvents, maxBytes int) ([][]string, error) {
	t.Helper()
	var chunks [][]string
	err := decodeStateDAG(strings.NewReader(body), maxEvents, maxBytes, func(raws []json.RawMessage) error {
		var ids []string
		for _, raw := range raws {
			var ev struct {
				EventID string `json:"event_id"`
			}
			require.NoError(t, json.Unmarshal(raw, &ev))
			ids = append(ids, ev.EventID)
		}
		chunks = append(chunks, ids)
		return nil
	})
	return chunks, err
}

func TestDecodeStateDAGInChunks(t *testing.T) {
	chunks, err := decodeChunks(t, stateDAGResponse(7), 3, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"$0", "$1", "$2"}, {"$3", "$4", "$5"}, {"$6"}}, chunks, "in order, at most 3 a chunk")

	chunks, err = decodeChunks(t, stateDAGResponse(4), 10, 90)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"$0", "$1"}, {"$2", "$3"}}, chunks, "a chunk past its first event holds at most 90 bytes")

	chunks, err = decodeChunks(t, stateDAGResponse(3), 10, 10)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"$0"}, {"$1"}, {"$2"}}, chunks, "an event past the bytes goes alone")

	chunks, err = decodeChunks(t, `{"state_dag":[]}`, 10, 1<<20)
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

func TestDecodeStateDAGFailures(t *testing.T) {
	for name, body := range map[string]string{
		"no state_dag":        `{"origin":"resident.example","state":[],"auth_chain":[]}`,
		"a null state_dag":    `{"state_dag":null}`,
		"not an object":       `[{"state_dag":[]}]`,
		"invalid JSON":        `{"state_dag":[{"event_id":"$0"},}`,
		"an event not an obj": `{"state_dag":[{"event_id":"$0"}, nope]}`,
	} {
		_, err := decodeChunks(t, body, 10, 1<<20)
		assert.ErrorIs(t, err, errBadStateDAGResponse, name)
	}

	_, err := decodeChunks(t, stateDAGResponse(3)[:60], 10, 1<<20)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errBadStateDAGResponse, "a body cut off is a failed request, not a bad answer")
	assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF), err)

	stop := errors.New("staging failed")
	err = decodeStateDAG(strings.NewReader(stateDAGResponse(5)), 2, 1<<20, func([]json.RawMessage) error { return stop })
	assert.Equal(t, stop, err, "an error handling a chunk ends the decoding with it")
}
