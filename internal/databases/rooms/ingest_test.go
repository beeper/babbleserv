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
