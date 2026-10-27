package federation

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestAdmissionResolvesOnce(t *testing.T) {
	admitted := newAdmission()
	admitted.resolve(true)
	admitted.resolve(false)
	assert.True(t, admitted.wait(), "the first outcome stands, as a failing PDU side resolves on its way out")

	refused := newAdmission()
	refused.resolve(false)
	assert.False(t, refused.wait())
}

func TestTransactionsWithoutIngestingRoomsDoNotWait(t *testing.T) {
	txnRooms := &pduRooms{rooms: map[id.RoomID]pduRoom{
		"!joined:example.com": {version: "11", joined: true},
		"!other:example.com":  {version: "11"},
	}}
	assert.False(t, txnRooms.anyIngesting())

	admitted := newAdmission()
	f := &FederationRoutes{}
	waits := f.waitForQueues(httptest.NewRequest("PUT", "/send/1", nil), txnRooms, map[id.RoomID][]*types.Event{
		"!joined:example.com": nil,
		"!other:example.com":  nil,
	}, admitted)
	assert.Empty(t, waits)
	assert.True(t, admitted.wait())

	txnRooms.rooms["!joining:example.com"] = pduRoom{version: "11", ingesting: true}
	assert.True(t, txnRooms.anyIngesting())
}
