package state

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const testRoomID = id.RoomID("!room:example.com")

var (
	createTup = types.StateTup{Type: event.StateCreate}
	rulesTup  = types.StateTup{Type: event.StateJoinRules}
	powerTup  = types.StateTup{Type: event.StatePowerLevels}
	topicTup  = types.StateTup{Type: event.StateTopic}
)

func userID(i int) id.UserID {
	return id.UserID(fmt.Sprintf("@user%d:example.com", i))
}

func initialState() types.StateMap {
	state := types.StateMap{
		createTup: "$create",
		rulesTup:  "$rules",
		powerTup:  "$power",
	}
	for i := range 12 {
		state[types.MemberStateTup(userID(i))] = id.EventID(fmt.Sprintf("$join%d", i))
	}
	return state
}

var testMemberships = []event.Membership{
	event.MembershipJoin, event.MembershipInvite, event.MembershipLeave, event.MembershipBan, event.MembershipKnock,
}

// testMembership is the membership of a test member event, a function of its ID as a real event's
// is: the one its ID starts with after the $, otherwise one picked by the ID's hash.
func testMembership(eventID id.EventID) event.Membership {
	for _, membership := range testMemberships {
		if strings.HasPrefix(string(eventID), "$"+string(membership)) {
			return membership
		}
	}
	return testMemberships[bucketOf([]byte(eventID))%uint64(len(testMemberships))]
}

// withMemberships gives each member set in a test state or delta its event's membership
func withMemberships(stateMap types.StateMap) types.StateEntries {
	out := make(types.StateEntries, len(stateMap))
	for tup, eventID := range stateMap {
		entry := types.StateEntry{EventID: eventID}
		if tup.Type == event.StateMember && eventID != "" {
			entry.Membership = testMembership(eventID)
		}
		out[tup] = entry
	}
	return out
}

func getContext(b *Batch, ctx types.StateHash) (stateContext, error) {
	return b.context(b.store.reader(nil, b.roomID), ctx)
}

func iterateEventIDs(b *Batch, ctx types.StateHash) (types.StateMap, error) {
	entries, err := b.TxnIterateEntries(nil, ctx)
	return entries.EventIDs(), err
}

func lookupEventIDs(b *Batch, ctx types.StateHash, tups []types.StateTup) (types.StateMap, error) {
	entries, err := b.TxnLookupEntries(nil, ctx, tups)
	return entries.EventIDs(), err
}

func TestContextRoutesStateAndMembers(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	state := initialState()

	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(state))
	require.NoError(t, err)
	c, err := getContext(b, ctx)
	require.NoError(t, err)
	assert.Equal(t, c.ID(), ctx)

	h := &treeHarness{t: t, store: store.room(testRoomID), cache: b.tree.cache, limits: smallLimits}
	b.TxnWrite(fdb.Transaction{}, ctx)
	assert.Equal(t, map[string]string{
		string(packStateKey(createTup)): "$create",
		string(packStateKey(rulesTup)):  "$rules",
		string(packStateKey(powerTup)):  "$power",
	}, h.contents(c.stateRoot))
	assert.Len(t, h.contents(c.memberRoot), 12)
	assert.Equal(t, "\x01$join3", h.contents(c.memberRoot)[userID(3).String()], "a member value is its membership code then its event ID")

	all, err := iterateEventIDs(b, ctx)
	require.NoError(t, err)
	assert.Equal(t, state, all)
	allEntries, err := b.TxnIterateEntries(nil, ctx)
	require.NoError(t, err)
	assert.Equal(t, withMemberships(state), allEntries)

	found, err := lookupEventIDs(b, ctx, []types.StateTup{
		createTup,
		topicTup,
		types.MemberStateTup(userID(4)),
		types.MemberStateTup("@absent:example.com"),
		{Type: event.StateMember},
	})
	require.NoError(t, err)
	assert.Equal(t, types.StateMap{createTup: "$create", types.MemberStateTup(userID(4)): "$join4"}, found)

	foundEntries, err := b.TxnLookupEntries(nil, ctx, []types.StateTup{createTup, types.MemberStateTup(userID(4)), topicTup})
	require.NoError(t, err)
	assert.Equal(t, types.StateEntries{
		createTup:                       {EventID: "$create"},
		types.MemberStateTup(userID(4)): {EventID: "$join4", Membership: event.MembershipJoin},
	}, foundEntries)
}

func TestMemberValueEncoding(t *testing.T) {
	for code, membership := range map[byte]event.Membership{
		1: event.MembershipJoin, 2: event.MembershipInvite, 3: event.MembershipLeave, 4: event.MembershipBan, 5: event.MembershipKnock,
	} {
		entry := types.StateEntry{EventID: "$event", Membership: membership}
		value, err := encodeValue(memberIndex, entry)
		require.NoError(t, err)
		assert.Equal(t, append([]byte{code}, "$event"...), value, "the codes are part of the format")
		decoded, err := decodeValue(memberIndex, value)
		require.NoError(t, err)
		assert.Equal(t, entry, decoded)
	}

	for _, value := range [][]byte{{}, {1}, append([]byte{0}, "$event"...), append([]byte{6}, "$event"...), []byte("$event")} {
		_, err := decodeValue(memberIndex, value)
		assert.ErrorIs(t, err, ErrInvalidPage, "member value %q", value)
	}
	decoded, err := decodeValue(stateIndex, []byte("$event"))
	require.NoError(t, err)
	assert.Equal(t, types.StateEntry{EventID: "$event"}, decoded)

	for m, entry := range map[int]types.StateEntry{
		memberIndex: {EventID: "$event"},
		stateIndex:  {EventID: "$event", Membership: event.MembershipJoin},
	} {
		_, err := encodeValue(m, entry)
		assert.ErrorIs(t, err, ErrInvalidEntry, "map %d entry %v", m, entry)
	}
	for _, entry := range []types.StateEntry{
		{EventID: "$event", Membership: "custom"},
		{Membership: event.MembershipLeave},
	} {
		_, err := encodeValue(memberIndex, entry)
		assert.ErrorIs(t, err, ErrInvalidEntry, "member entry %v", entry)
	}
	removal, err := encodeValue(memberIndex, types.StateEntry{})
	require.NoError(t, err)
	assert.Nil(t, removal)
}

func TestContextApplyRejectsEntriesWithoutValidMembership(t *testing.T) {
	b := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	pending := len(b.tree.pending)

	member := types.MemberStateTup(userID(50))
	for _, delta := range []types.StateEntries{
		{member: {EventID: "$join50"}},
		{member: {EventID: "$join50", Membership: "custom"}},
		{topicTup: {EventID: "$topic", Membership: event.MembershipJoin}},
		{member: {Membership: event.MembershipLeave}},
		{topicTup: {EventID: "$topic"}, member: {EventID: "$join50"}},
	} {
		_, err := b.TxnApply(nil, ctx, delta)
		assert.ErrorIs(t, err, ErrInvalidEntry, "delta %v", delta)
	}
	assert.Len(t, b.tree.pending, pending, "a rejected delta builds nothing")
}

// Memberships are part of the values, so the roots still depend only on the contents: applying a
// state's entries one at a time in any order gives the context building it at once gives.
func TestContextWithMembershipsIndependentOfBuildOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 7))
	state := initialState()
	for i := range 150 {
		state[types.MemberStateTup(userID(100+i))] = id.EventID(fmt.Sprintf("$%d", i))
	}
	want, err := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits).TxnApply(nil, EmptyContext, withMemberships(state))
	require.NoError(t, err)

	tups := slices.Collect(maps.Keys(state))
	for trial := range 5 {
		rng.Shuffle(len(tups), func(i, j int) { tups[i], tups[j] = tups[j], tups[i] })
		b := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)
		ctx := EmptyContext
		for _, tup := range tups {
			ctx, err = b.TxnApply(nil, ctx, withMemberships(types.StateMap{tup: state[tup]}))
			require.NoError(t, err)
		}
		require.Equal(t, want, ctx, "trial %d", trial)
	}
}

func TestContextApplyAndDiff(t *testing.T) {
	b := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)
	before, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)

	after, err := b.TxnApply(nil, before, withMemberships(types.StateMap{
		powerTup:                         "$power2",
		topicTup:                         "$topic",
		types.MemberStateTup(userID(2)):  "",
		types.MemberStateTup(userID(5)):  "$leave5",
		types.MemberStateTup(userID(9)):  "$join9",
		types.MemberStateTup(userID(99)): "",
	}))
	require.NoError(t, err)

	expected := initialState()
	expected[powerTup] = "$power2"
	expected[topicTup] = "$topic"
	delete(expected, types.MemberStateTup(userID(2)))
	expected[types.MemberStateTup(userID(5))] = "$leave5"
	state, err := iterateEventIDs(b, after)
	require.NoError(t, err)
	assert.Equal(t, expected, state)

	unchanged, err := iterateEventIDs(b, before)
	require.NoError(t, err)
	assert.Equal(t, initialState(), unchanged)

	changes, err := b.TxnDiff(nil, before, after)
	require.NoError(t, err)
	assert.Equal(t, []types.StateChange{
		{StateTup: types.MemberStateTup(userID(2)), OldEventID: "$join2", OldMembership: event.MembershipJoin},
		{
			StateTup:   types.MemberStateTup(userID(5)),
			OldEventID: "$join5", NewEventID: "$leave5",
			OldMembership: event.MembershipJoin, NewMembership: event.MembershipLeave,
		},
		{StateTup: powerTup, OldEventID: "$power", NewEventID: "$power2"},
		{StateTup: topicTup, OldEventID: "", NewEventID: "$topic"},
	}, changes)

	reverse, err := b.TxnDiff(nil, after, before)
	require.NoError(t, err)
	require.Len(t, reverse, len(changes))
	for i, change := range reverse {
		assert.Equal(t, changes[i].StateTup, change.StateTup)
		assert.Equal(t, changes[i].NewEventID, change.OldEventID)
		assert.Equal(t, changes[i].OldEventID, change.NewEventID)
		assert.Equal(t, changes[i].NewMembership, change.OldMembership)
		assert.Equal(t, changes[i].OldMembership, change.NewMembership)
	}

	fromEmpty, err := b.TxnDiff(nil, EmptyContext, before)
	require.NoError(t, err)
	assert.Len(t, fromEmpty, len(initialState()))

	same, err := b.TxnDiff(nil, after, after)
	require.NoError(t, err)
	assert.Empty(t, same)
}

func TestStateSinceIsTheDiffLessTheTimeline(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	from, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	to, err := b.TxnApply(nil, from, withMemberships(types.StateMap{
		topicTup:                         "$topic",
		powerTup:                         "$power2",
		types.MemberStateTup(userID(3)):  "$leave3",
		types.MemberStateTup(userID(4)):  "",
		types.MemberStateTup(userID(12)): "$join12",
	}))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, from, to)
	timeline := map[id.EventID]struct{}{"$message": {}, "$topic": {}, "$join12": {}}

	since, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, from, []types.StateHash{to}, timeline)
	require.NoError(t, err)
	assert.Equal(t, []types.EventStateTup{
		{StateTup: types.MemberStateTup(userID(3)), EventID: "$leave3"},
		{StateTup: powerTup, EventID: "$power2"},
	}, since, "changes the timeline holds are left out, and so is a removed member")

	store.resetCounts()
	same, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, to, []types.StateHash{to}, timeline)
	require.NoError(t, err)
	assert.Empty(t, same)
	assert.Zero(t, store.contextReads+store.pageReads, "equal contexts are not read")

	all, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, EmptyContext, []types.StateHash{from}, timeline)
	require.NoError(t, err)
	assert.Len(t, all, len(initialState()), "before the room's first state everything is new")
}

func TestStateSinceSendsAChangeAfterTheTimelineStart(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	atFrom, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	// The timeline starts at a message, then its second event's step resolves power levels to a fork's
	// event the timeline does not hold
	atStart := atFrom
	atEnd, err := b.TxnApply(nil, atStart, withMemberships(types.StateMap{powerTup: "$forkpower"}))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, atFrom, atEnd)
	timeline := map[id.EventID]struct{}{"$message1": {}, "$message2": {}}

	toStart, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, atFrom, []types.StateHash{atStart}, timeline)
	require.NoError(t, err)
	assert.Empty(t, toStart)

	since, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, atFrom, []types.StateHash{atStart, atEnd}, timeline)
	require.NoError(t, err)
	assert.Equal(t, []types.EventStateTup{{StateTup: powerTup, EventID: "$forkpower"}}, since)
}

func TestStateSinceTakesTheLastContextsEvent(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	atFrom, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	atStart, err := b.TxnApply(nil, atFrom, withMemberships(types.StateMap{topicTup: "$topic1", rulesTup: "$rules1"}))
	require.NoError(t, err)
	atEnd, err := b.TxnApply(nil, atStart, withMemberships(types.StateMap{topicTup: "$topic2", rulesTup: "$rules2"}))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, atFrom, atStart, atEnd)
	timeline := map[id.EventID]struct{}{"$rules2": {}}

	since, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, atFrom, []types.StateHash{atStart, atEnd}, timeline)
	require.NoError(t, err)
	assert.Equal(t, []types.EventStateTup{
		{StateTup: rulesTup, EventID: "$rules1"},
		{StateTup: topicTup, EventID: "$topic2"},
	}, since, "the end's event replaces the start's unless the timeline holds it")

	store.resetCounts()
	_, err = newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, atFrom, []types.StateHash{atEnd}, timeline)
	require.NoError(t, err)
	once := store.pageReads + store.contextReads
	store.resetCounts()
	_, err = newBatch(store, newCache(0), testRoomID, smallLimits).TxnStateSince(nil, atFrom, []types.StateHash{atFrom, atEnd, atEnd}, timeline)
	require.NoError(t, err)
	assert.Equal(t, once, store.pageReads+store.contextReads, "contexts equal to from or to the one before are not diffed")
}

func TestIterateStateReadsOnlyNonMemberState(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, ctx)

	store.resetCounts()
	_, err = iterateEventIDs(newBatch(store, newCache(0), testRoomID, smallLimits), ctx)
	require.NoError(t, err)
	allPages := store.pageReads

	store.resetCounts()
	stateMap, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnIterateState(nil, ctx)
	require.NoError(t, err)
	assert.Equal(t, types.StateMap{createTup: "$create", rulesTup: "$rules", powerTup: "$power"}, stateMap)
	assert.Less(t, store.pageReads, allPages)

	empty, err := b.TxnIterateState(nil, EmptyContext)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestIterateMembersReadsOnlyTheMemberMap(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, ctx)

	store.resetCounts()
	all, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnIterateEntries(nil, ctx)
	require.NoError(t, err)
	allPages := store.pageReads

	store.resetCounts()
	members, err := newBatch(store, newCache(0), testRoomID, smallLimits).TxnIterateMembers(nil, ctx)
	require.NoError(t, err)
	assert.Less(t, store.pageReads, allPages)
	require.Len(t, members, 12)
	for userID, member := range members {
		assert.Equal(t, all[types.MemberStateTup(userID)], member)
	}

	empty, err := b.TxnIterateMembers(nil, EmptyContext)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestMembersPagesCoverEveryMemberOnce(t *testing.T) {
	stateMap := types.StateMap{createTup: "$create"}
	for i := range 300 {
		stateMap[types.MemberStateTup(userID(i))] = id.EventID(fmt.Sprintf("$join%d", i))
	}
	store := newMemStore()
	b := newBatch(store, newCache(0), testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(stateMap))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, ctx)
	all, err := b.TxnIterateMembers(nil, ctx)
	require.NoError(t, err)

	for _, limit := range []int{1, 7, 64, 1000} {
		seen := make(map[id.UserID]types.StateEntry)
		pages := 0
		for from, more := uint64(0), true; more; pages++ {
			var page map[id.UserID]types.StateEntry
			page, from, more, err = newBatch(store, newCache(0), testRoomID, smallLimits).TxnMembersPage(nil, ctx, from, limit)
			require.NoError(t, err)
			if more {
				assert.GreaterOrEqual(t, len(page), limit, "only the last page holds fewer than the limit")
			}
			for userID, member := range page {
				_, again := seen[userID]
				assert.False(t, again, "%s in two pages", userID)
				seen[userID] = member
			}
		}
		assert.Equal(t, all, seen, "limit %d", limit)
		if limit < len(all) {
			assert.Greater(t, pages, 1, "limit %d", limit)
		}
	}

	page, _, more, err := b.TxnMembersPage(nil, EmptyContext, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.False(t, more)
}

func TestContextApplyWithoutChangesKeepsContext(t *testing.T) {
	b := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	pending := len(b.tree.pending)

	same, err := b.TxnApply(nil, ctx, withMemberships(types.StateMap{createTup: "$create", topicTup: ""}))
	require.NoError(t, err)
	assert.Equal(t, ctx, same)
	assert.Len(t, b.tree.pending, pending)

	empty, err := b.TxnApply(nil, EmptyContext, withMemberships(types.StateMap{createTup: ""}))
	require.NoError(t, err)
	assert.Equal(t, EmptyContext, empty)
}

func TestContextWriteAndReload(t *testing.T) {
	store := newMemStore()
	cache := newCache(defaultCacheBytes)
	b := newBatch(store, cache, testRoomID, smallLimits)

	first, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	intermediate, err := b.TxnApply(nil, first, withMemberships(types.StateMap{topicTup: "$topic1"}))
	require.NoError(t, err)
	last, err := b.TxnApply(nil, intermediate, withMemberships(types.StateMap{topicTup: "$topic2", types.MemberStateTup(userID(50)): "$join50"}))
	require.NoError(t, err)

	found, err := lookupEventIDs(b, last, []types.StateTup{topicTup, types.MemberStateTup(userID(50))})
	require.NoError(t, err)
	assert.Equal(t, types.StateMap{topicTup: "$topic2", types.MemberStateTup(userID(50)): "$join50"}, found)
	assert.Empty(t, store.pages)
	assert.Zero(t, store.pageReads)

	b.TxnWrite(fdb.Transaction{}, first, last, EmptyContext)
	assert.Len(t, store.contexts, 2)
	written := maps.Clone(store.pages)
	b.TxnWrite(fdb.Transaction{}, first, last)
	assert.Equal(t, written, store.pages, "a retried write stores the same records")

	fresh := newBatch(store, cache, testRoomID, smallLimits)
	expected := initialState()
	expected[topicTup] = "$topic2"
	expected[types.MemberStateTup(userID(50))] = "$join50"
	state, err := iterateEventIDs(fresh, last)
	require.NoError(t, err)
	assert.Equal(t, expected, state)
	state, err = iterateEventIDs(fresh, first)
	require.NoError(t, err)
	assert.Equal(t, initialState(), state)

	_, err = getContext(fresh, intermediate)
	assert.ErrorIs(t, err, ErrContextNotFound)

	otherRoom := newBatch(store, cache, "!other:example.com", smallLimits)
	_, err = iterateEventIDs(otherRoom, last)
	assert.ErrorIs(t, err, ErrContextNotFound)
	store.contexts[store.room(otherRoom.roomID).key(last)] = store.contexts[store.room(testRoomID).key(last)]
	_, err = iterateEventIDs(otherRoom, last)
	assert.ErrorIs(t, err, ErrPageNotFound, "pages cached for one room are not used by another")
}

func TestContextIterateAllSharesRoundTrips(t *testing.T) {
	store := newMemStore()
	cache := newCache(0)
	b := newBatch(store, cache, testRoomID, smallLimits)
	first, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	second, err := b.TxnApply(nil, first, withMemberships(types.StateMap{topicTup: "$topic", types.MemberStateTup(userID(3)): ""}))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, first, second)

	fresh := func() *Batch { return newBatch(store, cache, testRoomID, smallLimits) }
	store.resetCounts()
	firstState, err := iterateEventIDs(fresh(), first)
	require.NoError(t, err)
	singleReadCalls := store.pageReadCalls
	secondState, err := iterateEventIDs(fresh(), second)
	require.NoError(t, err)

	store.resetCounts()
	states, err := fresh().iterateAll(store.room(testRoomID), first, EmptyContext, second)
	require.NoError(t, err)
	assert.Equal(t, []types.StateEntries{withMemberships(firstState), {}, withMemberships(secondState)}, states)
	assert.Equal(t, singleReadCalls, store.pageReadCalls, "contexts are read in the same round trips")
	assert.Equal(t, 1, store.contextReadCalls)
}

func TestContextWriteStoresEachRecordOnce(t *testing.T) {
	store := newMemStore()
	b := newBatch(store, newCache(defaultCacheBytes), testRoomID, smallLimits)
	first, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	second, err := b.TxnApply(nil, first, withMemberships(types.StateMap{topicTup: "$topic"}))
	require.NoError(t, err)

	b.TxnWrite(fdb.Transaction{}, first, second, first, second)

	assert.Len(t, store.contexts, 2)
	assert.Equal(t, len(store.pages), store.pageWrites)
	assert.Equal(t, len(store.contexts), store.contextWrites)

	state, err := iterateEventIDs(newBatch(store, newCache(defaultCacheBytes), testRoomID, smallLimits), second)
	require.NoError(t, err)
	expected := initialState()
	expected[topicTup] = "$topic"
	assert.Equal(t, expected, state)
}

func TestContextCacheAvoidsReads(t *testing.T) {
	store := newMemStore()
	cache := newCache(defaultCacheBytes)
	b := newBatch(store, cache, testRoomID, smallLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, ctx)

	state, err := iterateEventIDs(newBatch(store, cache, testRoomID, smallLimits), ctx)
	require.NoError(t, err)
	assert.Equal(t, initialState(), state)
	assert.NotZero(t, store.pageReads, "writes are not cached before commit")

	store.resetCounts()
	store.pages, store.contexts = nil, nil
	state, err = iterateEventIDs(newBatch(store, cache, testRoomID, smallLimits), ctx)
	require.NoError(t, err)
	assert.Equal(t, initialState(), state)
	assert.Zero(t, store.pageReads)
	assert.Zero(t, store.contextReadCalls)
}

func TestContextUncommittedWritesAreNotCached(t *testing.T) {
	store := newMemStore()
	cache := newCache(defaultCacheBytes)
	failed := newBatch(uncommitted{store}, cache, testRoomID, smallLimits)
	ctx, err := failed.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	failed.TxnWrite(fdb.Transaction{}, ctx)

	next := newBatch(store, cache, testRoomID, smallLimits)
	_, err = next.TxnApply(nil, ctx, withMemberships(types.StateMap{topicTup: "$topic"}))
	assert.ErrorIs(t, err, ErrContextNotFound)
}

// A transaction reads back what it wrote before it commits, through any batch, so nothing it reads
// once it wrote is cached, and an abort leaves the cache holding only committed records.
func TestReadsOfAnOpenTransactionsOwnWritesAreNotCached(t *testing.T) {
	store := newMemStore()
	cache := newCache(defaultCacheBytes)
	b := newBatch(store, cache, testRoomID, smallLimits)
	committed, err := b.TxnApply(nil, EmptyContext, withMemberships(initialState()))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, committed)

	txn := newOpenTxn(store)
	_, err = iterateEventIDs(newBatch(txn, cache, testRoomID, smallLimits), committed)
	require.NoError(t, err)
	_, cached := cache.getContext(testRoomID, committed)
	assert.True(t, cached, "what a transaction reads before it writes is committed")

	writer := newBatch(txn, cache, testRoomID, smallLimits)
	written, err := writer.TxnApply(nil, committed, withMemberships(types.StateMap{topicTup: "$topic"}))
	require.NoError(t, err)
	pages := writer.unwritten([]types.StateHash{written}).pages
	require.NotEmpty(t, pages)
	writer.TxnWrite(fdb.Transaction{}, written)
	state, err := iterateEventIDs(newBatch(txn, cache, testRoomID, smallLimits), written)
	require.NoError(t, err)
	assert.Equal(t, id.EventID("$topic"), state[topicTup], "another batch of the transaction reads its writes")
	_, cached = cache.getContext(testRoomID, written)
	assert.False(t, cached)
	for _, h := range pages {
		assert.Nil(t, cache.getPage(testRoomID, h))
	}

	_, err = iterateEventIDs(newBatch(store, cache, testRoomID, smallLimits), written)
	assert.ErrorIs(t, err, ErrContextNotFound, "the transaction never committed")
}

func TestWrittenTransactionsAreForgottenAtTheSecondRotation(t *testing.T) {
	var txn fdb.Transaction
	assert.Equal(t, txnID(txn), txnID(txn.Snapshot()), "every view of a transaction is the same one")

	var w writtenTxns
	w.add(1)
	assert.True(t, w.has(1))
	w.rotated = w.rotated.Add(-writtenTxnsRotation)
	w.add(2)
	assert.True(t, w.has(1), "a rotation keeps the generation before")
	w.rotated = w.rotated.Add(-writtenTxnsRotation)
	w.add(3)
	assert.False(t, w.has(1))
	assert.True(t, w.has(2))
}

func TestContextReadsShareRoundTrips(t *testing.T) {
	store := newMemStore()
	cache := newCache(0)
	b := newBatch(store, cache, testRoomID, smallLimits)
	members := types.StateMap{createTup: "$create", powerTup: "$power"}
	for i := range 200 {
		members[types.MemberStateTup(userID(i))] = id.EventID(fmt.Sprintf("$join%d", i))
	}
	before, err := b.TxnApply(nil, EmptyContext, withMemberships(members))
	require.NoError(t, err)
	after, err := b.TxnApply(nil, before, withMemberships(types.StateMap{topicTup: "$topic"}))
	require.NoError(t, err)
	left, err := b.TxnApply(nil, before, withMemberships(types.StateMap{types.MemberStateTup(userID(3)): "$leave3"}))
	require.NoError(t, err)
	both, err := b.TxnApply(nil, left, withMemberships(types.StateMap{topicTup: "$topic"}))
	require.NoError(t, err)
	b.TxnWrite(fdb.Transaction{}, before, after, left, both)

	depthOf := func(tup types.StateTup) int {
		store.resetCounts()
		_, err := lookupEventIDs(newBatch(store, cache, testRoomID, smallLimits), before, []types.StateTup{tup})
		require.NoError(t, err)
		return store.pageReadCalls
	}
	memberDepth, stateDepth := depthOf(types.MemberStateTup(userID(3))), depthOf(createTup)
	require.Greater(t, memberDepth, stateDepth)

	store.resetCounts()
	_, err = newBatch(store, cache, testRoomID, smallLimits).TxnApply(nil, before, withMemberships(types.StateMap{
		topicTup:                        "$topic",
		types.MemberStateTup(userID(3)): "$leave3",
	}))
	require.NoError(t, err)
	assert.Equal(t, memberDepth, store.pageReadCalls, "both maps are read in the same round trips")

	conflictReadCalls := func(to types.StateHash) int {
		store.resetCounts()
		_, err := newBatch(store, cache, testRoomID, smallLimits).diffContexts(store.room(testRoomID), []types.StateHash{before, to})
		require.NoError(t, err)
		return store.pageReadCalls
	}
	assert.Equal(t, conflictReadCalls(left), conflictReadCalls(both), "both maps are diffed in the same round trips")

	store.resetCounts()
	_, err = newBatch(store, cache, testRoomID, smallLimits).TxnDiff(nil, before, after)
	require.NoError(t, err)
	assert.Equal(t, 1, store.contextReadCalls)

	store.resetCounts()
	_, err = newBatch(store, cache, testRoomID, smallLimits).TxnDiff(nil, before, before)
	require.NoError(t, err)
	assert.Equal(t, 1, store.contextReads, "a repeated context is read once")
}

func TestContextUnknownAndEmpty(t *testing.T) {
	b := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)

	_, err := lookupEventIDs(b, types.StateHash{}, []types.StateTup{createTup})
	assert.ErrorIs(t, err, ErrUnknownContext)
	_, err = b.TxnApply(nil, types.StateHash{}, withMemberships(types.StateMap{createTup: "$create"}))
	assert.ErrorIs(t, err, ErrUnknownContext)

	state, err := iterateEventIDs(b, EmptyContext)
	require.NoError(t, err)
	assert.Empty(t, state)
	found, err := lookupEventIDs(b, EmptyContext, []types.StateTup{createTup, types.MemberStateTup(userID(1))})
	require.NoError(t, err)
	assert.Empty(t, found)

	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(types.StateMap{createTup: "$create"}))
	require.NoError(t, err)
	emptied, err := b.TxnApply(nil, ctx, withMemberships(types.StateMap{createTup: ""}))
	require.NoError(t, err)
	assert.Equal(t, EmptyContext, emptied)
	assert.NotContains(t, b.contexts, EmptyContext)

	_, err = b.TxnDiff(nil, types.StateHash{}, types.StateHash{})
	assert.ErrorIs(t, err, ErrUnknownContext)
	missing := stateContext{stateRoot: hashOf([]byte("missing"))}.ID()
	_, err = b.TxnDiff(nil, missing, missing)
	assert.ErrorIs(t, err, ErrContextNotFound)
}

const (
	deltaTestTuples   = 40
	deltaTestVersions = 3
)

func deltaTestTup(i int) types.StateTup {
	if i < 4 {
		return []types.StateTup{createTup, rulesTup, powerTup, topicTup}[i]
	}
	return types.MemberStateTup(userID(i))
}

// Each event belongs to one tuple, as in a room.
func deltaTestEvent(i, version int) id.EventID {
	return id.EventID(fmt.Sprintf("$%d.%d", i, version))
}

func randomState(rng *rand.Rand) types.StateMap {
	stateMap := make(types.StateMap)
	for i := range deltaTestTuples {
		if rng.IntN(3) > 0 {
			stateMap[deltaTestTup(i)] = deltaTestEvent(i, rng.IntN(deltaTestVersions))
		}
	}
	return stateMap
}

// randomStatePair returns, in turn, a state from empty, a state to empty, two unrelated states, and
// a state with a few tuples changed or removed.
func randomStatePair(rng *rand.Rand, trial int) (from, to types.StateMap) {
	switch trial % 4 {
	case 0:
		return types.StateMap{}, randomState(rng)
	case 1:
		return randomState(rng), types.StateMap{}
	case 2:
		return randomState(rng), randomState(rng)
	}
	from = randomState(rng)
	to = maps.Clone(from)
	for range 1 + rng.IntN(6) {
		i := rng.IntN(deltaTestTuples)
		if rng.IntN(3) == 0 {
			delete(to, deltaTestTup(i))
		} else {
			to[deltaTestTup(i)] = deltaTestEvent(i, rng.IntN(deltaTestVersions))
		}
	}
	return from, to
}

func TestDeltaTurnsFromIntoTo(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 8))
	for trial := range 400 {
		from, to := randomStatePair(rng, trial)
		delta := Delta(withMemberships(from), withMemberships(to))
		for tup, entry := range delta {
			require.NotEqual(t, from[tup], entry.EventID, "trial %d: %v is unchanged", trial, tup)
		}
		applied := withMemberships(from)
		applyToModel(applied, delta)
		require.Equal(t, withMemberships(to), applied, "trial %d", trial)
	}
}

func TestContextApplyDeltaMatchesBuildingFromEmpty(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 21))
	for trial := range 200 {
		from, to := randomStatePair(rng, trial)
		store := newMemStore()
		b := newBatch(store, newCache(0), testRoomID, smallLimits)
		base, err := b.TxnApply(nil, EmptyContext, withMemberships(from))
		require.NoError(t, err)
		b.TxnWrite(fdb.Transaction{}, base)

		b = newBatch(store, newCache(0), testRoomID, smallLimits)
		count, err := b.TxnCount(nil, base)
		require.NoError(t, err)
		require.Equal(t, len(from), count)
		got, err := b.TxnApply(nil, base, Delta(withMemberships(from), withMemberships(to)))
		require.NoError(t, err)
		count, err = b.TxnCount(nil, got)
		require.NoError(t, err)
		require.Equal(t, len(to), count)

		fresh := newBatch(newMemStore(), newCache(0), testRoomID, smallLimits)
		want, err := fresh.TxnApply(nil, EmptyContext, withMemberships(to))
		require.NoError(t, err)
		require.Equal(t, want, got, "trial %d", trial)

		state, err := b.TxnIterateEntries(nil, got)
		require.NoError(t, err)
		require.Equal(t, withMemberships(to), state, "trial %d", trial)
	}
}
