package workers

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/beeper/babbleserv/internal/types"
)

func testItems(pdus, edus int, interleave bool) []transactionItem {
	var items []transactionItem
	add := func(pdu bool) {
		item := transactionItem{version: tuple.Versionstamp{UserVersion: uint16(len(items) + 1)}}
		if pdu {
			item.pdu = json.RawMessage(`{}`)
		} else {
			item.edu = &types.EDU{Type: types.EDUTypeToDevice}
		}
		items = append(items, item)
	}
	for i := 0; pdus > 0 || edus > 0; i++ {
		if pdus > 0 && (!interleave || i%2 == 0 || edus == 0) {
			add(true)
			pdus--
		} else {
			add(false)
			edus--
		}
	}
	return items
}

func TestChunkTransactionKeepsWithinTheSpecLimits(t *testing.T) {
	chunks := chunkTransaction(testItems(0, 250, false))
	require.Len(t, chunks, 3)
	assert.Len(t, chunks[0].edus, maxTransactionEDUs)
	assert.Len(t, chunks[1].edus, maxTransactionEDUs)
	assert.Len(t, chunks[2].edus, 50)
	assert.Equal(t, uint16(100), chunks[0].last.UserVersion, "a chunk ends at its last row")
	assert.Equal(t, uint16(250), chunks[2].last.UserVersion)

	chunks = chunkTransaction(testItems(120, 120, true))
	var pdus, edus int
	var last uint16
	for _, chunk := range chunks {
		assert.LessOrEqual(t, len(chunk.pdus), maxTransactionPDUs)
		assert.LessOrEqual(t, len(chunk.edus), maxTransactionEDUs)
		assert.Greater(t, chunk.last.UserVersion, last, "chunks go in row order")
		last = chunk.last.UserVersion
		pdus += len(chunk.pdus)
		edus += len(chunk.edus)
	}
	assert.Equal(t, 120, pdus)
	assert.Equal(t, 120, edus)
	assert.Len(t, chunks, 3, "the PDUs' limit cuts the chunks")

	assert.Empty(t, chunkTransaction(nil))
}

// A send failing resends from the first chunk not saved, whose transaction ID is unchanged
func TestSendChunksSavesEachChunkSent(t *testing.T) {
	chunks := chunkTransaction(testItems(0, 250, false))
	var sent []int
	var saved []uint16
	failAt := 1
	send := func(i int, _ transactionChunk) error {
		if i == failAt {
			return errors.New("timed out")
		}
		sent = append(sent, i)
		return nil
	}
	save := func(chunk transactionChunk) error {
		saved = append(saved, chunk.last.UserVersion)
		return nil
	}

	assert.Error(t, sendChunks(chunks, send, save))
	assert.Equal(t, []int{0}, sent)
	assert.Equal(t, []uint16{100}, saved, "the position moves past the first chunk only")

	resumed := chunkTransaction(testItems(0, 250, false)[saved[0]:])
	assert.Equal(t, chunks[1].last, resumed[0].last, "the chunk resent ends where it did")

	failAt, sent, saved = -1, nil, nil
	require.NoError(t, sendChunks(chunks, send, save))
	assert.Equal(t, []int{0, 1, 2}, sent)
	assert.Equal(t, []uint16{100, 200}, saved, "the last chunk's position is the sync's, saved by the caller")

	sent = nil
	require.NoError(t, sendChunks(chunks, send, nil))
	assert.Equal(t, []int{0, 1, 2}, sent)
}
