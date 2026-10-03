package events

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type chainPosition struct{ chain, seq uint32 }
type chainLink struct{ seq, target, targetSeq uint32 }
type chainInterval struct{ chain, first, last uint32 }

// linkRead reads at most limit of a chain's links, from its first or, when after is set, from the
// one following after.
type linkRead struct {
	chain uint32
	after chainLink
	limit int
}

// chainCover holds the chain links a graph read and the positions it assigned, reading position rows
// only for tips, subgraph intervals and auth differences.
//
// Staging outside the room's mutex may extend a chain or allocate a chain ID after a graph read it,
// so a graph never reads a chain's rows above what it holds: of a chain it extended it holds the
// links and every position above the stored tip, and a chain it allocated it never reads. A graph
// labels only within one transaction; one spanning transactions, an artifact job's, labels nothing.
type chainCover struct {
	// Returns the links of the reads completed before a failure
	loadLinks func([]linkRead) ([][]chainLink, error)
	loadTip   func(uint32) (uint32, error)
	allocate  func() (uint32, error)
	// Returns the events at every position of the intervals, interval by interval
	loadPositions func([]chainInterval) ([]id.EventID, error)
	links         map[uint32][]chainLink
	// Links of chains read in part, to continue after the last
	partialLinks map[uint32][]chainLink
	tips         map[uint32]uint32
	positions    map[chainPosition]id.EventID
	// The lowest position assigned on each chain, from which every position of the chain is held
	pendingFrom map[uint32]uint32
	addedLinks  map[uint32][]chainLink
	next        uint32
	allocated   bool
	// The allocator as read before the first allocation
	allocatedFrom uint32
}

func (g *AuthGraph) newChainCover() *chainCover {
	c := &chainCover{links: make(map[uint32][]chainLink), partialLinks: make(map[uint32][]chainLink), tips: make(map[uint32]uint32), positions: make(map[chainPosition]id.EventID), pendingFrom: make(map[uint32]uint32), addedLinks: make(map[uint32][]chainLink)}
	c.loadLinks = func(reads []linkRead) ([][]chainLink, error) {
		futures := make([]fdb.RangeResult, len(reads))
		for i, read := range reads {
			begin, end := g.dir.authChainLinks.Sub(g.roomID.String(), int64(read.chain)).FDBRangeKeys()
			if read.after.seq != 0 {
				after := g.dir.authChainLinks.Pack(tuple.Tuple{g.roomID.String(), int64(read.chain), int64(read.after.seq), int64(read.after.target), int64(read.after.targetSeq)})
				begin = append(after, 0x00)
			}
			futures[i] = g.txn.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: read.limit})
		}
		out := make([][]chainLink, 0, len(reads))
		for _, future := range futures {
			rows, err := future.GetSliceWithError()
			if err != nil {
				return out, err
			}
			links := make([]chainLink, 0, len(rows))
			for _, row := range rows {
				t, err := g.dir.authChainLinks.Unpack(row.Key)
				if err != nil || len(t) != 5 {
					return out, fmt.Errorf("invalid auth chain link")
				}
				vals := [3]uint32{}
				for j := range vals {
					n, ok := t[j+2].(int64)
					if !ok || n <= 0 || n > math.MaxUint32 {
						return out, fmt.Errorf("invalid auth chain coordinate")
					}
					vals[j] = uint32(n)
				}
				links = append(links, chainLink{vals[0], vals[1], vals[2]})
			}
			out = append(out, links)
		}
		return out, nil
	}
	c.loadTip = func(chain uint32) (uint32, error) {
		return g.readTip(g.txn, chain)
	}
	c.allocate = func() (uint32, error) {
		if !c.allocated {
			next, err := g.readNextChain(g.txn)
			if err != nil {
				return 0, err
			}
			c.next, c.allocatedFrom, c.allocated = next, next, true
		}
		if c.next == math.MaxUint32 {
			return 0, fmt.Errorf("auth chain ID overflow")
		}
		c.next++
		return c.next, nil
	}
	c.loadPositions = func(intervals []chainInterval) ([]id.EventID, error) {
		futures := make([]fdb.RangeResult, len(intervals))
		for i, in := range intervals {
			futures[i] = g.txn.GetRange(fdb.KeyRange{
				Begin: g.dir.authChainPositions.Pack(tuple.Tuple{g.roomID.String(), int64(in.chain), int64(in.first)}),
				End:   g.dir.authChainPositions.Pack(tuple.Tuple{g.roomID.String(), int64(in.chain), int64(in.last) + 1}),
			}, fdb.RangeOptions{})
		}
		var out []id.EventID
		for i, future := range futures {
			rows, err := future.GetSliceWithError()
			if err != nil {
				return nil, err
			}
			in := intervals[i]
			if uint64(len(rows)) != uint64(in.last)-uint64(in.first)+1 {
				return nil, fmt.Errorf("missing auth chain positions in chain %d from %d to %d", in.chain, in.first, in.last)
			}
			for j, row := range rows {
				seq := in.first + uint32(j)
				t, err := g.dir.authChainPositions.Unpack(row.Key)
				if err != nil || len(t) != 3 || t[1] != int64(in.chain) || t[2] != int64(seq) {
					return nil, fmt.Errorf("invalid auth chain position")
				}
				eventID, err := unpackChainEntry(row.Value)
				if err != nil {
					return nil, fmt.Errorf("auth chain position %d of chain %d: %w", seq, in.chain, err)
				}
				out = append(out, eventID)
			}
		}
		return out, nil
	}
	return c
}

func (g *AuthGraph) readTip(txn fdb.ReadTransaction, chain uint32) (uint32, error) {
	rows, err := txn.GetRange(g.dir.authChainPositions.Sub(g.roomID.String(), int64(chain)), fdb.RangeOptions{Limit: 1, Reverse: true}).GetSliceWithError()
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	t, err := g.dir.authChainPositions.Unpack(rows[0].Key)
	if err != nil || len(t) != 3 {
		return 0, fmt.Errorf("invalid auth chain position")
	}
	n, ok := t[2].(int64)
	if !ok || n <= 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("invalid auth chain sequence")
	}
	return uint32(n), nil
}

func (g *AuthGraph) readNextChain(txn fdb.ReadTransaction) (uint32, error) {
	raw, err := txn.Get(g.dir.authChainNext.Pack(tuple.Tuple{g.roomID.String()})).Get()
	if err != nil || raw == nil {
		return 0, err
	}
	t, err := tuple.Unpack(raw)
	if err != nil || len(t) != 1 {
		return 0, fmt.Errorf("invalid chain allocator")
	}
	n, ok := t[0].(int64)
	if !ok || n < 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("invalid chain allocator")
	}
	return uint32(n), nil
}

// TxnLabelsUnchanged reports whether the headers the graph finalized are still unfinalized, and the
// tips of the chains it extended and the chain allocator it allocated from are as it read them. On a
// transaction that writes, the reads conflict with a writer committing after them.
func (g *AuthGraph) TxnLabelsUnchanged(txn fdb.ReadTransaction) (bool, error) {
	headers := make([]fdb.FutureByteSlice, 0, len(g.dirty))
	for eventID := range g.dirty {
		headers = append(headers, txn.Get(g.dir.keyForAuthEventIDs(eventID)))
	}
	c := g.cover
	if c.allocated {
		if next, err := g.readNextChain(txn); err != nil || next != c.allocatedFrom {
			return false, err
		}
	}
	for chain, from := range c.pendingFrom {
		if c.allocated && chain > c.allocatedFrom {
			continue
		} else if tip, err := g.readTip(txn, chain); err != nil || tip != from-1 {
			return false, err
		}
	}
	for _, header := range headers {
		if raw, err := header.Get(); err != nil {
			return false, err
		} else if raw == nil {
			continue
		} else if h, err := unpackAuthHeader(raw); err != nil || h.Finalized() {
			return false, err
		}
	}
	return true, nil
}

// fetch reads the links of the chains and of every chain they link to, in batches of at most
// linkChainBatch chains and linkRowBatch links, shared evenly between the chains.
// A chain with more links continues in a later batch. Links are kept in source sequence order.
func (g *AuthGraph) fetch(chains []uint32) error {
	c := g.cover
	var queue []uint32
	queued := make(map[uint32]bool)
	enqueue := func(chain uint32) {
		if _, done := c.links[chain]; !done && !queued[chain] {
			queued[chain] = true
			queue = append(queue, chain)
		}
	}
	for _, chain := range chains {
		enqueue(chain)
	}
	for len(queue) > 0 {
		part := queue[:min(len(queue), linkChainBatch)]
		queue = queue[len(part):]
		limit := max(linkRowBatch/len(part), 1)
		if err := g.do(func() error {
			var reads []linkRead
			for _, chain := range part {
				if _, done := c.links[chain]; !done {
					read := linkRead{chain: chain, limit: limit}
					if partial := c.partialLinks[chain]; len(partial) > 0 {
						read.after = partial[len(partial)-1]
					}
					reads = append(reads, read)
				}
			}
			results, err := c.loadLinks(reads)
			for i, links := range results {
				chain := reads[i].chain
				c.partialLinks[chain] = append(c.partialLinks[chain], links...)
				if len(links) < reads[i].limit {
					c.links[chain] = c.partialLinks[chain]
					delete(c.partialLinks, chain)
				}
				for _, l := range links {
					enqueue(l.target)
				}
			}
			return err
		}); err != nil {
			return err
		}
		for _, chain := range part {
			if _, done := c.links[chain]; !done {
				queue = append(queue, chain)
			}
		}
	}
	return nil
}

// readPositions reads the events at the intervals' positions, at most positionBatch per batch.
func (g *AuthGraph) readPositions(intervals []chainInterval) ([]id.EventID, error) {
	var out []id.EventID
	for _, part := range positionBatches(intervals, positionBatch) {
		if err := g.do(func() error {
			eventIDs, err := g.cover.loadPositions(part)
			if err == nil {
				out = append(out, eventIDs...)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stored returns the part of an interval below the positions the graph assigned on its chain, the
// only part it reads from storage.
func (c *chainCover) stored(in chainInterval) (chainInterval, bool) {
	if from, ok := c.pendingFrom[in.chain]; ok && in.last >= from {
		in.last = from - 1
	}
	return in, in.first <= in.last
}

// positionBatches splits intervals into batches of at most size positions, cutting an interval
// where a batch fills.
func positionBatches(intervals []chainInterval, size uint64) [][]chainInterval {
	var batches [][]chainInterval
	var current []chainInterval
	room := size
	for _, in := range intervals {
		for first := uint64(in.first); first <= uint64(in.last); {
			n := min(uint64(in.last)-first+1, room)
			current = append(current, chainInterval{in.chain, uint32(first), uint32(first + n - 1)})
			first += n
			if room -= n; room == 0 {
				batches = append(batches, current)
				current, room = nil, size
			}
		}
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func (c *chainCover) backward(seeds []chainPosition) map[uint32]uint32 {
	reach := make(map[uint32]uint32)
	stack := slices.Clone(seeds)
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reach[at.chain] >= at.seq {
			continue
		}
		reach[at.chain] = at.seq
		for _, l := range c.links[at.chain] {
			if l.seq > at.seq {
				break
			}
			stack = append(stack, chainPosition{l.target, l.targetSeq})
		}
	}
	return reach
}

func (g *AuthGraph) assign(ev *types.Event, parents []*types.Event, headers []types.AuthHeader) (chainPosition, error) {
	c := g.cover
	var own chainPosition
	var seeds []chainPosition
	var chains []uint32
	for i, p := range parents {
		h := headers[i]
		if h.Chain == 0 {
			return own, fmt.Errorf("unlabelled auth parent %s", p.ID)
		}
		seeds = append(seeds, chainPosition{h.Chain, h.Sequence})
		chains = append(chains, h.Chain)
	}
	for i, p := range parents {
		if p.StateKey == nil || p.StateTup() != ev.StateTup() {
			continue
		}
		pos := seeds[i]
		tip, ok := c.tips[pos.chain]
		if !ok {
			var err error
			tip, err = c.loadTip(pos.chain)
			if err != nil {
				return own, err
			}
			c.tips[pos.chain] = tip
		}
		if tip == pos.seq && tip < math.MaxUint32 {
			own = chainPosition{pos.chain, tip + 1}
		}
		break
	}
	if own.chain == 0 {
		chain, err := c.allocate()
		if err != nil {
			return own, err
		}
		own = chainPosition{chain, 1}
		c.links[chain] = nil
	}
	if err := g.fetch(chains); err != nil {
		return own, err
	}
	closures := make([]map[uint32]uint32, len(seeds))
	for i, seed := range seeds {
		closures[i] = c.backward([]chainPosition{seed})
	}
	for i, target := range seeds {
		if target.chain == own.chain {
			continue
		}
		redundant := false
		for j, reach := range closures {
			if i != j && (seeds[i] != seeds[j] || j < i) && reach[target.chain] >= target.seq {
				redundant = true
				break
			}
		}
		if redundant {
			continue
		}
		for _, l := range c.links[own.chain] {
			if l.target == target.chain && l.targetSeq >= target.seq {
				redundant = true
				break
			}
		}
		if !redundant {
			l := chainLink{own.seq, target.chain, target.seq}
			c.links[own.chain] = append(c.links[own.chain], l)
			c.addedLinks[own.chain] = append(c.addedLinks[own.chain], l)
		}
	}
	c.tips[own.chain] = own.seq
	c.positions[own] = ev.ID
	if _, ok := c.pendingFrom[own.chain]; !ok {
		c.pendingFrom[own.chain] = own.seq
	}
	return own, nil
}

// ConflictedSubgraph returns the events on paths between conflicted events,
// including endpoints. Intersect backward maxima with forward minima on each
// chain, then load the overlapping intervals in bounded batches.
func (g *AuthGraph) ConflictedSubgraph(ids []id.EventID) ([]id.EventID, error) {
	headers, err := g.HeadersInBatches(ids)
	if err != nil {
		return nil, err
	}
	c := g.cover
	seeds := make([]chainPosition, len(headers))
	chains := make([]uint32, len(headers))
	for i, h := range headers {
		seeds[i] = chainPosition{h.Chain, h.Sequence}
		chains[i] = h.Chain
	}
	if err := g.fetch(chains); err != nil {
		return nil, err
	}
	back := c.backward(seeds)
	type inverse struct {
		targetSeq uint32
		source    chainPosition
	}
	inverted := make(map[uint32][]inverse)
	for chain := range back {
		for _, l := range c.links[chain] {
			if l.seq <= back[chain] {
				inverted[l.target] = append(inverted[l.target], inverse{l.targetSeq, chainPosition{chain, l.seq}})
			}
		}
	}
	forward := make(map[uint32]uint32)
	stack := slices.Clone(seeds)
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if first := forward[at.chain]; first != 0 && first <= at.seq {
			continue
		}
		forward[at.chain] = at.seq
		for _, l := range inverted[at.chain] {
			if l.targetSeq >= at.seq {
				stack = append(stack, l.source)
			}
		}
	}
	var intervals []chainInterval
	for chain, last := range back {
		if first := forward[chain]; first != 0 && first <= last {
			intervals = append(intervals, chainInterval{chain, first, last})
		}
	}
	between, err := g.chainEvents(intervals)
	if err != nil {
		return nil, err
	}
	out := slices.Concat(ids, between)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// AuthChainDifference returns, sorted, the events in the auth chains of some of the state sets but
// not all, each state event counted in its own set's, as Synapse's
// _get_auth_chain_difference_using_cover_index computes it. A set reaches, on each chain, the
// highest position its state events' own positions and the links below them reach. Every position
// of a chain above the lowest reach of any set, a set not reaching the chain having none, and at or
// below the highest is in the difference. It reads the state events' headers, the links of every
// chain they reach and the positions of the difference in bounded batches, so it
// costs O(state events + chains touched).
func (g *AuthGraph) AuthChainDifference(stateSets [][]id.EventID) ([]id.EventID, error) {
	var ids []id.EventID
	for _, set := range stateSets {
		ids = append(ids, set...)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	headers, err := g.HeadersInBatches(ids)
	if err != nil {
		return nil, err
	}
	positions := make(map[id.EventID]chainPosition, len(ids))
	chains := make([]uint32, 0, len(ids))
	for i, h := range headers {
		if h.Chain == 0 {
			return nil, fmt.Errorf("%w: state event %s has no chain position", ErrAuthEventNotFinalized, ids[i])
		}
		positions[ids[i]] = chainPosition{h.Chain, h.Sequence}
		chains = append(chains, h.Chain)
	}
	c := g.cover
	if err := g.fetch(chains); err != nil {
		return nil, err
	}
	reaches := make([]map[uint32]uint32, len(stateSets))
	reached := make(map[uint32]struct{})
	for i, set := range stateSets {
		seeds := make([]chainPosition, len(set))
		for j, eventID := range set {
			seeds[j] = positions[eventID]
		}
		reaches[i] = c.backward(seeds)
		for chain := range reaches[i] {
			reached[chain] = struct{}{}
		}
	}
	var intervals []chainInterval
	for _, chain := range slices.Sorted(maps.Keys(reached)) {
		lowest, highest := uint32(math.MaxUint32), uint32(0)
		for _, reach := range reaches {
			lowest, highest = min(lowest, reach[chain]), max(highest, reach[chain])
		}
		if lowest < highest {
			intervals = append(intervals, chainInterval{chain, lowest + 1, highest})
		}
	}
	difference, err := g.chainEvents(intervals)
	if err != nil {
		return nil, err
	}
	slices.Sort(difference)
	return difference, nil
}

// chainEvents returns the events at every position of the intervals, interval by interval in
// sequence order, reading at most positionBatch stored positions per batch.
// Positions at or above the lowest the graph assigned on a chain come from memory and the rest
// from storage, so a chain staging appended to since is read only below them.
func (g *AuthGraph) chainEvents(intervals []chainInterval) ([]id.EventID, error) {
	c := g.cover
	var storedIntervals []chainInterval
	for _, in := range intervals {
		if in.first == 0 || in.first > in.last {
			return nil, fmt.Errorf("invalid chain interval %d from %d to %d", in.chain, in.first, in.last)
		}
		if part, ok := c.stored(in); ok {
			storedIntervals = append(storedIntervals, part)
		}
	}
	stored, err := g.readPositions(storedIntervals)
	if err != nil {
		return nil, err
	}
	var out []id.EventID
	for _, in := range intervals {
		for seq := in.first; ; seq++ {
			if from, ok := c.pendingFrom[in.chain]; ok && seq >= from {
				eventID, ok := c.positions[chainPosition{in.chain, seq}]
				if !ok {
					return nil, fmt.Errorf("auth chain position %d of chain %d is above those the graph holds", seq, in.chain)
				}
				out = append(out, eventID)
			} else {
				out, stored = append(out, stored[0]), stored[1:]
			}
			if seq == in.last {
				break
			}
		}
	}
	return out, nil
}

func (g *AuthGraph) writeChains(txn fdb.Transaction) {
	c := g.cover
	for pos, eventID := range c.positions {
		txn.Set(g.dir.authChainPositions.Pack(tuple.Tuple{g.roomID.String(), int64(pos.chain), int64(pos.seq)}), packChainEntry(eventID))
	}
	for chain, links := range c.addedLinks {
		for _, l := range links {
			txn.Set(g.dir.authChainLinks.Pack(tuple.Tuple{g.roomID.String(), int64(chain), int64(l.seq), int64(l.target), int64(l.targetSeq)}), nil)
		}
	}
	if c.allocated {
		txn.Set(g.dir.authChainNext.Pack(tuple.Tuple{g.roomID.String()}), tuple.Tuple{int64(c.next)}.Pack())
	}
}
