package rooms

import (
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
