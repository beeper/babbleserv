package state

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"math/bits"
	"slices"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var (
	ErrPageNotFound  = errors.New("state page not found")
	ErrEntryTooLarge = errors.New("state entry does not fit in a page")
)

type pageReader interface {
	// readPages returns the stored bytes of each page in one round trip, a job batch's in one per
	// bounded batch, nil for a missing page.
	readPages(hashes []types.StateHash) ([][]byte, error)
	// cacheable reports whether everything read is committed, so may be cached: false for a
	// transaction that wrote state rows, which it would read back before they commit.
	cacheable() bool
}

type pageWriter interface {
	writePage(h types.StateHash, raw []byte)
}

type pendingPage struct {
	raw  []byte
	page *page
}

type pageSet map[types.StateHash]*page

// tree implements immutable maps from byte keys to byte values stored in one room. The form is
// canonical: a subtree that fits the leaf limits is one leaf and a larger one is a branch on the
// next nibble of each key's bucket hash, so a root depends only on the map contents. Pages
// produced by apply stay pending until committed, and reads consult pending, then the cache, then
// the reader.
type tree struct {
	roomID  id.RoomID
	limits  leafLimits
	cache   *cache
	pending map[types.StateHash]pendingPage
}

type leafLimits struct {
	entries, bytes int
}

var defaultLeafLimits = leafLimits{entries: leafCapacity, bytes: leafMaxBytes}

// fits reports whether entries with this total encoded size make a single leaf, whose encoding
// adds a one byte tag. Fitting is monotone, so a branch never fits and neither does any subtree
// containing one.
func (l leafLimits) fits(entries, bytes int) bool {
	return entries <= l.entries && bytes+1 <= l.bytes
}

func newTree(c *cache, roomID id.RoomID, limits leafLimits) tree {
	return tree{
		roomID:  roomID,
		limits:  limits,
		cache:   c,
		pending: make(map[types.StateHash]pendingPage),
	}
}

func (t *tree) fetch(r pageReader, pages pageSet, hashes []types.StateHash) error {
	var missing []types.StateHash
	for _, h := range hashes {
		if h.IsZero() {
			continue
		} else if _, ok := pages[h]; ok {
			continue
		} else if p, ok := t.pending[h]; ok {
			pages[h] = p.page
		} else if p := t.cache.getPage(t.roomID, h); p != nil {
			pages[h] = p
		} else {
			// Placeholder so repeated hashes are read once, filled below or the fetch fails.
			pages[h] = nil
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	raws, err := r.readPages(missing)
	if err != nil {
		return err
	}
	for i, h := range missing {
		raw := raws[i]
		if raw == nil {
			return fmt.Errorf("%w: %x", ErrPageNotFound, h)
		} else if hashOf(raw) != h {
			return fmt.Errorf("%w: %x does not match its hash", ErrInvalidPage, h)
		}
		p, err := decodePage(raw)
		if err != nil {
			return fmt.Errorf("page %x: %w", h, err)
		}
		if r.cacheable() {
			t.cache.add(cacheKey{t.roomID, h}, p, cacheCost(raw, p))
		}
		pages[h] = p
	}
	return nil
}

type cursor struct {
	at     types.StateHash
	bucket uint64
}

// loadPaths reads every page from each cursor down to the leaf covering its bucket, one batched
// read per level. Cursors must not start at the zero hash.
func (t *tree) loadPaths(r pageReader, pages pageSet, cursors []cursor) error {
	for depth := 0; len(cursors) > 0; depth++ {
		hashes := make([]types.StateHash, len(cursors))
		for i, c := range cursors {
			hashes[i] = c.at
		}
		if err := t.fetch(r, pages, hashes); err != nil {
			return err
		}

		next := cursors[:0]
		for _, c := range cursors {
			if p := pages[c.at]; !p.leaf {
				if child := p.children[slot(c.bucket, depth)]; !child.IsZero() {
					next = append(next, cursor{child, c.bucket})
				}
			}
		}
		cursors = next
	}
	return nil
}

func (pages pageSet) find(root types.StateHash, key []byte, bucket uint64) []byte {
	h := root
	for depth := 0; !h.IsZero(); depth++ {
		p := pages[h]
		if p.leaf {
			return p.find(key)
		}
		h = p.children[slot(bucket, depth)]
	}
	return nil
}

type probe struct {
	root types.StateHash
	key  []byte
}

// lookup returns the value for each probe's key in its root's map, nil when absent. Probes may
// target different roots and share one batched read per level.
func (t *tree) lookup(r pageReader, probes []probe) ([][]byte, error) {
	buckets := make([]uint64, len(probes))
	cursors := make([]cursor, 0, len(probes))
	for i, p := range probes {
		buckets[i] = bucketOf(p.key)
		if !p.root.IsZero() {
			cursors = append(cursors, cursor{p.root, buckets[i]})
		}
	}

	pages := make(pageSet)
	if err := t.loadPaths(r, pages, cursors); err != nil {
		return nil, err
	}

	values := make([][]byte, len(probes))
	for i, p := range probes {
		values[i] = pages.find(p.root, p.key, buckets[i])
	}
	return values, nil
}

// iterate calls fn with every entry of each root's map, reading one level of all the maps per
// batch. The order is deterministic but is not key order.
func (t *tree) iterate(r pageReader, roots []types.StateHash, fn func(root int, e entry) error) error {
	type node struct {
		root int
		hash types.StateHash
	}

	var frontier []node
	for i, h := range roots {
		if !h.IsZero() {
			frontier = append(frontier, node{i, h})
		}
	}

	for len(frontier) > 0 {
		hashes := make([]types.StateHash, len(frontier))
		for i, n := range frontier {
			hashes[i] = n.hash
		}
		pages := make(pageSet, len(hashes))
		if err := t.fetch(r, pages, hashes); err != nil {
			return err
		}

		var next []node
		for _, n := range frontier {
			p := pages[n.hash]
			if !p.leaf {
				for _, child := range p.children {
					if !child.IsZero() {
						next = append(next, node{n.root, child})
					}
				}
				continue
			}
			for _, e := range p.entries {
				if err := fn(n.root, e); err != nil {
					return err
				}
			}
		}
		frontier = next
	}
	return nil
}

// iterateFrom calls fn with the entries of the leaves of root's map, a leaf at a time in bucket order,
// from the leaf starting at bucket from until the leaves seen hold at least limit entries. Each
// branch's children are read together. It returns the bucket the next leaf starts at, and false
// once no leaf is left.
func (t *tree) iterateFrom(r pageReader, root types.StateHash, from uint64, limit int, fn func(e entry) error) (uint64, bool, error) {
	type node struct {
		hash  types.StateHash
		depth int
		first uint64
	}

	pages := make(pageSet)
	if err := t.fetch(r, pages, []types.StateHash{root}); err != nil {
		return 0, false, err
	}
	stack := []node{{hash: root}}
	seen := 0
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		end, last := subtreeEnd(n.first, n.depth)
		if n.hash.IsZero() || (!last && end <= from) {
			continue
		}

		p := pages[n.hash]
		if !p.leaf {
			children := make([]node, 0, fanout)
			for s, child := range p.children {
				first := n.first | uint64(s)<<(60-4*n.depth)
				if childEnd, childLast := subtreeEnd(first, n.depth+1); !child.IsZero() && (childLast || childEnd > from) {
					children = append(children, node{child, n.depth + 1, first})
				}
			}
			hashes := make([]types.StateHash, len(children))
			for i, child := range children {
				hashes[i] = child.hash
			}
			if err := t.fetch(r, pages, hashes); err != nil {
				return 0, false, err
			}
			for i := len(children) - 1; i >= 0; i-- {
				stack = append(stack, children[i])
			}
			continue
		}

		for _, e := range p.entries {
			if err := fn(e); err != nil {
				return 0, false, err
			}
		}
		if seen += len(p.entries); last {
			return 0, false, nil
		} else if seen >= limit && len(stack) > 0 {
			return end, true, nil
		}
	}
	return 0, false, nil
}

// subtreeEnd returns the first bucket past those of the subtree at depth starting at bucket first,
// and true for a subtree ending the bucket space.
func subtreeEnd(first uint64, depth int) (uint64, bool) {
	if depth == 0 {
		return 0, true
	}
	end, carry := bits.Add64(first, 1<<(64-4*depth), 0)
	return end, carry != 0
}

type keyChange struct {
	key, old, new []byte
}

type rootPair struct {
	from, to types.StateHash
}

// diff returns for each pair the keys whose values differ between its two maps, in no particular
// order, with nil for a side where the key is absent. All pairs share one batched read per level.
// Subtrees with equal hashes are skipped; where a leaf or empty slot meets a branch both sides of
// that range are enumerated.
func (t *tree) diff(r pageReader, pairs ...rootPair) ([][]keyChange, error) {
	type node struct {
		pair int
		rootPair
	}

	var frontier, boundaries []node
	for i, p := range pairs {
		if p.from != p.to {
			frontier = append(frontier, node{i, p})
		}
	}

	for len(frontier) > 0 {
		hashes := make([]types.StateHash, 0, 2*len(frontier))
		for _, n := range frontier {
			hashes = append(hashes, n.from, n.to)
		}
		pages := make(pageSet, len(hashes))
		if err := t.fetch(r, pages, hashes); err != nil {
			return nil, err
		}

		var next []node
		for _, n := range frontier {
			a, b := pages[n.from], pages[n.to]
			if a == nil || b == nil || a.leaf || b.leaf {
				boundaries = append(boundaries, n)
				continue
			}
			for i := range fanout {
				if a.children[i] != b.children[i] {
					next = append(next, node{n.pair, rootPair{a.children[i], b.children[i]}})
				}
			}
		}
		frontier = next
	}

	roots := make([]types.StateHash, 0, 2*len(boundaries))
	for _, n := range boundaries {
		roots = append(roots, n.from, n.to)
	}
	sides := make([][]entry, len(roots))
	if err := t.iterate(r, roots, func(root int, e entry) error {
		sides[root] = append(sides[root], e)
		return nil
	}); err != nil {
		return nil, err
	}

	changes := make([][]keyChange, len(pairs))
	for i, n := range boundaries {
		changes[n.pair] = appendChanges(changes[n.pair], sides[2*i], sides[2*i+1])
	}
	return changes, nil
}

func appendChanges(changes []keyChange, from, to []entry) []keyChange {
	before := make(map[string][]byte, len(from))
	for _, e := range from {
		before[string(e.key)] = e.value
	}
	for _, e := range to {
		old, ok := before[string(e.key)]
		delete(before, string(e.key))
		if !ok || !bytes.Equal(old, e.value) {
			changes = append(changes, keyChange{e.key, old, e.value})
		}
	}
	for key, old := range before {
		changes = append(changes, keyChange{[]byte(key), old, nil})
	}
	return changes
}

type change struct {
	key    []byte
	value  []byte
	bucket uint64
}

func (c change) isDelete() bool {
	return c.value == nil
}

// growth is the change in a subtree's entry count and encoded entry bytes.
type growth struct {
	entries, bytes int
}

type mapDelta struct {
	root    types.StateHash
	changes map[string][]byte
}

// apply returns the new root of each map with its changes applied, an empty value deleting the
// key. Pages on changed paths are copied into new pending pages and everything else is shared.
// The paths of all maps are read together, one batched read per level.
func (t *tree) apply(r pageReader, deltas ...mapDelta) ([]types.StateHash, error) {
	sorted := make([][]change, len(deltas))
	var cursors []cursor
	for i, d := range deltas {
		changes := make([]change, 0, len(d.changes))
		for k, v := range d.changes {
			c := change{key: []byte(k), bucket: bucketOf([]byte(k))}
			if len(v) > 0 {
				c.value = bytes.Clone(v)
				if !t.limits.fits(1, entry{c.key, c.value}.encodedSize()) {
					return nil, fmt.Errorf("%w: %d byte key, %d byte value", ErrEntryTooLarge, len(c.key), len(c.value))
				}
			}
			changes = append(changes, c)
			if !d.root.IsZero() {
				cursors = append(cursors, cursor{d.root, c.bucket})
			}
		}
		slices.SortFunc(changes, func(a, b change) int {
			return cmp.Or(cmp.Compare(a.bucket, b.bucket), bytes.Compare(a.key, b.key))
		})
		sorted[i] = changes
	}

	pages := make(pageSet)
	if err := t.loadPaths(r, pages, cursors); err != nil {
		return nil, err
	}

	roots := make([]types.StateHash, len(deltas))
	for i, d := range deltas {
		if len(sorted[i]) == 0 {
			roots[i] = d.root
			continue
		}
		root, _, err := t.update(r, pages, d.root, sorted[i], 0)
		if err != nil {
			return nil, err
		}
		roots[i] = root
	}
	return roots, nil
}

// update rewrites the subtree at h, whose pages on the paths of changes are already in pages.
// Changes are sorted by bucket so each branch slot's changes are contiguous.
func (t *tree) update(
	r pageReader,
	pages pageSet,
	h types.StateHash,
	changes []change,
	depth int,
) (types.StateHash, growth, error) {
	p := pages[h]
	if p == nil || p.leaf {
		var entries []entry
		if p != nil {
			entries = p.entries
		}
		merged, g, changed := mergeEntries(entries, changes)
		if !changed {
			return h, g, nil
		}
		return t.build(merged, depth), g, nil
	}

	children := p.children
	var g growth
	for lo := 0; lo < len(changes); {
		i := slot(changes[lo].bucket, depth)
		hi := lo + 1
		for hi < len(changes) && slot(changes[hi].bucket, depth) == i {
			hi++
		}
		child, childGrowth, err := t.update(r, pages, children[i], changes[lo:hi], depth+1)
		if err != nil {
			return types.StateHash{}, growth{}, err
		}
		children[i] = child
		g.entries, g.bytes = g.entries+childGrowth.entries, g.bytes+childGrowth.bytes
		lo = hi
	}

	if children == p.children {
		return h, g, nil
	}
	count, size := p.count+g.entries, p.bytes+g.bytes
	if !t.limits.fits(count, size) {
		return t.saveBranch(&children, count, size), g, nil
	}
	entries, err := t.gather(r, pages, &children)
	if err != nil {
		return types.StateHash{}, growth{}, err
	}
	return t.build(entries, depth), g, nil
}

// gather returns every entry under children sorted by key, for a subtree that now fits in one leaf.
// Every child of such a subtree is a leaf, and the ones not yet read cost one batched read.
func (t *tree) gather(r pageReader, pages pageSet, children *[fanout]types.StateHash) ([]entry, error) {
	if err := t.fetch(r, pages, children[:]); err != nil {
		return nil, err
	}

	var entries []entry
	for _, child := range children {
		if child.IsZero() {
			continue
		}
		p := pages[child]
		if !p.leaf {
			return nil, fmt.Errorf("%w: branch %x inside a subtree that fits in a leaf", ErrInvalidPage, child)
		}
		entries = append(entries, p.entries...)
	}
	slices.SortFunc(entries, compareEntries)
	return entries, nil
}

func compareEntries(a, b entry) int {
	return bytes.Compare(a.key, b.key)
}

func mergeEntries(entries []entry, changes []change) ([]entry, growth, bool) {
	changes = slices.Clone(changes)
	slices.SortFunc(changes, func(a, b change) int {
		return bytes.Compare(a.key, b.key)
	})

	merged := make([]entry, 0, len(entries)+len(changes))
	var g growth
	changed := false
	i := 0
	for _, c := range changes {
		for i < len(entries) && bytes.Compare(entries[i].key, c.key) < 0 {
			merged = append(merged, entries[i])
			i++
		}
		if i < len(entries) && bytes.Equal(entries[i].key, c.key) {
			if bytes.Equal(entries[i].value, c.value) {
				merged = append(merged, entries[i])
				i++
				continue
			}
			g.entries, g.bytes = g.entries-1, g.bytes-entries[i].encodedSize()
			changed = true
			i++
		}
		if !c.isDelete() {
			e := entry{c.key, c.value}
			g.entries, g.bytes = g.entries+1, g.bytes+e.encodedSize()
			changed = true
			merged = append(merged, e)
		}
	}
	return append(merged, entries[i:]...), g, changed
}

type bucketed struct {
	entry
	bucket uint64
}

// build returns the canonical subtree at depth holding entries, which are sorted by key.
func (t *tree) build(entries []entry, depth int) types.StateHash {
	if h, ok := t.saveLeaf(entries, depth); ok {
		return h
	}
	items := make([]bucketed, len(entries))
	for i, e := range entries {
		items[i] = bucketed{e, bucketOf(e.key)}
	}
	return t.buildBranch(items, depth)
}

func (t *tree) buildBranch(items []bucketed, depth int) types.StateHash {
	var groups [fanout][]bucketed
	for _, item := range items {
		i := slot(item.bucket, depth)
		groups[i] = append(groups[i], item)
	}

	var children [fanout]types.StateHash
	var count, size int
	for i, group := range groups {
		entries := make([]entry, len(group))
		for j, item := range group {
			entries[j] = item.entry
		}
		if h, ok := t.saveLeaf(entries, depth+1); ok {
			children[i] = h
		} else {
			children[i] = t.buildBranch(group, depth+1)
		}
		if child, ok := t.pending[children[i]]; ok {
			count, size = count+child.page.count, size+child.page.bytes
		}
	}
	return t.saveBranch(&children, count, size)
}

// saveLeaf saves entries as one leaf if they fit, or unconditionally at the maximum depth.
func (t *tree) saveLeaf(entries []entry, depth int) (types.StateHash, bool) {
	if len(entries) == 0 {
		return types.StateHash{}, true
	} else if len(entries) > t.limits.entries && depth < maxDepth {
		return types.StateHash{}, false
	}
	raw := encodeLeaf(entries)
	if !t.limits.fits(len(entries), len(raw)-1) && depth < maxDepth {
		return types.StateHash{}, false
	}
	return t.save(raw, &page{leaf: true, entries: entries, count: len(entries), bytes: len(raw) - 1}), true
}

func (t *tree) saveBranch(children *[fanout]types.StateHash, count, size int) types.StateHash {
	return t.save(encodeBranch(children, count, size), &page{children: *children, count: count, bytes: size})
}

func (t *tree) save(raw []byte, p *page) types.StateHash {
	h := hashOf(raw)
	t.pending[h] = pendingPage{raw, p}
	return h
}

// unwritten returns the pending pages reachable from roots, each after its children, skipping
// intermediates that no root uses. Every child of a pending page is pending or stored, so the walk
// stops at pages that are not pending.
func (t *tree) unwritten(roots ...types.StateHash) []types.StateHash {
	visited := make(map[types.StateHash]struct{})
	var order []types.StateHash
	var visit func(h types.StateHash)
	visit = func(h types.StateHash) {
		p, ok := t.pending[h]
		if !ok {
			return
		} else if _, done := visited[h]; done {
			return
		}
		visited[h] = struct{}{}
		if !p.page.leaf {
			for _, child := range p.page.children {
				visit(child)
			}
		}
		order = append(order, h)
	}
	for _, root := range roots {
		visit(root)
	}
	return order
}

// write stores the pending pages reachable from roots. Pending pages are kept so a retried
// transaction can write them again.
func (t *tree) write(w pageWriter, roots ...types.StateHash) {
	for _, h := range t.unwritten(roots...) {
		w.writePage(h, t.pending[h].raw)
	}
}

// committed moves pending pages a transaction committed into the cache.
func (t *tree) committed(hashes []types.StateHash) {
	for _, h := range hashes {
		p := t.pending[h]
		t.cache.add(cacheKey{t.roomID, h}, p.page, cacheCost(p.raw, p.page))
		delete(t.pending, h)
	}
}

func cacheCost(raw []byte, p *page) int {
	return len(raw) + decodedEntryCost*len(p.entries)
}
