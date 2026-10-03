package events

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var errLoadFailed = errors.New("load failed")

type graphStore struct {
	events                                map[id.EventID]*types.Event
	headers                               map[id.EventID]types.AuthHeader
	links                                 map[uint32][]chainLink
	positions                             map[chainPosition]id.EventID
	tips                                  map[uint32]uint32
	next                                  uint32
	headerReads, linkReads, positionReads int
	headerLoads, positionLoads            int
	// The most links one load returned
	linkLoadRows int
	// The next header load fails once it has read this many, unless zero
	failAfter        int
	prefetched       map[id.EventID]bool
	unprefetchedGets int
}

func newGraphStore() *graphStore {
	return &graphStore{events: make(map[id.EventID]*types.Event), headers: make(map[id.EventID]types.AuthHeader), links: make(map[uint32][]chainLink), positions: make(map[chainPosition]id.EventID), tips: make(map[uint32]uint32), prefetched: make(map[id.EventID]bool)}
}

func (s *graphStore) WillGet(ids ...id.EventID) {
	for _, eventID := range ids {
		s.prefetched[eventID] = true
	}
}

func (s *graphStore) Get(eventID id.EventID) (*types.Event, error) {
	if !s.prefetched[eventID] {
		s.unprefetchedGets++
	}
	return s.events[eventID], nil
}

func (s *graphStore) graph() *AuthGraph {
	g := &AuthGraph{headers: make(map[id.EventID]types.AuthHeader), dirty: make(map[id.EventID]types.AuthHeader), pending: make(map[id.EventID]*types.Event), order: make(map[id.EventID]int), roomID: "!room", events: s}
	g.do = func(reads func() error) error { return reads() }
	g.load = func(ids []id.EventID) ([]types.AuthHeader, error) {
		s.headerLoads++
		out := make([]types.AuthHeader, 0, len(ids))
		for _, eventID := range ids {
			if s.failAfter > 0 && len(out) == s.failAfter {
				s.failAfter = 0
				return out, errLoadFailed
			}
			s.headerReads++
			h, ok := s.headers[eventID]
			if !ok {
				ev := s.events[eventID]
				if ev == nil {
					return out, fmt.Errorf("%w: missing %s", errNoAuthHeader, eventID)
				}
				h.AuthEventIDs = ev.AuthEventIDs
			}
			out = append(out, h)
		}
		return out, nil
	}
	c := &chainCover{links: make(map[uint32][]chainLink), partialLinks: make(map[uint32][]chainLink), tips: make(map[uint32]uint32), positions: make(map[chainPosition]id.EventID), pendingFrom: make(map[uint32]uint32), addedLinks: make(map[uint32][]chainLink)}
	c.loadLinks = func(reads []linkRead) ([][]chainLink, error) {
		out := make([][]chainLink, len(reads))
		s.linkReads += len(reads)
		rows := 0
		for i, read := range reads {
			links := slices.SortedFunc(slices.Values(s.links[read.chain]), compareLinks)
			start := 0
			if read.after.seq != 0 {
				if start = slices.IndexFunc(links, func(l chainLink) bool { return compareLinks(l, read.after) > 0 }); start < 0 {
					start = len(links)
				}
			}
			out[i] = links[start:min(len(links), start+read.limit)]
			rows += len(out[i])
		}
		s.linkLoadRows = max(s.linkLoadRows, rows)
		return out, nil
	}
	c.loadTip = func(chain uint32) (uint32, error) { return s.tips[chain], nil }
	c.allocate = func() (uint32, error) { s.next++; return s.next, nil }
	c.loadPositions = func(intervals []chainInterval) ([]id.EventID, error) {
		var out []id.EventID
		s.positionLoads++
		s.positionReads += len(intervals)
		for _, in := range intervals {
			for seq := in.first; seq <= in.last; seq++ {
				eventID, ok := s.positions[chainPosition{in.chain, seq}]
				if !ok {
					return nil, fmt.Errorf("missing position")
				}
				stored, err := unpackChainEntry(packChainEntry(eventID))
				if err != nil {
					return nil, err
				}
				out = append(out, stored)
			}
		}
		return out, nil
	}
	g.cover = c
	return g
}

func compareLinks(a, b chainLink) int {
	return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(a.target, b.target), cmp.Compare(a.targetSeq, b.targetSeq))
}

func (s *graphStore) commit(g *AuthGraph) {
	maps.Copy(s.headers, g.dirty)
	c := g.cover
	maps.Copy(s.positions, c.positions)
	maps.Copy(s.tips, c.tips)
	for chain, links := range c.addedLinks {
		s.links[chain] = append(s.links[chain], links...)
	}
}

func (s *graphStore) add(n, slot int, parents ...id.EventID) id.EventID {
	eventID := id.EventID(fmt.Sprintf("$%d", n))
	s.events[eventID] = &types.Event{ID: eventID, RoomVersion: "12", PartialEvent: types.PartialEvent{RoomID: "!room", Type: event.StateMember, StateKey: new(fmt.Sprintf("@user%d:test", slot))}, AuthEventIDs: parents}
	return eventID
}

func (s *graphStore) addCreate() id.EventID {
	eventID := id.EventID("$create")
	s.events[eventID] = &types.Event{ID: eventID, RoomVersion: "11", PartialEvent: types.PartialEvent{RoomID: "!room", Type: event.StateCreate, StateKey: new("")}}
	return eventID
}

// finalize stores the events, in order, as a staging transaction does.
func (s *graphStore) finalize(t *testing.T, ids ...id.EventID) {
	t.Helper()
	g := s.graph()
	for _, eventID := range ids {
		g.Add(s.events[eventID])
	}
	_, err := g.Headers(ids)
	require.NoError(t, err)
	s.commit(g)
}

func (s *graphStore) ancestors(root id.EventID) map[id.EventID]bool {
	seen := make(map[id.EventID]bool)
	stack := []id.EventID{root}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, p := range s.events[at].AuthEventIDs {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func (s *graphStore) subgraph(conflicts []id.EventID) []id.EventID {
	out := make(map[id.EventID]bool)
	for _, from := range conflicts {
		out[from] = true
		anc := s.ancestors(from)
		for _, to := range conflicts {
			if !anc[to] {
				continue
			}
			for at := range anc {
				if at == to || s.ancestors(at)[to] {
					out[at] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(out))
}

func (s *graphStore) requireSubgraphs(t *testing.T, rng *rand.Rand, ids []id.EventID) {
	t.Helper()
	for range 100 {
		conflicts := []id.EventID{ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))]}
		got, err := s.graph().ConflictedSubgraph(conflicts)
		require.NoError(t, err)
		require.Equal(t, s.subgraph(conflicts), got)
	}
}

func TestChainCoverOfRandomGraphs(t *testing.T) {
	s := newGraphStore()
	rng := rand.New(rand.NewPCG(129, 33))
	var ids []id.EventID
	for n := range 180 {
		var parents []id.EventID
		if n > 0 {
			for range 1 + rng.IntN(4) {
				p := ids[rng.IntN(n)]
				if !slices.Contains(parents, p) {
					parents = append(parents, p)
				}
			}
		}
		eventID := s.add(n, rng.IntN(12), parents...)
		ids = append(ids, eventID)
		s.finalize(t, eventID)
		require.True(t, s.headers[eventID].Finalized())
	}
	s.requireSubgraphs(t, rng, ids)
	s.requireAuthDifferences(t, rng, ids, s.graph())
}

// Before room version 12 every event but the create event cites it. Any other auth event reaches
// it, so only an event citing the create event alone links to its chain.
func TestChainCoverWhereEveryEventCitesTheCreateEvent(t *testing.T) {
	s := newGraphStore()
	rng := rand.New(rand.NewPCG(7, 91))
	create := s.addCreate()
	s.finalize(t, create)
	ids := []id.EventID{create}
	alone := 0
	for n := 1; n < 180; n++ {
		parents := []id.EventID{create}
		for range min(rng.IntN(4), n-1) {
			if p := ids[1+rng.IntN(n-1)]; !slices.Contains(parents, p) {
				parents = append(parents, p)
			}
		}
		if len(parents) == 1 {
			alone++
		}
		eventID := s.add(n, rng.IntN(12), parents...)
		ids = append(ids, eventID)
		s.finalize(t, eventID)
	}
	createChain := s.headers[create].Chain
	linked := 0
	for chain, links := range s.links {
		for _, l := range links {
			if l.target == createChain {
				linked++
				require.Equal(t, []id.EventID{create}, s.events[s.positions[chainPosition{chain, l.seq}]].AuthEventIDs)
			}
		}
	}
	require.Equal(t, alone, linked)
	s.requireSubgraphs(t, rng, ids)
	s.requireAuthDifferences(t, rng, ids, s.graph())
}

func TestChainTipForkAndLongHistory(t *testing.T) {
	s := newGraphStore()
	root := s.add(0, 0)
	s.finalize(t, root)
	prev := root
	for n := 1; n <= 5000; n++ {
		next := s.add(n, 0, prev)
		s.finalize(t, next)
		prev = next
	}
	fork := s.add(5001, 0, root)
	s.finalize(t, fork)
	require.NotEqual(t, s.headers[fork].Chain, s.headers[prev].Chain)
	require.Equal(t, uint32(5001), s.headers[prev].Sequence)
	s.headerReads, s.linkReads, s.positionReads = 0, 0, 0
	conflicts := []id.EventID{prev, fork}
	got, err := s.graph().ConflictedSubgraph(conflicts)
	require.NoError(t, err)
	require.ElementsMatch(t, conflicts, got)
	require.Equal(t, 2, s.headerReads)
	require.LessOrEqual(t, s.linkReads, 2)
	require.LessOrEqual(t, s.positionReads, 2)
}

func TestConflictedSubgraphReadsPositionsInBatches(t *testing.T) {
	s := newGraphStore()
	var chain []id.EventID
	for seq := uint32(1); seq <= 2*positionBatch+1; seq++ {
		eventID := s.add(int(seq), 0)
		s.headers[eventID] = types.AuthHeader{Chain: 1, Sequence: seq}
		s.positions[chainPosition{1, seq}] = eventID
		chain = append(chain, eventID)
	}
	batches := 0
	g := s.graph()
	g.do = func(reads func() error) error {
		batches++
		return reads()
	}
	got, err := g.ConflictedSubgraph([]id.EventID{chain[0], chain[len(chain)-1]})
	require.NoError(t, err)
	require.Equal(t, slices.Sorted(slices.Values(chain)), got)
	require.Equal(t, 3, s.positionLoads)
	require.Equal(t, 5, batches, "one of headers, one of links and three of positions")
}

func TestChainLinksReadInBatchesOfRows(t *testing.T) {
	s := newGraphStore()
	n := uint32(2*linkRowBatch + 5)
	var first, second []id.EventID
	for seq := uint32(1); seq <= n; seq++ {
		e, f := s.add(int(seq), 0), s.add(int(n+seq), 1)
		s.headers[e] = types.AuthHeader{Chain: 1, Sequence: seq}
		s.headers[f] = types.AuthHeader{Chain: 2, Sequence: seq}
		s.positions[chainPosition{1, seq}], s.positions[chainPosition{2, seq}] = e, f
		s.links[2] = append(s.links[2], chainLink{seq, 1, seq})
		first, second = append(first, e), append(second, f)
	}
	g := s.graph()
	got, err := g.ConflictedSubgraph([]id.EventID{first[0], second[n-1]})
	require.NoError(t, err)
	require.Equal(t, slices.Sorted(slices.Values(slices.Concat(first, second))), got)
	require.Equal(t, s.links[2], g.cover.links[2])
	require.Empty(t, g.cover.partialLinks)
	require.Equal(t, linkRowBatch, s.linkLoadRows)
}

func TestPositionBatches(t *testing.T) {
	require.Empty(t, positionBatches(nil, 4))
	require.Equal(t, [][]chainInterval{
		{{1, 1, 3}, {2, 5, 5}},
		{{2, 6, 9}},
		{{2, 10, 10}, {3, math.MaxUint32 - 4, math.MaxUint32 - 2}},
		{{3, math.MaxUint32 - 1, math.MaxUint32}},
	}, positionBatches([]chainInterval{{1, 1, 3}, {2, 5, 10}, {3, math.MaxUint32 - 4, math.MaxUint32}}, 4))
}

// A graph finalizes pending events in batch order, whatever order they are asked for in, reading the
// headers of their auth events in one batch, and fails for an event citing one not finalized.
func TestHeadersFinalizeInBatchOrder(t *testing.T) {
	s := newGraphStore()
	rng := rand.New(rand.NewPCG(3, 4))
	create := s.addCreate()
	s.finalize(t, create)
	// Stored without a finalized header, as an outlier is
	outlier := s.add(1, 0, create)
	s.headers[outlier] = types.AuthHeader{AuthEventIDs: []id.EventID{create}}
	var first, second []*types.Event
	for n := range 20 {
		first = append(first, s.events[s.add(100+n, n, create)])
	}
	for n := range 50 {
		parents := []id.EventID{create}
		for range 3 {
			if p := first[rng.IntN(len(first))].ID; !slices.Contains(parents, p) {
				parents = append(parents, p)
			}
		}
		second = append(second, s.events[s.add(200+n, 20+n, parents...)])
	}

	g := s.graph()
	g.Add(slices.Concat(first, second)...)
	_, err := g.Headers(eventIDs(second))
	require.ErrorIs(t, err, ErrAuthEventPending, "the first events are pending but not finalized")
	require.Empty(t, g.dirty)

	g = s.graph()
	g.Add(slices.Concat(first, second)...)
	ids := eventIDs(slices.Concat(first, second))
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	s.headerLoads, s.unprefetchedGets = 0, 0
	_, err = g.Headers(ids)
	require.NoError(t, err)
	require.Equal(t, 2, s.headerLoads, "the events, then the auth events they cite")
	require.Zero(t, s.unprefetchedGets)
	require.Len(t, g.dirty, len(first)+len(second))
	s.commit(g)
	for _, ev := range slices.Concat(first, second) {
		require.True(t, s.headers[ev.ID].Finalized())
	}
	s.requireSubgraphs(t, rng, slices.Concat([]id.EventID{create}, ids))

	_, err = s.graph().Headers([]id.EventID{outlier})
	require.ErrorIs(t, err, ErrAuthEventNotFinalized, "an event asked for is finalized only when pending")
}

// A stored event whose header is not finalized, as an outlier's, is finalized ahead of the batch
// event citing it, with the outliers it cites in turn. One that is rejected, on a cycle or citing an
// event not stored makes the event citing it no accepted state. One citing a pending event not
// finalized yet leaves the event citing it to when that one is. A batch event citing an event not
// stored fails the batch.
func TestHeadersFinalizeCitedOutliersFirst(t *testing.T) {
	s := newGraphStore()
	create := s.addCreate()
	s.finalize(t, create)
	outlier := func(n int, parents ...id.EventID) id.EventID {
		eventID := s.add(n, n, parents...)
		s.headers[eventID] = types.AuthHeader{AuthEventIDs: parents}
		return eventID
	}
	invite := outlier(1, create)
	rescind := outlier(2, create, invite)
	citing := s.events[s.add(3, 0, create, rescind)]

	g := s.graph()
	g.Add(citing)
	hs, err := g.Headers([]id.EventID{citing.ID})
	require.NoError(t, err)
	require.Len(t, g.dirty, 3, "the event and both outliers")
	require.True(t, g.dirty[invite].Finalized() && g.dirty[rescind].Finalized() && hs[0].Finalized())
	s.commit(g)

	g = s.graph()
	again := s.events[s.add(4, 0, create, invite)]
	g.Add(again)
	_, err = g.Headers([]id.EventID{again.ID})
	require.NoError(t, err)
	require.Len(t, g.dirty, 1, "an outlier finalized once is stored finalized")

	rejected := outlier(5, create)
	s.events[rejected].Rejected = true
	cyclic, other := outlier(6, create), outlier(7, create)
	s.headers[cyclic] = types.AuthHeader{AuthEventIDs: []id.EventID{create, other}}
	s.headers[other] = types.AuthHeader{AuthEventIDs: []id.EventID{create, cyclic}}
	unknownAuth := outlier(8, create, "$gone")
	for n, cited := range []id.EventID{rejected, cyclic, unknownAuth} {
		ev := s.events[s.add(20+n, 0, create, cited)]
		g := s.graph()
		g.Add(ev)
		_, err := g.Headers([]id.EventID{ev.ID})
		require.ErrorIs(t, err, ErrAuthEventNotFinalized, "citing %s", cited)
	}

	later := s.events[s.add(30, 30, create)]
	citesLater := outlier(31, create, later.ID)
	ev := s.events[s.add(32, 0, create, citesLater)]
	g = s.graph()
	g.Add(ev, later)
	_, err = g.Headers([]id.EventID{ev.ID, later.ID})
	require.ErrorIs(t, err, ErrAuthEventPending, "an outlier citing a pending event later in the batch")
	g = s.graph()
	g.Add(ev, later)
	_, err = g.Headers([]id.EventID{ev.ID, later.ID})
	require.ErrorIs(t, err, ErrAuthEventPending, "an event citing a pending event later in the batch")
	g = s.graph()
	g.Add(later, ev)
	_, err = g.Headers([]id.EventID{ev.ID, later.ID})
	require.NoError(t, err, "with the pending event first")
	later.Rejected = true
	g = s.graph()
	g.Add(later, ev)
	_, err = g.Headers([]id.EventID{ev.ID})
	require.ErrorIs(t, err, ErrAuthEventNotFinalized, "an outlier citing a rejected pending event")

	missing := s.events[s.add(11, 0, create, "$missing")]
	g = s.graph()
	g.Add(missing)
	_, err = g.Headers([]id.EventID{missing.ID})
	require.ErrorContains(t, err, "missing $missing")
}

func eventIDs(evs []*types.Event) []id.EventID {
	ids := make([]id.EventID, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID
	}
	return ids
}

func TestReadKeepsHeadersLoadedBeforeAFailure(t *testing.T) {
	s := newGraphStore()
	var ids []id.EventID
	for n := range 5 {
		ids = append(ids, s.add(n, n))
	}
	s.finalize(t, ids...)
	g := s.graph()
	s.headerReads, s.failAfter = 0, 2
	g.do = func(reads func() error) error {
		err := reads()
		require.ErrorIs(t, err, errLoadFailed)
		return reads()
	}
	hs, err := g.HeadersInBatches(ids)
	require.NoError(t, err)
	require.Equal(t, 5, s.headerReads, "the second read loads only the three headers left")
	for i, h := range hs {
		require.Equal(t, s.headers[ids[i]], h)
	}
}

func TestGraphSpanningTransactionsFinalizesNothing(t *testing.T) {
	s := newGraphStore()
	stored := s.add(0, 0)
	s.finalize(t, stored)
	pending := s.events[s.add(1, 0, stored)]
	g := s.graph()
	g.events = nil
	g.Add(pending)
	_, err := g.Headers([]id.EventID{stored})
	require.NoError(t, err)
	_, err = g.Headers([]id.EventID{pending.ID})
	require.ErrorContains(t, err, "not finalized")
	require.Empty(t, g.dirty)
}

// addRandom adds an event of one of four users citing the create event, usually the user's latest
// event too, and up to extra events of other users.
func (s *graphStore) addRandom(rng *rand.Rand, create id.EventID, ids []id.EventID, extra int) *types.Event {
	slot := rng.IntN(4)
	stateKey := fmt.Sprintf("@user%d:test", slot)
	parents := []id.EventID{create}
	for i := len(ids) - 1; i > 0; i-- {
		if *s.events[ids[i]].StateKey == stateKey {
			if rng.IntN(3) > 0 {
				parents = append(parents, ids[i])
			}
			break
		}
	}
	for range extra {
		if len(ids) > 1 {
			if p := ids[1+rng.IntN(len(ids)-1)]; !slices.Contains(parents, p) && *s.events[p].StateKey != stateKey {
				parents = append(parents, p)
			}
		}
	}
	return s.events[s.add(len(s.events), slot, parents...)]
}

func TestLabellingPendingEventsTogether(t *testing.T) {
	for seed := range uint64(40) {
		s := newGraphStore()
		rng := rand.New(rand.NewPCG(seed, 77))
		create := s.addCreate()
		s.finalize(t, create)
		ids := []id.EventID{create}
		for range 8 {
			var pending []*types.Event
			for range 1 + rng.IntN(25) {
				ev := s.addRandom(rng, create, ids, rng.IntN(3))
				ids = append(ids, ev.ID)
				pending = append(pending, ev)
			}
			g := s.graph()
			g.Add(pending...)
			pendingIDs := eventIDs(pending)
			rng.Shuffle(len(pendingIDs), func(i, j int) { pendingIDs[i], pendingIDs[j] = pendingIDs[j], pendingIDs[i] })
			_, err := g.Headers(pendingIDs)
			require.NoError(t, err)
			for range 30 {
				conflicts := []id.EventID{ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))]}
				got, err := g.ConflictedSubgraph(conflicts)
				require.NoError(t, err)
				require.Equal(t, s.subgraph(conflicts), got, "seed %d", seed)
			}
			s.requireAuthDifferences(t, rng, ids, g)
			s.commit(g)
		}
		s.requireSubgraphs(t, rng, ids)
		positions := make(map[chainPosition]bool)
		for _, eventID := range ids {
			h := s.headers[eventID]
			require.False(t, positions[chainPosition{h.Chain, h.Sequence}])
			positions[chainPosition{h.Chain, h.Sequence}] = true
		}
	}
}

// A preparation labels its pending events, then staging stores others, extending the same chains and
// allocating the same chain IDs, before a job reads the conflicted subgraph from the preparation's
// graph.
func TestJobAfterConcurrentStaging(t *testing.T) {
	collisions := 0
	for seed := range uint64(40) {
		s := newGraphStore()
		rng := rand.New(rand.NewPCG(seed, 99))
		create := s.addCreate()
		s.finalize(t, create)
		stored := []id.EventID{create}
		addBatch := func(ids []id.EventID) []*types.Event {
			var evs []*types.Event
			for range 10 {
				ev := s.addRandom(rng, create, ids, rng.IntN(2))
				ids = append(ids, ev.ID)
				evs = append(evs, ev)
			}
			return evs
		}
		for range 3 {
			evs := addBatch(stored)
			stored = append(stored, eventIDs(evs)...)
			s.finalize(t, eventIDs(evs)...)
		}

		prepared := addBatch(stored)
		allocated := s.next
		prep := s.graph()
		prep.Add(prepared...)
		_, err := prep.Headers(eventIDs(prepared))
		require.NoError(t, err)

		s.next = allocated
		staged := addBatch(stored)
		staging := s.graph()
		staging.Add(staged...)
		_, err = staging.Headers(eventIDs(staged))
		require.NoError(t, err)
		for pos := range staging.cover.positions {
			if _, found := prep.cover.positions[pos]; found {
				collisions++
			}
		}
		s.commit(staging)

		prep.events = nil
		ids := slices.Concat(stored, eventIDs(prepared))
		for range 30 {
			conflicts := []id.EventID{ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))], ids[rng.IntN(len(ids))]}
			got, err := prep.ConflictedSubgraph(conflicts)
			require.NoError(t, err)
			require.Equal(t, s.subgraph(conflicts), got, "seed %d", seed)
		}
	}
	require.Positive(t, collisions, "staging took positions the preparation holds")
}

// authDifference is the model auth difference of state sets: the events in the auth chain of some
// sets but not all, a set's state events counted in its own
func (s *graphStore) authDifference(sets [][]id.EventID) []id.EventID {
	counts := make(map[id.EventID]int)
	for _, set := range sets {
		chain := make(map[id.EventID]bool)
		for _, eventID := range set {
			chain[eventID] = true
			maps.Copy(chain, s.ancestors(eventID))
		}
		for eventID := range chain {
			counts[eventID]++
		}
	}
	var difference []id.EventID
	for eventID, n := range counts {
		if n < len(sets) {
			difference = append(difference, eventID)
		}
	}
	slices.Sort(difference)
	return difference
}

func (s *graphStore) requireAuthDifferences(t *testing.T, rng *rand.Rand, ids []id.EventID, g *AuthGraph) {
	t.Helper()
	for range 30 {
		sets := make([][]id.EventID, 2+rng.IntN(2))
		for i := range sets {
			for range rng.IntN(6) {
				sets[i] = append(sets[i], ids[rng.IntN(len(ids))])
			}
		}
		got, err := g.AuthChainDifference(sets)
		require.NoError(t, err)
		require.Equal(t, s.authDifference(sets), got, "sets %v", sets)
	}
}

// diffTestGraph is a small room: the create event, two power levels events, two members' joins and
// profile updates, and a third member's join.
type diffTestGraph struct {
	s                                  *graphStore
	create, p1, p2, a1, a2, b1, b2, c1 id.EventID
}

func newDiffTestGraph(t *testing.T) *diffTestGraph {
	s := newGraphStore()
	d := &diffTestGraph{s: s, create: s.addCreate()}
	d.p1 = s.add(1, 1, d.create)
	d.a1 = s.add(2, 2, d.create, d.p1)
	d.a2 = s.add(3, 2, d.create, d.p1, d.a1)
	d.b1 = s.add(4, 3, d.create, d.p1)
	d.b2 = s.add(5, 3, d.create, d.p1, d.b1)
	d.p2 = s.add(6, 1, d.create, d.p1, d.a2)
	d.c1 = s.add(7, 4, d.create, d.p1)
	s.finalize(t, d.create, d.p1, d.a1, d.a2, d.b1, d.b2, d.p2, d.c1)
	return d
}

func (d *diffTestGraph) difference(t *testing.T, sets ...[]id.EventID) []id.EventID {
	t.Helper()
	got, err := d.s.graph().AuthChainDifference(sets)
	require.NoError(t, err)
	require.Equal(t, d.s.authDifference(sets), got)
	return got
}

func TestAuthChainDifferenceOfIdenticalSetsIsEmpty(t *testing.T) {
	d := newDiffTestGraph(t)
	state := []id.EventID{d.create, d.p2, d.a2, d.b2}
	require.Empty(t, d.difference(t, state, slices.Clone(state)))
	require.Empty(t, d.difference(t, state, []id.EventID{d.b2, d.a2, d.p2, d.create}, state), "whatever the order")
}

func TestAuthChainDifferenceOfAChangedMemberIsItsChainAboveTheCommonReach(t *testing.T) {
	d := newDiffTestGraph(t)
	require.Equal(t, d.s.headers[d.b1].Chain, d.s.headers[d.b2].Chain, "Bob's join and profile update are one chain")
	require.Equal(t, []id.EventID{d.b2}, d.difference(t,
		[]id.EventID{d.create, d.p1, d.a1, d.b1},
		[]id.EventID{d.create, d.p1, d.a1, d.b2},
	), "Bob's join is in both, below his profile update")
}

func TestAuthChainDifferenceFollowsLinksToOtherChains(t *testing.T) {
	d := newDiffTestGraph(t)
	require.NotEqual(t, d.s.headers[d.p2].Chain, d.s.headers[d.a2].Chain)
	require.ElementsMatch(t, []id.EventID{d.p2, d.a2}, d.difference(t,
		[]id.EventID{d.create, d.p1, d.a1},
		[]id.EventID{d.create, d.p2, d.a1},
	), "the second power levels cite Alice's profile update, which neither state holds")
}

func TestAuthChainDifferenceHoldsAChainOnlyOneSetReaches(t *testing.T) {
	d := newDiffTestGraph(t)
	require.ElementsMatch(t, []id.EventID{d.b1, d.b2}, d.difference(t,
		[]id.EventID{d.create, d.p1},
		[]id.EventID{d.create, d.p1, d.b2},
	), "the whole prefix of Bob's chain")
	require.ElementsMatch(t, []id.EventID{d.b1, d.b2, d.c1}, d.difference(t,
		[]id.EventID{d.create, d.p1, d.c1},
		[]id.EventID{d.create, d.p1, d.b2},
		[]id.EventID{d.create, d.p1, d.b2, d.c1},
	))
	require.ElementsMatch(t, []id.EventID{d.create, d.p1, d.a1}, d.difference(t, nil, []id.EventID{d.a1}), "an empty state")
}

func TestAuthChainDifferenceReadsInBatches(t *testing.T) {
	d := newDiffTestGraph(t)
	d.s.headerLoads, d.s.positionLoads = 0, 0
	batches := 0
	g := d.s.graph()
	g.do = func(reads func() error) error {
		batches++
		return reads()
	}
	got, err := g.AuthChainDifference([][]id.EventID{{d.create, d.p1, d.a1}, {d.create, d.p2, d.a1}})
	require.NoError(t, err)
	require.ElementsMatch(t, []id.EventID{d.p2, d.a2}, got)
	require.Equal(t, 1, d.s.headerLoads)
	require.Equal(t, 1, d.s.positionLoads)
	require.Equal(t, 3, batches, "one of headers, one of links and one of positions")

	unfinalized := d.s.add(8, 5, d.create)
	_, err = d.s.graph().AuthChainDifference([][]id.EventID{{d.create}, {unfinalized}})
	require.ErrorIs(t, err, ErrAuthEventNotFinalized)
}

// A preparation's graph holds the positions it assigned above the stored tips, where staging may store
// other events before a job reads them. The auth difference of states holding the preparation's
// events is exact, and holds no event staging stored since.
func TestAuthChainDifferenceOfPendingEventsAfterConcurrentStaging(t *testing.T) {
	collisions := 0
	for seed := range uint64(30) {
		s := newGraphStore()
		rng := rand.New(rand.NewPCG(seed, 31))
		create := s.addCreate()
		s.finalize(t, create)
		stored := []id.EventID{create}
		addBatch := func(ids []id.EventID) []*types.Event {
			var evs []*types.Event
			for range 15 {
				ev := s.addRandom(rng, create, ids, rng.IntN(3))
				ids = append(ids, ev.ID)
				evs = append(evs, ev)
			}
			return evs
		}
		for range 4 {
			evs := addBatch(stored)
			stored = append(stored, eventIDs(evs)...)
			s.finalize(t, eventIDs(evs)...)
		}

		prepared := addBatch(stored)
		allocated := s.next
		prep := s.graph()
		prep.Add(prepared...)
		_, err := prep.Headers(eventIDs(prepared))
		require.NoError(t, err)
		s.next = allocated
		staged := addBatch(stored)
		staging := s.graph()
		staging.Add(staged...)
		_, err = staging.Headers(eventIDs(staged))
		require.NoError(t, err)
		for pos := range staging.cover.positions {
			if _, found := prep.cover.positions[pos]; found {
				collisions++
			}
		}
		s.commit(staging)

		prep.events = nil
		s.requireAuthDifferences(t, rng, slices.Concat(stored, eventIDs(prepared)), prep)
		for chain, from := range prep.cover.pendingFrom {
			loads := s.positionLoads
			_, err := prep.chainEvents([]chainInterval{{chain, from, prep.cover.tips[chain]}})
			require.NoError(t, err)
			require.Equal(t, loads, s.positionLoads, "positions the graph holds are not read")
		}
	}
	require.Positive(t, collisions, "staging took positions the preparation holds")
}

func TestAuthHeaderCodecAndIncompleteGraph(t *testing.T) {
	finalized := types.AuthHeader{AuthEventIDs: []id.EventID{"$a", "$b"}, Chain: 7, Sequence: 22}
	for _, h := range []types.AuthHeader{{}, {AuthEventIDs: []id.EventID{"$a"}}, finalized} {
		got, err := unpackAuthHeader(packAuthHeader(h))
		require.NoError(t, err)
		require.Equal(t, h, got)
	}
	for _, raw := range [][]byte{
		tuple.Tuple{"$a", "$b"}.Pack(),
		tuple.Tuple{int64(8)}.Pack(),
		tuple.Tuple{}.Pack(),
		tuple.Tuple{int64(2), int64(0), int64(0), tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(0), int64(0), tuple.Tuple{}, tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(1), int64(0), tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(0), int64(1), tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(-1), int64(1), tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(math.MaxUint32 + 1), int64(1), tuple.Tuple{}}.Pack(),
		tuple.Tuple{int64(1), int64(0), int64(0), tuple.Tuple{""}}.Pack(),
		tuple.Tuple{int64(1), int64(0), int64(0), tuple.Tuple{int64(1)}}.Pack(),
	} {
		_, err := unpackAuthHeader(raw)
		require.Error(t, err)
	}

	got, err := unpackChainEntry(packChainEntry("$c"))
	require.NoError(t, err)
	require.Equal(t, id.EventID("$c"), got)
	for _, raw := range [][]byte{
		[]byte("$c"),
		tuple.Tuple{}.Pack(),
		tuple.Tuple{""}.Pack(),
		tuple.Tuple{int64(19)}.Pack(),
		tuple.Tuple{"$c", int64(19)}.Pack(),
	} {
		_, err := unpackChainEntry(raw)
		require.Error(t, err)
	}
	// A header rejected at step 4 for too many auth events stays readable, but cannot be finalized.
	tooMany := make([]id.EventID, maxAuthEdges+1)
	for i := range tooMany {
		tooMany[i] = id.EventID(fmt.Sprintf("$auth%d", i))
	}
	rejected, err := unpackAuthHeader(packAuthHeader(types.AuthHeader{AuthEventIDs: tooMany}))
	require.NoError(t, err)
	require.Equal(t, tooMany, rejected.AuthEventIDs)
	_, err = unpackAuthHeader(packAuthHeader(types.AuthHeader{AuthEventIDs: tooMany, Chain: 1, Sequence: 1}))
	require.Error(t, err)

	s := newGraphStore()
	pending := func(ids ...id.EventID) *AuthGraph {
		g := s.graph()
		for _, eventID := range ids {
			g.Add(s.events[eventID])
		}
		return g
	}
	a := s.add(0, 0, "$missing")
	_, err = pending(a).Headers([]id.EventID{a})
	require.ErrorContains(t, err, "missing $missing")
	s.events[a].AuthEventIDs = []id.EventID{a}
	_, err = pending(a).Headers([]id.EventID{a})
	require.ErrorIs(t, err, ErrAuthEventNotFinalized, "an event citing itself")
	b, c := s.add(1, 1, a), s.add(2, 2)
	s.events[a].AuthEventIDs = []id.EventID{b}
	s.events[c].AuthEventIDs = []id.EventID{a}
	_, err = pending(a, b, c).Headers([]id.EventID{a, b, c})
	require.ErrorIs(t, err, ErrAuthEventPending, "a cycle cites an event later in the batch")

	s.events[a].AuthEventIDs = nil
	g := pending(a)
	hs, err := g.Headers([]id.EventID{a})
	require.NoError(t, err)
	require.True(t, hs[0].Finalized())
	s.commit(g)
	g = s.graph()
	again, err := g.Headers([]id.EventID{a})
	require.NoError(t, err)
	require.Equal(t, hs, again)
	require.Empty(t, g.dirty)
}
