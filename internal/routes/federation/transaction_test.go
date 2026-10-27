package federation

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
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

func TestRunTransactionProcessesEDUsOnlyForPDUsSucceeding(t *testing.T) {
	log := zerolog.Nop()
	edusRan := func(pdus func(*admission) error) bool {
		ran := false
		runTransaction(&log, pdus, func() error {
			ran = true
			return nil
		})
		return ran
	}

	assert.True(t, edusRan(func(*admission) error { return nil }),
		"PDUs that waited on a queue and succeeded without admitting the EDUs early admit them")
	assert.False(t, edusRan(func(*admission) error { return errQueueFull }),
		"a transaction failing on a full queue leaves its EDUs for the retry")
	assert.False(t, edusRan(func(*admission) error { panic("boom") }))

	pduErr, _ := runTransaction(&log, func(*admission) error { panic("boom") }, func() error { return nil })
	assert.Error(t, pduErr, "a panic fails the transaction")
}

func TestRunTransactionAdmitsEDUsEarly(t *testing.T) {
	log := zerolog.Nop()
	edusDone := make(chan struct{})
	pduErr, eduErr := runTransaction(&log, func(admitted *admission) error {
		admitted.resolve(true)
		select {
		case <-edusDone:
		case <-time.After(5 * time.Second):
			t.Error("EDUs admitted early did not run alongside the PDUs")
		}
		return errors.New("failed after admitting")
	}, func() error {
		close(edusDone)
		return nil
	})
	assert.Error(t, pduErr)
	assert.NoError(t, eduErr)
}

func TestTransactionsWithoutIngestingRoomsDoNotWait(t *testing.T) {
	txnRooms := &pduRooms{rooms: map[id.RoomID]pduRoom{
		"!joined:example.com": {version: "11", joined: true},
		"!other:example.com":  {version: "11"},
	}}
	assert.False(t, txnRooms.anyIngesting())

	f := &FederationRoutes{}
	waits := f.waitForQueues(httptest.NewRequest("PUT", "/send/1", nil), txnRooms, map[id.RoomID][]*types.Event{
		"!joined:example.com": nil,
		"!other:example.com":  nil,
	}, time.Now())
	assert.Empty(t, waits)

	txnRooms.rooms["!joining:example.com"] = pduRoom{version: "11", ingesting: true}
	assert.True(t, txnRooms.anyIngesting())
}
