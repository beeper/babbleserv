package rooms

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestPublishedTargets(t *testing.T) {
	join := sendTestMember("$join", "@alice:example.com")
	softFailedJoin := sendTestMember("$softfailed", "@bob:example.com")
	softFailedJoin.SoftFailed = true
	rejectedJoin := sendTestMember("$rejected", "@carol:example.com")
	rejectedJoin.Rejected = true
	rejectedPDU := sendTestMember("$pdu", "@remote:other.example")
	rejectedPDU.Rejected = true
	notTarget := sendTestMember("$other", "@dave:other.example")

	targets := map[id.EventID]types.IngestTargetKind{
		join.ID:           types.IngestTargetJoin,
		softFailedJoin.ID: types.IngestTargetJoin,
		rejectedJoin.ID:   types.IngestTargetJoin,
		rejectedPDU.ID:    types.IngestTargetPDU,
	}
	published := publishedTargets([]*types.Event{join, softFailedJoin, rejectedJoin, rejectedPDU, notTarget}, targets)

	var cleared []id.EventID
	for _, target := range published {
		cleared = append(cleared, target.Event.ID)
		assert.Equal(t, targets[target.Event.ID], target.Kind)
	}
	assert.Equal(t, []id.EventID{join.ID, rejectedPDU.ID}, cleared,
		"a PDU is done once stored, a join only once it is the user's membership")
}

// fdbAdd adds a delta to a counter as FoundationDB's atomic add does
func fdbAdd(value, delta []byte) []byte {
	sum := binary.LittleEndian.Uint64(append(value, make([]byte, 8-len(value))...)) + binary.LittleEndian.Uint64(delta)
	return binary.LittleEndian.AppendUint64(nil, sum)
}

func TestIngestQueueCounters(t *testing.T) {
	pdus, bytes := []byte(nil), []byte(nil)
	sizes := []int{1200, 64_000, 300}
	for _, size := range sizes {
		pdus = fdbAdd(pdus, counterDelta(1))
		bytes = fdbAdd(bytes, counterDelta(int64(size)))
	}
	assert.Equal(t, int64(3), counterValue(pdus))
	assert.Equal(t, int64(65_500), counterValue(bytes))

	pdus = fdbAdd(pdus, counterDelta(-1))
	bytes = fdbAdd(bytes, counterDelta(-int64(sizes[1])))
	assert.Equal(t, int64(2), counterValue(pdus), "a cleared target takes back what enqueueing it added")
	assert.Equal(t, int64(1500), counterValue(bytes))

	assert.Zero(t, counterValue(nil), "a room without a queue")
}

// A gap fill's pulled PDUs are enqueued in transactions of bounded size
func TestChunkIngestTargets(t *testing.T) {
	sized := func(sizes ...int) []*types.IngestTarget {
		targets := make([]*types.IngestTarget, len(sizes))
		for i, size := range sizes {
			targets[i] = &types.IngestTarget{Size: size}
		}
		return targets
	}
	chunkSizes := func(chunks [][]*types.IngestTarget) [][]int {
		var sizes [][]int
		for _, chunk := range chunks {
			var chunkSizes []int
			for _, target := range chunk {
				chunkSizes = append(chunkSizes, target.Size)
			}
			sizes = append(sizes, chunkSizes)
		}
		return sizes
	}
	assert.Equal(t, [][]int{{40, 50}, {30, 60}, {100}}, chunkSizes(chunkIngestTargets(sized(40, 50, 30, 60, 100), 90)))
	assert.Equal(t, [][]int{{200}, {10}}, chunkSizes(chunkIngestTargets(sized(200, 10), 90)), "a target over the bound has a chunk of its own")
	assert.Empty(t, chunkIngestTargets(nil, 90))
}

// Read apart from a target written with them, targets citing it would have their gaps filled elsewhere
func TestWithoutCitersOfUnreadTargets(t *testing.T) {
	target := func(eventID id.EventID, prevIDs ...id.EventID) *types.IngestTarget {
		ev := sendTestMember(eventID, "@remote:other.example")
		ev.RoomVersion, ev.PrevEventIDs = "11", prevIDs
		return &types.IngestTarget{Kind: types.IngestTargetPDU, Event: ev}
	}
	targets := []*types.IngestTarget{
		target("$citesCiter", "$citesUnread"),
		target("$citesUnread", "$unread"),
		target("$independent", "$stored"),
	}
	var kept []id.EventID
	for _, target := range withoutCitersOf(targets, map[id.EventID]struct{}{"$unread": {}}) {
		kept = append(kept, target.Event.ID)
	}
	assert.Equal(t, []id.EventID{"$independent"}, kept)
	assert.Len(t, withoutCitersOf(targets, nil), len(targets))
}
