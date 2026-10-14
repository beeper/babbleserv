package state

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type stageTest struct {
	store    *memStore
	batch    *Batch
	contexts []types.StateHash
	states   []types.StateMap
	// Applied on the way to the contexts but never staged
	intermediate types.StateHash
}

func newStageTest(t *testing.T) *stageTest {
	st := &stageTest{store: newMemStore()}
	st.batch = newBatch(st.store, newCache(0), testRoomID, smallLimits)

	stateMap := initialState()
	for i := range 150 {
		stateMap[types.MemberStateTup(userID(i))] = id.EventID(fmt.Sprintf("$join%d", i))
	}
	ctx := st.apply(t, EmptyContext, stateMap, stateMap)
	st.intermediate = st.apply(t, ctx, stateMap, types.StateMap{topicTup: "$topic1"})
	ctx = st.apply(t, st.intermediate, stateMap, types.StateMap{topicTup: "$topic2", types.MemberStateTup(userID(3)): "$leave3"})
	st.apply(t, ctx, stateMap, types.StateMap{types.MemberStateTup(userID(200)): "$join200"})
	return st
}

func (st *stageTest) apply(t *testing.T, from types.StateHash, stateMap, delta types.StateMap) types.StateHash {
	ctx, err := st.batch.TxnApply(nil, from, withMemberships(delta))
	require.NoError(t, err)
	applyToModel(stateMap, delta)
	st.contexts = append(st.contexts, ctx)
	st.states = append(st.states, maps.Clone(stateMap))
	return ctx
}

func (st *stageTest) staged() []types.StateHash {
	staged := make([]types.StateHash, 0, len(st.contexts))
	for _, ctx := range st.contexts {
		if ctx != st.intermediate {
			staged = append(staged, ctx)
		}
	}
	return staged
}

func (st *stageTest) recordBytes(r stagedRecord) int {
	return st.store.keySize(testRoomID) + len(r.raw)
}

func TestStageWritesContextsReadableInAFreshBatch(t *testing.T) {
	st := newStageTest(t)
	require.NoError(t, st.batch.Stage(context.Background(), 2048, st.staged()...))
	require.Greater(t, len(st.store.stages), 1)

	fresh := newBatch(st.store, newCache(0), testRoomID, smallLimits)
	for i, ctx := range st.contexts {
		state, err := iterateEventIDs(fresh, ctx)
		if ctx == st.intermediate {
			assert.ErrorIs(t, err, ErrContextNotFound)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, st.states[i], state)
	}
}

func TestStageWritesChildrenBeforeParents(t *testing.T) {
	st := newStageTest(t)
	pending := maps.Clone(st.batch.tree.pending)
	require.NoError(t, st.batch.Stage(context.Background(), 1024, st.staged()...))

	type recordKey struct {
		kind recordKind
		hash types.StateHash
	}
	written := make(map[recordKey]int)
	chunkOf := make(map[recordKey]int)
	var records []stagedRecord
	for i, chunk := range st.store.stages {
		for _, r := range chunk {
			key := recordKey{r.kind, r.hash}
			_, again := written[key]
			require.False(t, again, "%x is staged twice", r.hash)
			written[key], chunkOf[key] = len(records), i
			records = append(records, r)
		}
	}

	var withinChunk, acrossChunks int
	requireWrittenBefore := func(parent, child recordKey) {
		at, found := written[child]
		require.True(t, found, "%x is not staged", child.hash)
		require.Less(t, at, written[parent])
		if chunkOf[child] == chunkOf[parent] {
			withinChunk++
		} else {
			acrossChunks++
		}
	}
	requirePagesBefore := func(parent recordKey, pages ...types.StateHash) {
		for _, h := range pages {
			if _, pending := pending[h]; pending {
				requireWrittenBefore(parent, recordKey{pageRecord, h})
			}
		}
	}
	kind := pageRecord
	for _, r := range records {
		require.GreaterOrEqual(t, r.kind, kind, "pages go first, then context rows")
		kind = r.kind
		key := recordKey{r.kind, r.hash}
		switch r.kind {
		case pageRecord:
			p, err := decodePage(r.raw)
			require.NoError(t, err)
			requirePagesBefore(key, p.children[:]...)
		case contextRecord:
			c, err := decodeContext(r.raw)
			require.NoError(t, err)
			requirePagesBefore(key, c.stateRoot, c.memberRoot)
		}
	}
	assert.Equal(t, contextRecord, kind)
	assert.NotZero(t, withinChunk)
	assert.NotZero(t, acrossChunks)
}

func TestStageChunksRespectTheBudget(t *testing.T) {
	for _, budget := range []int{1, 600, 4096, 5_000_000} {
		st := newStageTest(t)
		require.NoError(t, st.batch.Stage(context.Background(), budget, st.staged()...))
		if budget == 5_000_000 {
			assert.Len(t, st.store.stages, 1)
		}
		for i, chunk := range st.store.stages {
			require.NotEmpty(t, chunk)
			total := 0
			for _, r := range chunk {
				total += st.recordBytes(r)
			}
			if len(chunk) > 1 {
				require.LessOrEqual(t, total, budget, "budget %d, chunk %d", budget, i)
			}
			if i+1 < len(st.store.stages) {
				require.Greater(t, total+st.recordBytes(st.store.stages[i+1][0]), budget, "budget %d, chunk %d is not full", budget, i)
			}
		}
	}
}

func TestTxnWriteSkipsStagedRecords(t *testing.T) {
	st := newStageTest(t)
	staged := st.staged()
	require.NoError(t, st.batch.Stage(context.Background(), 4096, staged...))
	for _, ctx := range staged {
		assert.NotContains(t, st.batch.contexts, ctx, "a committed staging transaction leaves nothing pending")
	}

	st.store.pageWrites, st.store.contextWrites = 0, 0
	st.batch.TxnWrite(fdb.Transaction{}, staged...)
	assert.Zero(t, st.store.pageWrites)
	assert.Zero(t, st.store.contextWrites)

	last := staged[len(staged)-1]
	next, err := st.batch.TxnApply(nil, last, withMemberships(types.StateMap{topicTup: "$topic3"}))
	require.NoError(t, err)
	newPages := len(st.batch.unwritten([]types.StateHash{next}).pages)
	require.NotZero(t, newPages)
	st.batch.TxnWrite(fdb.Transaction{}, append(staged, next)...)
	assert.Equal(t, newPages, st.store.pageWrites)
	assert.Equal(t, 1, st.store.contextWrites)
}

func TestStagedRecordsMoveIntoTheCache(t *testing.T) {
	st := newStageTest(t)
	cache := newCache(defaultCacheBytes)
	st.batch.tree.cache = cache
	staged := st.staged()
	require.NoError(t, st.batch.Stage(context.Background(), 4096, staged...))
	assert.Equal(t, []types.StateHash{st.intermediate}, slices.Collect(maps.Keys(st.batch.contexts)), "only what was not staged stays pending")

	st.store.resetCounts()
	for i, ctx := range st.contexts {
		if ctx == st.intermediate {
			continue
		}
		state, err := iterateEventIDs(newBatch(st.store, cache, testRoomID, smallLimits), ctx)
		require.NoError(t, err)
		assert.Equal(t, st.states[i], state)
	}
	assert.Zero(t, st.store.pageReads+st.store.contextReads)
}

func TestStageAgainIsANoop(t *testing.T) {
	st := newStageTest(t)
	require.NoError(t, st.batch.Stage(context.Background(), 4096, st.staged()...))
	stages := st.store.stageCalls

	require.NoError(t, st.batch.Stage(context.Background(), 4096, st.staged()...))
	assert.Equal(t, stages, st.store.stageCalls)
}

func TestStageCarriesOnAfterAFailedChunk(t *testing.T) {
	st := newStageTest(t)
	st.store.failingStage = 3
	require.ErrorIs(t, st.batch.Stage(context.Background(), 1024, st.staged()...), errStageFailed)
	require.Len(t, st.store.stages, 2)

	require.NoError(t, st.batch.Stage(context.Background(), 1024, st.staged()...))
	written := make(map[types.StateHash]struct{})
	for _, chunk := range st.store.stages {
		for _, r := range chunk {
			_, again := written[r.hash]
			require.False(t, again, "%x is staged twice", r.hash)
			written[r.hash] = struct{}{}
		}
	}

	fresh := newBatch(st.store, newCache(0), testRoomID, smallLimits)
	for i, ctx := range st.contexts {
		if ctx != st.intermediate {
			state, err := iterateEventIDs(fresh, ctx)
			require.NoError(t, err)
			assert.Equal(t, st.states[i], state)
		}
	}
}

func TestPendingBytesIsWhatStageWrites(t *testing.T) {
	st := newStageTest(t)
	pending := st.batch.PendingBytes(st.staged()...)
	require.NoError(t, st.batch.Stage(context.Background(), 1<<20, st.staged()...))

	written := 0
	for _, chunk := range st.store.stages {
		for _, r := range chunk {
			written += st.recordBytes(r)
		}
	}
	assert.Equal(t, written, pending)
	assert.Zero(t, st.batch.PendingBytes(st.staged()...))
}
