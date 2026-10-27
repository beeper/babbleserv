package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestIngestTargetRoundTrip(t *testing.T) {
	stateKey := "@alice:example.com"
	ev := &Event{
		PartialEvent: PartialEvent{
			RoomID:   "!room:example.com",
			Sender:   "@alice:example.com",
			StateKey: &stateKey,
			Type:     event.StateMember,
			Content:  []byte(`{"membership":"join"}`),
		},
		ID:           "$join",
		RoomVersion:  "11",
		PrevEventIDs: []id.EventID{"$prev"},
		AuthEventIDs: []id.EventID{"$create"},
		Hashes:       map[string]string{"sha256": "hash"},
		Signatures:   map[string]map[string]string{"example.com": {"ed25519:1": "sig"}},
	}
	target := &IngestTarget{
		Kind:          IngestTargetJoin,
		Event:         ev,
		Servers:       []string{"resident.example", "other.example"},
		Enqueued:      1234,
		MayHaveLanded: true,
		Failures:      2,
		Size:          321,
	}
	decoded, err := NewIngestTargetFromBytes(target.ToMsgpack(), ev.ID)
	require.NoError(t, err)
	assert.Equal(t, target.Kind, decoded.Kind)
	assert.Equal(t, target.Servers, decoded.Servers)
	assert.Equal(t, target.Enqueued, decoded.Enqueued)
	assert.True(t, decoded.MayHaveLanded)
	assert.Equal(t, 2, decoded.Failures)
	assert.Equal(t, 321, decoded.Size)
	assert.Equal(t, ev.ID, decoded.Event.ID, "the ID comes from the key")
	assert.Equal(t, ev.Type, decoded.Event.Type)
	assert.Equal(t, ev.RoomVersion, decoded.Event.RoomVersion)
	assert.Equal(t, ev.Signatures, decoded.Event.Signatures)
	assert.Equal(t, ev.Membership(), decoded.Event.Membership())

	_, err = NewIngestTargetFromBytes((&IngestTarget{Kind: IngestTargetPDU}).ToMsgpack(), "$pdu")
	assert.ErrorIs(t, err, ErrEventNotFound)
}

func TestIngestJobAndJoinRoundTrip(t *testing.T) {
	job := &IngestJob{RoomVersion: "11", Created: 1}
	decodedJob, err := NewIngestJobFromBytes(job.ToMsgpack(), "!room:example.com")
	require.NoError(t, err)
	job.RoomID = "!room:example.com"
	assert.Equal(t, job, decodedJob)

	join := &IngestJoin{EventID: "$join", Failure: &IngestFailure{Status: 403, ErrCode: "M_FORBIDDEN", Message: "banned"}}
	decodedJoin, err := NewIngestJoinFromBytes(join.ToMsgpack())
	require.NoError(t, err)
	assert.Equal(t, join, decodedJoin)

	pending, err := NewIngestJoinFromBytes((&IngestJoin{EventID: "$join"}).ToMsgpack())
	require.NoError(t, err)
	assert.Nil(t, pending.Failure)
}

func TestIngestQueueFull(t *testing.T) {
	assert.False(t, IngestQueue{}.Full(2000, 50_000_000))
	assert.False(t, IngestQueue{PDUs: 1999, Bytes: 49_999_999}.Full(2000, 50_000_000))
	assert.True(t, IngestQueue{PDUs: 2000}.Full(2000, 50_000_000), "full by count")
	assert.True(t, IngestQueue{PDUs: 3, Bytes: 50_000_000}.Full(2000, 50_000_000), "full by bytes")
}
