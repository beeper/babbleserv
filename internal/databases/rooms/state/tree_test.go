package state

import (
	"bytes"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/babbleserv/internal/types"
)

var smallLimits = leafLimits{entries: 4, bytes: 1 << 20}

type treeHarness struct {
	t      *testing.T
	store  memRoom
	cache  *cache
	limits leafLimits
}

func newTreeHarness(t *testing.T, limits leafLimits) *treeHarness {
	return &treeHarness{
		t:      t,
		store:  newMemStore().room(testRoomID),
		cache:  newCache(0),
		limits: limits,
	}
}

func (h *treeHarness) tree() *tree {
	tr := newTree(h.cache, h.store.roomID, h.limits)
	return &tr
}

func (h *treeHarness) applyWith(tr *tree, deltas ...mapDelta) []types.StateHash {
	roots, err := tr.apply(h.store, deltas...)
	require.NoError(h.t, err)
	return roots
}

func (h *treeHarness) apply(root types.StateHash, delta map[string]string) types.StateHash {
	tr := h.tree()
	next := h.applyWith(tr, mapDelta{root, toDelta(delta)})[0]
	tr.write(h.store, next)
	return next
}

func (h *treeHarness) build(contents map[string]string) types.StateHash {
	return h.applyWith(h.tree(), mapDelta{types.StateHash{}, toDelta(contents)})[0]
}

func (h *treeHarness) contents(root types.StateHash) map[string]string {
	return h.contentsWith(h.tree(), root)
}

func (h *treeHarness) contentsWith(tr *tree, root types.StateHash) map[string]string {
	out := make(map[string]string)
	require.NoError(h.t, tr.iterate(h.store, []types.StateHash{root}, func(_ int, e entry) error {
		if _, dup := out[string(e.key)]; dup {
			h.t.Fatalf("iterate returned %q twice", e.key)
		}
		out[string(e.key)] = string(e.value)
		return nil
	}))
	return out
}

func (h *treeHarness) lookup(root types.StateHash, keys []string) map[string]string {
	return h.lookupWith(h.tree(), root, keys)
}

func (h *treeHarness) lookupWith(tr *tree, root types.StateHash, keys []string) map[string]string {
	probes := make([]probe, len(keys))
	for i, key := range keys {
		probes[i] = probe{root, []byte(key)}
	}
	values, err := tr.lookup(h.store, probes)
	require.NoError(h.t, err)
	out := make(map[string]string)
	for i, value := range values {
		if value != nil {
			out[keys[i]] = string(value)
		}
	}
	return out
}

func (h *treeHarness) diff(from, to types.StateHash) []string {
	changes, err := h.tree().diff(h.store, rootPair{from, to})
	require.NoError(h.t, err)
	slices.SortFunc(changes[0], func(a, b keyChange) int {
		return bytes.Compare(a.key, b.key)
	})
	out := make([]string, len(changes[0]))
	for i, c := range changes[0] {
		out[i] = fmt.Sprintf("%s:%s>%s", c.key, c.old, c.new)
	}
	return out
}

func (h *treeHarness) page(hash types.StateHash) *page {
	pages := make(pageSet)
	require.NoError(h.t, h.tree().fetch(h.store, pages, []types.StateHash{hash}))
	return pages[hash]
}

// requireCanonical walks a stored tree checking that leaves fit and are sorted, every key sits
// under the branch slots of its bucket hash, branch totals are exact and no branch would fit in a
// single leaf.
func (h *treeHarness) requireCanonical(root types.StateHash) {
	var walk func(hash types.StateHash, path []int) (int, int)
	walk = func(hash types.StateHash, path []int) (int, int) {
		p := h.page(hash)
		if p.leaf {
			if len(p.entries) == 0 || p.count != len(p.entries) || !h.limits.fits(p.count, p.bytes) {
				h.t.Fatalf("leaf %x has %d entries in %d bytes", hash, len(p.entries), p.bytes)
			}
			for i, e := range p.entries {
				if i > 0 && bytes.Compare(p.entries[i-1].key, e.key) >= 0 {
					h.t.Fatalf("leaf %x is not strictly sorted at %q", hash, e.key)
				}
				bucket := bucketOf(e.key)
				for d, s := range path {
					if slot(bucket, d) != s {
						h.t.Fatalf("key %q is under slot %d at depth %d", e.key, s, d)
					}
				}
			}
			return p.count, p.bytes
		}

		var count, size int
		for i, child := range p.children {
			if !child.IsZero() {
				c, s := walk(child, append(slices.Clone(path), i))
				count, size = count+c, size+s
			}
		}
		if count != p.count || size != p.bytes {
			h.t.Fatalf("branch %x records %d entries in %d bytes, holds %d in %d", hash, p.count, p.bytes, count, size)
		} else if h.limits.fits(count, size) {
			h.t.Fatalf("branch %x with %d entries fits in a leaf", hash, count)
		}
		return count, size
	}
	if !root.IsZero() {
		walk(root, nil)
	}
}

func toDelta(contents map[string]string) map[string][]byte {
	delta := make(map[string][]byte, len(contents))
	for k, v := range contents {
		delta[k] = []byte(v)
	}
	return delta
}

func expectedDiff(from, to map[string]string) []string {
	out := []string{}
	for _, key := range slices.Sorted(maps.Keys(keysOf(from, to))) {
		if from[key] != to[key] {
			out = append(out, fmt.Sprintf("%s:%s>%s", key, from[key], to[key]))
		}
	}
	return out
}

func keysOf(ms ...map[string]string) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, m := range ms {
		for k := range m {
			keys[k] = struct{}{}
		}
	}
	return keys
}

func randomValue(rng *rand.Rand) string {
	return "$" + strings.Repeat(string(rune('a'+rng.IntN(26))), 1+rng.IntN(24))
}

func modelKey(i int) string {
	if i%3 == 0 {
		return fmt.Sprintf("k\x00%d", i)
	}
	return fmt.Sprintf("k%d", i)
}

type version struct {
	root     types.StateHash
	contents map[string]string
}

func randomDelta(rng *rand.Rand, step, keySpace int, model map[string]string) map[string]string {
	delta := make(map[string]string)
	if step%100 == 99 {
		for key := range model {
			delta[key] = ""
		}
		return delta
	}
	deleteChance := []float64{0.1, 0.95}[(step/40)%2]
	for range 1 + rng.IntN(12) {
		key := modelKey(rng.IntN(keySpace))
		if rng.Float64() < deleteChance {
			delta[key] = ""
		} else {
			delta[key] = randomValue(rng)
		}
	}
	return delta
}

func applyToModel[K comparable, V comparable](model, delta map[K]V) {
	var absent V
	for key, value := range delta {
		if value == absent {
			delete(model, key)
		} else {
			model[key] = value
		}
	}
}

func modelKeys(keySpace int) []string {
	keys := make([]string, 0, keySpace+10)
	for i := range keySpace {
		keys = append(keys, modelKey(i))
	}
	for i := range 10 {
		keys = append(keys, fmt.Sprintf("absent\x00%d", i))
	}
	return keys
}

func TestTreeMatchesModel(t *testing.T) {
	const keySpace, steps = 250, 250

	for _, limits := range []leafLimits{smallLimits, {entries: 4, bytes: 48}, {entries: 1, bytes: 1 << 20}} {
		for seed := range uint64(3) {
			t.Run(fmt.Sprintf("entries=%d,bytes=%d,seed=%d", limits.entries, limits.bytes, seed), func(t *testing.T) {
				h := newTreeHarness(t, limits)
				rng := rand.New(rand.NewPCG(seed, seed))
				allKeys := modelKeys(keySpace)

				history := []version{{contents: map[string]string{}}}
				model := make(map[string]string)
				var root types.StateHash

				for step := range steps {
					delta := randomDelta(rng, step, keySpace, model)
					applyToModel(model, delta)
					root = h.apply(root, delta)
					history = append(history, version{root, maps.Clone(model)})

					require.Equal(t, model, h.lookup(root, allKeys))
					require.Equal(t, model, h.contents(root))
					require.Equal(t, h.build(model), root, "root depends on history, not only contents")
					h.requireCanonical(root)
					if len(model) == 0 {
						require.True(t, root.IsZero())
					}

					from, to := history[rng.IntN(len(history))], history[rng.IntN(len(history))]
					require.Equal(t, expectedDiff(from.contents, to.contents), h.diff(from.root, to.root))
				}

				for _, v := range history {
					require.Equal(t, v.contents, h.lookup(v.root, allKeys))
					require.Equal(t, v.contents, h.contents(v.root))
				}
			})
		}
	}
}

func TestTreeLongLivedBatchWithPartialWrites(t *testing.T) {
	const keySpace, steps = 200, 200
	h := newTreeHarness(t, leafLimits{entries: 4, bytes: 64})
	rng := rand.New(rand.NewPCG(11, 11))
	allKeys := modelKeys(keySpace)
	tr := h.tree()

	models := [2]map[string]string{{}, {}}
	var roots [2]types.StateHash
	var written []version
	for step := range steps {
		var deltas [2]mapDelta
		for i := range models {
			delta := randomDelta(rng, step+i*20, keySpace, models[i])
			applyToModel(models[i], delta)
			deltas[i] = mapDelta{roots[i], toDelta(delta)}
		}
		copy(roots[:], h.applyWith(tr, deltas[:]...))

		for i := range models {
			require.Equal(t, models[i], h.lookupWith(tr, roots[i], allKeys))
			require.Equal(t, models[i], h.contentsWith(tr, roots[i]))
		}
		if step%4 == 0 {
			tr.write(h.store, roots[:]...)
			written = append(written, version{roots[0], maps.Clone(models[0])}, version{roots[1], maps.Clone(models[1])})
		}
	}

	for _, v := range written {
		require.Equal(t, v.contents, h.contents(v.root))
		require.Equal(t, v.contents, h.lookup(v.root, allKeys))
		h.requireCanonical(v.root)
	}
}

func TestTreeShrinkingReadsOnlyChangedPaths(t *testing.T) {
	h := newTreeHarness(t, defaultLeafLimits)
	contents := make(map[string]string)
	for i := range 5000 {
		contents[fmt.Sprintf("@user%d:example.com", i)] = fmt.Sprintf("$join%d", i)
	}
	root := h.apply(types.StateHash{}, contents)

	h.store.resetCounts()
	h.lookup(root, []string{"@user7:example.com"})
	pathLength := h.store.pageReads
	require.Greater(t, pathLength, 1)

	h.store.resetCounts()
	h.apply(root, map[string]string{"@user7:example.com": ""})
	assert.Equal(t, pathLength, h.store.pageReads)
	assert.Equal(t, pathLength, h.store.pageReadCalls)

	h.store.resetCounts()
	h.apply(root, map[string]string{"@user8:example.com": "$x"})
	assert.Equal(t, pathLength, h.store.pageReads)

	delta := make(map[string]string)
	for i := range 50 {
		delta[fmt.Sprintf("@user%d:example.com", i*97)] = ""
	}
	h.store.resetCounts()
	h.apply(root, delta)
	assert.Equal(t, pathLength, h.store.pageReadCalls, "one batched read per level")
}

func TestTreePagesStayUnderValueLimit(t *testing.T) {
	h := newTreeHarness(t, defaultLeafLimits)
	rng := rand.New(rand.NewPCG(3, 3))
	contents := make(map[string]string)
	for i := range 600 {
		key := string(packStateKey(types.StateTup{
			Type:     event.NewEventType(fmt.Sprintf("%03d", i) + strings.Repeat("t", 252)),
			StateKey: strings.Repeat("\x00", 1+rng.IntN(254)),
		}))
		contents[key] = "$" + strings.Repeat("e", 254)
	}
	root := h.apply(types.StateHash{}, contents)
	h.requireCanonical(root)
	assert.Equal(t, contents, h.contents(root))

	for _, raw := range h.store.pages {
		assert.LessOrEqual(t, len(raw), 100_000)
		if p, err := decodePage(raw); assert.NoError(t, err) && p.leaf {
			assert.LessOrEqual(t, len(raw), leafMaxBytes)
		}
	}
}

func TestTreeRejectsEntriesLargerThanAPage(t *testing.T) {
	h := newTreeHarness(t, defaultLeafLimits)
	key := strings.Repeat("k", 1000)
	largest := strings.Repeat("v", leafMaxBytes-1-4-len(key))
	root := h.apply(types.StateHash{}, map[string]string{key: largest})
	assert.Equal(t, map[string]string{key: largest}, h.contents(root))

	_, err := h.tree().apply(h.store, mapDelta{root, map[string][]byte{key: []byte(largest + "v")}})
	assert.ErrorIs(t, err, ErrEntryTooLarge)
	_, err = h.tree().apply(h.store, mapDelta{changes: map[string][]byte{strings.Repeat("x", 70_000): []byte("$v")}})
	assert.ErrorIs(t, err, ErrEntryTooLarge)
}

func TestTreeDiffAcrossBranchBoundaries(t *testing.T) {
	h := newTreeHarness(t, smallLimits)
	small := map[string]string{"a": "$1", "b": "$2", "c": "$3"}
	large := map[string]string{"a": "$1", "b": "$changed"}
	for i := range 30 {
		large[fmt.Sprintf("n%02d", i)] = "$new"
	}

	smallRoot := h.apply(types.StateHash{}, small)
	largeRoot := h.apply(types.StateHash{}, large)
	largerRoot := h.apply(largeRoot, map[string]string{"n07": "$again"})

	h.store.resetCounts()
	assert.Equal(t, []string{"n07:$new>$again"}, h.diff(largeRoot, largerRoot))
	assert.Less(t, h.store.pageReads, 12, "diff should only read the changed path")

	changes, err := h.tree().diff(h.store, rootPair{smallRoot, largeRoot}, rootPair{largeRoot, largerRoot})
	require.NoError(t, err)
	assert.Len(t, changes[0], 32)
	assert.Len(t, changes[1], 1)
}

func TestTreeWriteSkipsIntermediatePages(t *testing.T) {
	h := newTreeHarness(t, smallLimits)
	tr := h.tree()

	var root types.StateHash
	for i := range 30 {
		root = h.applyWith(tr, mapDelta{root, map[string][]byte{fmt.Sprintf("k%d", i): []byte("$v")}})[0]
	}
	assert.Empty(t, h.store.pages)

	tr.write(h.store, root)
	reachable := make(map[types.StateHash]struct{})
	var walk func(types.StateHash)
	walk = func(hash types.StateHash) {
		if hash.IsZero() {
			return
		}
		reachable[hash] = struct{}{}
		if p := tr.pending[hash].page; !p.leaf {
			for _, child := range p.children {
				walk(child)
			}
		}
	}
	walk(root)
	assert.Len(t, h.store.pages, len(reachable))
	assert.Greater(t, len(tr.pending), len(reachable))
}

func TestTreeMissingPageIsAnError(t *testing.T) {
	h := newTreeHarness(t, smallLimits)
	_, err := h.tree().lookup(h.store, []probe{{hashOf([]byte("nowhere")), []byte("key")}})
	assert.ErrorIs(t, err, ErrPageNotFound)

	root := h.apply(types.StateHash{}, map[string]string{"key": "$v"})
	for key := range h.store.pages {
		h.store.pages[key] = encodeLeaf([]entry{{[]byte("key"), []byte("$tampered")}})
	}
	_, err = h.tree().lookup(h.store, []probe{{root, []byte("key")}})
	assert.ErrorIs(t, err, ErrInvalidPage)
}
