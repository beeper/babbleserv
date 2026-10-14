package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/beeper/babbleserv/internal/types"
)

func TestResolutionResultIsStoredOnceStaged(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	first, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	second, err := b.TxnApply(nil, first, withMemberships(types.StateMap{topicTup: "$topic"}))
	require.NoError(t, err)
	result, err := b.TxnApply(nil, first, withMemberships(types.StateMap{topicTup: "$topic2"}))
	require.NoError(t, err)
	require.NoError(t, b.Stage(context.Background(), 4096, first, second))

	inputs := []types.StateHash{first, second}
	_, found, err := b.TxnResolved(nil, inputs)()
	require.NoError(t, err)
	assert.False(t, found)

	require.Error(t, b.StoreResolved(context.Background(), inputs, result))
	_, found, err = b.TxnResolved(nil, inputs)()
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, b.Stage(context.Background(), 4096, result))
	require.NoError(t, b.StoreResolved(context.Background(), inputs, result))

	fresh := newBatch(store, newCache(0), testRoomID, smallLimits)
	_, found, err = fresh.TxnResolved(nil, []types.StateHash{first, result})()
	require.NoError(t, err)
	assert.False(t, found)
	stored, found, err := fresh.TxnResolved(nil, []types.StateHash{second, first, second})()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, result, stored)
	_, err = iterateEventIDs(fresh, stored)
	require.NoError(t, err)

	require.NoError(t, fresh.StoreResolved(context.Background(), []types.StateHash{result, first}, first))
	stored, found, err = fresh.TxnResolved(nil, []types.StateHash{first, result})()
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, first, stored)
}
