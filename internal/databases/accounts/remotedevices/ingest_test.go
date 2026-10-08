package remotedevices

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var (
	testNow        = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	keysA1         = json.RawMessage(`{"device_id":"A","signatures":{"@u:remote":{"ed25519:A":"one"}}}`)
	keysA2         = json.RawMessage(`{"device_id":"A","signatures":{"@u:remote":{"ed25519:A":"two"}}}`)
	keysA1Signed   = json.RawMessage(`{"device_id":"A","signatures":{"@u:remote":{"ed25519:A":"one","ed25519:X":"cross"}}}`)
	keysB1         = json.RawMessage(`{"device_id":"B","signatures":{"@u:remote":{"ed25519:B":"one"}}}`)
	masterUnsigned = json.RawMessage(`{"usage":["master"]}`)
	master1        = json.RawMessage(`{"usage":["master"],"signatures":{"@u:remote":{"ed25519:X":"one"}}}`)
	master1Signed  = json.RawMessage(`{"usage":["master"],"signatures":{"@u:remote":{"ed25519:A":"device","ed25519:X":"one"}}}`)
	master2        = json.RawMessage(`{"usage":["master"],"signatures":{"@u:remote":{"ed25519:X":"two"}}}`)
	selfSig1       = json.RawMessage(`{"usage":["self_signing"]}`)
)

// acceptedInOrder is stored history for stream IDs accepted in the given order at the same time
func acceptedInOrder(at time.Time, streamIDs ...int64) map[int64]acceptance {
	history := make(map[int64]acceptance, len(streamIDs))
	for i, streamID := range streamIDs {
		history[streamID] = acceptance{at: at, seq: int64(i + 1)}
	}
	return history
}

var testWrittenAt = testNow.Add(-time.Minute).UnixMilli()

func validState(streamID int64, devices map[id.DeviceID]*types.RemoteDevice) ingestState {
	return ingestState{
		meta: meta{
			WrittenAt: testWrittenAt, Generation: 3, StreamID: &streamID, DeviceCount: len(devices), AcceptSeq: 1,
		},
		history: acceptedInOrder(testNow.Add(-time.Minute), streamID),
		devices: devices,
	}
}

// applyPlan stores a plan's accepted updates as TxnIngest does
func applyPlan(t *testing.T, state ingestState, plan ingestPlan, now time.Time) ingestState {
	t.Helper()
	require.False(t, plan.wipe)
	next := state
	next.devices = maps.Clone(state.devices)
	maps.Copy(next.devices, plan.devices)
	next.history = maps.Clone(state.history)
	for _, streamID := range plan.accepted {
		next.meta.AcceptSeq++
		next.history[streamID] = acceptance{at: now, seq: next.meta.AcceptSeq}
	}
	for _, streamID := range historyToPrune(next.history, now) {
		delete(next.history, streamID)
	}
	current := plan.accepted[len(plan.accepted)-1]
	next.meta.StreamID = &current
	next.meta.DeviceCount = plan.deviceCount
	return next
}

func deviceEDU(deviceID id.DeviceID, streamID int64, prevIDs []int64, keys json.RawMessage) types.RemoteDeviceEDU {
	return types.RemoteDeviceEDU{DeviceList: &types.RemoteDeviceListUpdate{
		DeviceID: deviceID,
		StreamID: streamID,
		PrevIDs:  prevIDs,
		Device:   types.RemoteDevice{DisplayName: "device", Keys: keys},
	}}
}

func deletionEDU(deviceID id.DeviceID, streamID int64, prevIDs []int64) types.RemoteDeviceEDU {
	return types.RemoteDeviceEDU{DeviceList: &types.RemoteDeviceListUpdate{
		DeviceID: deviceID, StreamID: streamID, PrevIDs: prevIDs, Deleted: true,
	}}
}

func signingKeysEDU(master, selfSigning json.RawMessage) types.RemoteDeviceEDU {
	return types.RemoteDeviceEDU{SigningKeys: &types.RemoteSigningKeyUpdate{MasterKey: master, SelfSigningKey: selfSigning}}
}

func storedDeviceOf(keys json.RawMessage) *types.RemoteDevice {
	return &types.RemoteDevice{DisplayName: "device", Keys: keys}
}

func assertWipe(t *testing.T, plan ingestPlan) {
	t.Helper()
	assert.Equal(t, ingestPlan{wipe: true, refetch: true}, plan)
}

func assertRefetch(t *testing.T, plan ingestPlan) {
	t.Helper()
	assert.Equal(t, ingestPlan{refetch: true}, plan)
}

// assertNoWrite checks TxnIngest would write nothing for the plan
func assertNoWrite(t *testing.T, plan ingestPlan) {
	t.Helper()
	assert.False(t, plan.wipe)
	assert.False(t, plan.refetch)
	assert.Empty(t, plan.accepted)
	assert.Empty(t, plan.devices)
	assert.Nil(t, plan.signingKeys)
}

func TestConnectedChangeStoresAndNotifies(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, []int64{5}, keysA2)}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{6}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)}, plan.devices)
	assert.Equal(t, state.meta.DeviceCount, plan.deviceCount)
}

func TestAddedSignatureUnderSameSignerIsAChange(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, []int64{5}, keysA1Signed)}, testNow)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1Signed)}, plan.devices)

	state.signingKeys = storedSigningKeys{MasterKey: master1}
	plan = planIngest(state, []types.RemoteDeviceEDU{signingKeysEDU(master1Signed, nil)}, testNow)
	assert.Equal(t, &storedSigningKeys{MasterKey: master1Signed}, plan.signingKeys)
}

func TestIdenticalContentAdvancesSilentlyAndSuccessorConnects(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, []int64{5}, keysA1)}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{6}, plan.accepted, "an unchanged delta still advances the stream")
	assert.Empty(t, plan.devices)

	plan = planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, []int64{5}, keysA1),
		deviceEDU("A", 7, []int64{6}, keysA2),
	}, testNow)
	assert.Equal(t, []int64{6, 7}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)}, plan.devices)

	next := applyPlan(t, state, planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, []int64{5}, keysA1)}, testNow), testNow)
	plan = planIngest(next, []types.RemoteDeviceEDU{deviceEDU("A", 7, []int64{6}, keysA2)}, testNow)
	assert.Equal(t, []int64{7}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)}, plan.devices)
}

func TestKnownReplayIsSilentAndDoesNotRollBack(t *testing.T) {
	state := validState(7, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)})
	state.history = acceptedInOrder(testNow.Add(-2*time.Minute), 6, 7)

	plan := planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, []int64{5}, keysA1),
		deletionEDU("A", 6, []int64{5}),
		deviceEDU("A", 7, []int64{6}, keysA1),
	}, testNow)
	assertNoWrite(t, plan)

	invalid := deviceEDU("A", 6, []int64{5}, nil)
	invalid.DeviceList.Invalid = true
	plan = planIngest(state, []types.RemoteDeviceEDU{invalid}, testNow)
	assertNoWrite(t, plan)
}

func TestReplayWithinBatchIsSilent(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	plan := planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, []int64{5}, keysA2),
		deviceEDU("A", 6, []int64{5}, keysA1),
	}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{6}, plan.accepted)
	assert.Equal(t, storedDeviceOf(keysA2), plan.devices["A"])
}

func TestUnknownOldHistoryResyncs(t *testing.T) {
	state := validState(7, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)})
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 3, []int64{2}, keysA2)}, testNow)
	assertWipe(t, plan)
}

func TestMissingOrEmptyPrevWipesEvenWithIdenticalContent(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	for _, prevIDs := range [][]int64{nil, {}} {
		plan := planIngest(state, []types.RemoteDeviceEDU{
			deviceEDU("A", 6, prevIDs, keysA1),
			deviceEDU("A", 7, []int64{6}, keysA1),
			signingKeysEDU(master1, nil),
		}, testNow)
		assertWipe(t, plan)
	}
}

func TestMissedDeletionThenUnchangedDeviceWipes(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{
		"A": storedDeviceOf(keysA1),
		"B": storedDeviceOf(keysB1),
	})
	// Stream 6 deleted A and never arrived: B's equality says nothing about A
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("B", 7, []int64{6}, keysB1)}, testNow)
	assertWipe(t, plan)
}

func TestAcceptedUpdatesBeforeAWipeAreDiscarded(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	plan := planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, []int64{5}, keysA2),
		signingKeysEDU(master1, nil),
		deviceEDU("A", 8, []int64{7}, keysA2),
		deviceEDU("A", 9, []int64{8}, keysA2),
	}, testNow)
	assertWipe(t, plan)
}

func TestBranchOnRetainedOlderPredecessorConnects(t *testing.T) {
	state := validState(7, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	state.history = acceptedInOrder(testNow.Add(-5*time.Minute), 5, 6, 7)

	plan := planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("B", 8, []int64{5}, keysB1),
		deviceEDU("A", 9, []int64{6, 8}, keysA2),
	}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{8, 9}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2), "B": storedDeviceOf(keysB1)}, plan.devices)
	assert.Equal(t, state.meta.DeviceCount+1, plan.deviceCount)
}

func TestExpiredPredecessorRefetches(t *testing.T) {
	state := validState(7, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	state.history = map[int64]acceptance{
		5: {at: testNow.Add(-types.RemoteDeviceStreamHistoryRetention - time.Millisecond), seq: 1},
		7: {at: testNow.Add(-time.Hour), seq: 2},
	}

	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 8, []int64{5}, keysA2)}, testNow)
	assertWipe(t, plan)

	plan = planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 8, []int64{7}, keysA2)}, testNow)
	assert.False(t, plan.wipe, "the current stream ID stays known after its history entry expires")
	assert.Equal(t, []int64{8}, plan.accepted)
}

func TestPrunedPredecessorRefetches(t *testing.T) {
	streamIDs := make([]int64, types.MaxRemoteDeviceStreamHistory+1)
	for i := range streamIDs {
		streamIDs[i] = int64((i*7919)%1000 - 500)
	}
	history := acceptedInOrder(testNow.Add(-time.Minute), streamIDs...)
	pruned := historyToPrune(history, testNow)
	require.Equal(t, []int64{streamIDs[0]}, pruned, "the first accepted, whatever its value")
	delete(history, pruned[0])

	state := validState(streamIDs[len(streamIDs)-1], nil)
	state.history = history
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 1001, []int64{streamIDs[0]}, keysA1)}, testNow)
	assertWipe(t, plan)
}

func TestHistoryIsPrunedInAcceptanceOrder(t *testing.T) {
	expiredAt := testNow.Add(-types.RemoteDeviceStreamHistoryRetention - time.Second)
	history := map[int64]acceptance{
		1: {at: expiredAt, seq: 1},
		2: {at: testNow.Add(-types.RemoteDeviceStreamHistoryRetention), seq: 2},
		3: {at: testNow, seq: 3},
	}
	assert.Equal(t, []int64{1}, historyToPrune(history, testNow))

	history = map[int64]acceptance{
		9: {at: testNow, seq: 1},
		4: {at: expiredAt, seq: 2},
		6: {at: testNow, seq: 3},
	}
	assert.Equal(t, []int64{9, 4}, historyToPrune(history, testNow),
		"an expiry prunes everything accepted before it, so known IDs stay a suffix of acceptance order")

	streamIDs := make([]int64, types.MaxRemoteDeviceStreamHistory)
	for i := range streamIDs {
		streamIDs[i] = int64(1000 - i)
	}
	history = acceptedInOrder(testNow, append([]int64{500, 100, 600}, streamIDs...)...)
	assert.Equal(t, []int64{500, 100, 600}, historyToPrune(history, testNow),
		"one batch shares a millisecond, so only acceptance order decides")
}

func TestPruningNeverLetsAReplayRollBack(t *testing.T) {
	state := validState(1000, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	chain := []types.RemoteDeviceEDU{
		deviceEDU("A", 500, []int64{1000}, keysA2),
		deviceEDU("A", 100, []int64{500}, keysA1),
		deviceEDU("A", 600, []int64{100}, keysA2),
	}
	state = applyPlan(t, state, planIngest(state, chain, testNow), testNow)

	// Two more batches push history over its cap by two: the anchor and 500 are pruned
	prevID := int64(600)
	for batch := range 2 {
		edus := make([]types.RemoteDeviceEDU, 0, 99)
		for range 99 {
			streamID := 2000 + prevID
			edus = append(edus, deviceEDU("B", streamID, []int64{prevID}, keysB1))
			prevID = streamID
		}
		at := testNow.Add(time.Duration(batch+1) * time.Second)
		state = applyPlan(t, state, planIngest(state, edus, at), at)
	}
	require.Len(t, state.history, types.MaxRemoteDeviceStreamHistory)
	_, known500 := state.history[500]
	_, known100 := state.history[100]
	require.False(t, known500)
	require.True(t, known100)
	require.Equal(t, storedDeviceOf(keysA2), state.devices["A"])

	for _, edu := range chain {
		plan := planIngest(state, []types.RemoteDeviceEDU{edu}, testNow.Add(3*time.Second))
		assert.Empty(t, plan.accepted, "replay of %d", edu.DeviceList.StreamID)
		assert.Empty(t, plan.devices, "replay of %d", edu.DeviceList.StreamID)
	}
	plan := planIngest(state, []types.RemoteDeviceEDU{chain[1]}, testNow.Add(3*time.Second))
	assertNoWrite(t, plan)
}

func TestStreamIDsNeedNotIncrease(t *testing.T) {
	state := validState(100, nil)
	plan := planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 3, []int64{100}, keysA1),
		deviceEDU("A", -7, []int64{3}, keysA2),
		deletionEDU("A", 50, []int64{-7}),
	}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{3, -7, 50}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": nil}, plan.devices)
	assert.Equal(t, state.meta.DeviceCount, plan.deviceCount)
}

func TestInvalidConnectedPayloadIsNeverAccepted(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})
	invalid := deviceEDU("A", 6, []int64{5}, nil)
	invalid.DeviceList.Invalid = true
	plan := planIngest(state, []types.RemoteDeviceEDU{invalid, deviceEDU("A", 7, []int64{6}, keysA2)}, testNow)
	assertWipe(t, plan)

	longest := id.DeviceID(strings.Repeat("D", types.MaxRemoteDeviceIDBytes))
	plan = planIngest(state, []types.RemoteDeviceEDU{deviceEDU(longest, 6, []int64{5}, nil)}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{longest: storedDeviceOf(nil)}, plan.devices)

	// An over-long device ID cannot be stored
	plan = planIngest(state, []types.RemoteDeviceEDU{deletionEDU(longest+"D", 6, []int64{5})}, testNow)
	assertWipe(t, plan)
}

func TestContinuityIsDecidedBeforeReplay(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})

	// A known stream ID without predecessors is a reset, not a replay
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 5, nil, keysA1)}, testNow)
	assertWipe(t, plan)

	// A known stream ID is a replay however many predecessors it names
	prevIDs := make([]int64, types.MaxRemoteDevicePrevIDs+1)
	plan = planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 5, prevIDs, keysA1)}, testNow)
	assertNoWrite(t, plan)
}

func TestOverLimitInputIsNeverAccepted(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1)})

	prevIDs := make([]int64, types.MaxRemoteDevicePrevIDs+1)
	for i := range prevIDs {
		prevIDs[i] = 5
	}
	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, prevIDs, keysA2)}, testNow)
	assertWipe(t, plan)

	edus := make([]types.RemoteDeviceEDU, types.MaxRemoteDeviceEDUsPerIngest+1)
	for i := range edus {
		edus[i] = deviceEDU("A", int64(6+i), []int64{int64(5 + i)}, keysA2)
	}
	plan = planIngest(state, edus, testNow)
	assertWipe(t, plan)

	noSnapshot := state
	noSnapshot.meta.StreamID = nil
	assertRefetch(t, planIngest(noSnapshot, edus, testNow))
}

func TestDeviceCreationPastLimitIsOverLimit(t *testing.T) {
	state := validState(5, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA1), "B": nil})
	state.meta.DeviceCount = types.MaxRemoteDevicesPerUser

	plan := planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 6, []int64{5}, keysA2)}, testNow)
	assert.False(t, plan.wipe, "updating an existing device")
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(keysA2)}, plan.devices)

	plan = planIngest(state, []types.RemoteDeviceEDU{
		deletionEDU("A", 6, []int64{5}),
		deviceEDU("B", 7, []int64{6}, keysB1),
	}, testNow)
	assert.False(t, plan.wipe)
	assert.Equal(t, []int64{6, 7}, plan.accepted)
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": nil, "B": storedDeviceOf(keysB1)}, plan.devices)
	assert.Equal(t, state.meta.DeviceCount, plan.deviceCount)

	plan = planIngest(state, []types.RemoteDeviceEDU{deviceEDU("B", 6, []int64{5}, keysB1)}, testNow)
	assertWipe(t, plan)
}

func TestZeroDeviceSnapshotIsDistinctFromAMiss(t *testing.T) {
	state := validState(0, nil)
	plan := planIngest(state, []types.RemoteDeviceEDU{
		deletionEDU("A", 1, []int64{0}),
		deviceEDU("A", 2, []int64{1}, nil),
	}, testNow)
	assert.Equal(t, []int64{1, 2}, plan.accepted, "deleting an absent device is accepted unchanged")
	assert.Equal(t, map[id.DeviceID]*types.RemoteDevice{"A": storedDeviceOf(nil)}, plan.devices)
	assert.Equal(t, state.meta.DeviceCount+1, plan.deviceCount)

	state.meta.StreamID = nil
	assertRefetch(t, planIngest(state, []types.RemoteDeviceEDU{deviceEDU("A", 2, []int64{1}, nil)}, testNow))
}

func TestEDUWithoutSnapshotRefetches(t *testing.T) {
	state := ingestState{meta: meta{WrittenAt: testWrittenAt, Generation: 4}}
	assertRefetch(t, planIngest(state, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, nil, keysA1),
		signingKeysEDU(master1, nil),
	}, testNow))

	invalid := signingKeysEDU(master2, nil)
	invalid.SigningKeys.Invalid = true
	assertRefetch(t, planIngest(state, []types.RemoteDeviceEDU{invalid}, testNow))
}

func TestUncachedIgnoresEverything(t *testing.T) {
	uncached := validState(5, nil)
	uncached.meta.WrittenAt = 0
	plan := planIngest(uncached, []types.RemoteDeviceEDU{
		deviceEDU("A", 6, nil, keysA1),
		deviceEDU("A", 6, []int64{5}, keysA1),
		signingKeysEDU(master1, nil),
	}, testNow)
	assert.Equal(t, ingestPlan{}, plan)

	plan = planIngest(validState(5, nil), nil, testNow)
	assert.Equal(t, ingestPlan{}, plan)
}

func TestSigningKeyUpdates(t *testing.T) {
	state := validState(5, nil)
	state.signingKeys = storedSigningKeys{MasterKey: master1, SelfSigningKey: selfSig1}

	plan := planIngest(state, []types.RemoteDeviceEDU{signingKeysEDU(master1, selfSig1)}, testNow)
	assertNoWrite(t, plan)

	plan = planIngest(state, []types.RemoteDeviceEDU{signingKeysEDU(master2, nil)}, testNow)
	assert.Equal(t, &storedSigningKeys{MasterKey: master2, SelfSigningKey: selfSig1}, plan.signingKeys,
		"a changed signature under the same signer is a change, and an omitted key is never a deletion")
	assert.Empty(t, plan.accepted, "signing keys have no stream ID")

	plan = planIngest(state, []types.RemoteDeviceEDU{signingKeysEDU(master2, nil), signingKeysEDU(nil, nil)}, testNow)
	assert.Equal(t, &storedSigningKeys{MasterKey: master2, SelfSigningKey: selfSig1}, plan.signingKeys)

	keyless := validState(5, nil)
	plan = planIngest(keyless, []types.RemoteDeviceEDU{signingKeysEDU(masterUnsigned, nil)}, testNow)
	assert.Equal(t, &storedSigningKeys{MasterKey: masterUnsigned}, plan.signingKeys, "an unsigned master key")
	plan = planIngest(keyless, []types.RemoteDeviceEDU{signingKeysEDU(nil, selfSig1)}, testNow)
	assert.Equal(t, &storedSigningKeys{SelfSigningKey: selfSig1}, plan.signingKeys, "a self-signing key without a master")

	invalid := signingKeysEDU(master2, nil)
	invalid.SigningKeys.Invalid = true
	assertWipe(t, planIngest(state, []types.RemoteDeviceEDU{invalid}, testNow))
}
