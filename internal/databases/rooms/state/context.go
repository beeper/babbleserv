package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var (
	ErrUnknownContext  = errors.New("unknown state context")
	ErrContextNotFound = errors.New("state context not found")
	ErrInvalidContext  = errors.New("invalid state context")
	ErrInvalidEntry    = errors.New("invalid state entry")
)

// stateContext is the room state at a point in the DAG as the roots of two immutable maps:
// non-member state keyed by packed (type, stateKey) to event IDs, and members keyed by user ID to
// their membership and event ID, see encodeValue.
type stateContext struct {
	stateRoot, memberRoot types.StateHash
}

// EmptyContext is the ID of the context holding no state, it is never stored. The zero hash is not
// a context and means the state is unknown.
var EmptyContext = stateContext{}.ID()

func (c stateContext) ID() types.StateHash {
	return hashOf(c.encode())
}

func (c stateContext) encode() []byte {
	return tuple.Tuple{hashToBytes(c.stateRoot), hashToBytes(c.memberRoot)}.Pack()
}

func decodeContext(raw []byte) (stateContext, error) {
	tup, err := tuple.Unpack(raw)
	if err != nil {
		return stateContext{}, fmt.Errorf("%w: %w", ErrInvalidContext, err)
	} else if len(tup) != 2 {
		return stateContext{}, fmt.Errorf("%w: %d elements", ErrInvalidContext, len(tup))
	}
	var c stateContext
	if c.stateRoot, err = hashFromElement(tup[0]); err != nil {
		return stateContext{}, fmt.Errorf("%w: state root: %w", ErrInvalidContext, err)
	} else if c.memberRoot, err = hashFromElement(tup[1]); err != nil {
		return stateContext{}, fmt.Errorf("%w: member root: %w", ErrInvalidContext, err)
	}
	return c, nil
}

// A context has a map of non-member state and a map of members, indexed in that order by roots.
const (
	stateIndex = iota
	memberIndex
	contextMaps
)

func (c stateContext) roots() [contextMaps]types.StateHash {
	return [contextMaps]types.StateHash{stateIndex: c.stateRoot, memberIndex: c.memberRoot}
}

// tupleKey returns the index in roots of the map holding the tuple, and its key in that map.
func tupleKey(tup types.StateTup) (int, []byte) {
	if tup.Type.Type == event.StateMember.Type {
		return memberIndex, []byte(tup.StateKey)
	}
	return stateIndex, packStateKey(tup)
}

func keyTuple(m int, key []byte) (types.StateTup, error) {
	if m == memberIndex {
		return types.MemberStateTup(id.UserID(key)), nil
	}
	return unpackStateKey(key)
}

// The codes of the memberships member map values start with, part of the format. Zero is invalid.
var membershipCodes = [...]event.Membership{
	1: event.MembershipJoin,
	2: event.MembershipInvite,
	3: event.MembershipLeave,
	4: event.MembershipBan,
	5: event.MembershipKnock,
}

// encodeValue returns the value the map holds for an entry, nil for a removal. A member's value is
// its membership code then the event ID. Membership is a function of the event, so the value, and
// with it every root and context ID, still depends only on the event IDs.
func encodeValue(m int, entry types.StateEntry) ([]byte, error) {
	switch {
	case entry.EventID == "" && entry.Membership != "":
		return nil, fmt.Errorf("%w: removal with membership %q", ErrInvalidEntry, entry.Membership)
	case entry.EventID == "":
		return nil, nil
	case m != memberIndex && entry.Membership != "":
		return nil, fmt.Errorf("%w: %s is not a member event but has membership %q", ErrInvalidEntry, entry.EventID, entry.Membership)
	case m != memberIndex:
		return []byte(entry.EventID), nil
	}
	code := slices.Index(membershipCodes[:], entry.Membership)
	if code <= 0 {
		return nil, fmt.Errorf("%w: member event %s has invalid membership %q", ErrInvalidEntry, entry.EventID, entry.Membership)
	}
	return append([]byte{byte(code)}, entry.EventID...), nil
}

// decodeValue returns the entry of a value the map holds, the empty entry for nil.
func decodeValue(m int, value []byte) (types.StateEntry, error) {
	if value == nil {
		return types.StateEntry{}, nil
	} else if m != memberIndex {
		return types.StateEntry{EventID: id.EventID(value)}, nil
	} else if len(value) < 2 || int(value[0]) >= len(membershipCodes) || membershipCodes[value[0]] == "" {
		return types.StateEntry{}, fmt.Errorf("%w: member value %q", ErrInvalidPage, value)
	}
	return types.StateEntry{EventID: id.EventID(value[1:]), Membership: membershipCodes[value[0]]}, nil
}

// encodeChanges returns the changes of each map that apply the delta.
func encodeChanges(delta types.StateEntries) ([contextMaps]map[string][]byte, error) {
	var changes [contextMaps]map[string][]byte
	for m := range changes {
		changes[m] = make(map[string][]byte)
	}
	for tup, entry := range delta {
		m, key := tupleKey(tup)
		value, err := encodeValue(m, entry)
		if err != nil {
			return changes, fmt.Errorf("%v: %w", tup, err)
		}
		changes[m][string(key)] = value
	}
	return changes, nil
}

func packStateKey(tup types.StateTup) []byte {
	return tup.TupleElements().Pack()
}

func unpackStateKey(key []byte) (types.StateTup, error) {
	tup, err := tuple.Unpack(key)
	if err != nil {
		return types.StateTup{}, fmt.Errorf("%w: state key: %w", ErrInvalidPage, err)
	}
	if stateTup, ok := types.StateTupFromElements(tup); ok {
		return stateTup, nil
	}
	return types.StateTup{}, fmt.Errorf("%w: state key %v is not (type, stateKey)", ErrInvalidPage, tup)
}

type storeReader interface {
	pageReader
	// readContexts returns the stored bytes of each context in one round trip, a job batch's in one
	// per bounded batch, nil for a missing one.
	readContexts(ctxs []types.StateHash) ([][]byte, error)
	// readResolution starts reading the stored result of a resolution and returns a function waiting
	// for it, which gives nil when there is none.
	readResolution(key types.StateHash) func() ([]byte, error)
}

type storeWriter interface {
	pageWriter
	writeContext(ctx types.StateHash, raw []byte)
	writeResolution(key types.StateHash, raw []byte)
}

type storage interface {
	reader(txn fdb.ReadTransaction, roomID id.RoomID) storeReader
	writer(txn fdb.Transaction, roomID id.RoomID) storeWriter
	stage(ctx context.Context, roomID id.RoomID, fn func(storeWriter) error) error
	// Page and context keys of a room have the same length
	keySize(roomID id.RoomID) int
}

// Batch collects the contexts and pages produced for one room until they are written. Every read
// consults the batch first, so a context is usable before it is stored. A batch is not safe for
// concurrent use and may span transactions, reading in one and writing in another.
type Batch struct {
	store    storage
	roomID   id.RoomID
	tree     tree
	contexts map[types.StateHash]stateContext
}

func newBatch(store storage, c *cache, roomID id.RoomID, limits leafLimits) *Batch {
	return &Batch{
		store:    store,
		roomID:   roomID,
		tree:     newTree(c, roomID, limits),
		contexts: make(map[types.StateHash]stateContext),
	}
}

// resolve returns the roots of each context, reading the ones neither pending nor cached together.
func (b *Batch) resolve(r storeReader, ctxs ...types.StateHash) ([]stateContext, error) {
	resolved := make([]stateContext, len(ctxs))
	var missing []types.StateHash
	for i, ctx := range ctxs {
		if ctx == EmptyContext {
			continue
		} else if ctx.IsZero() {
			return nil, ErrUnknownContext
		} else if c, ok := b.contexts[ctx]; ok {
			resolved[i] = c
		} else if c, ok := b.tree.cache.getContext(b.roomID, ctx); ok {
			resolved[i] = c
		} else {
			missing = append(missing, ctx)
		}
	}

	read, err := b.readContexts(r, missing)
	if err != nil {
		return nil, err
	}
	for i, ctx := range ctxs {
		if c, ok := read[ctx]; ok {
			resolved[i] = c
		}
	}
	return resolved, nil
}

// readContexts reads each distinct context from storage in one round trip and caches it.
func (b *Batch) readContexts(r storeReader, ctxs []types.StateHash) (map[types.StateHash]stateContext, error) {
	unique := slices.Compact(slices.SortedFunc(slices.Values(ctxs), compareHashes))
	read := make(map[types.StateHash]stateContext, len(unique))
	if len(unique) == 0 {
		return read, nil
	}

	raws, err := r.readContexts(unique)
	if err != nil {
		return nil, err
	}
	for i, ctx := range unique {
		if raws[i] == nil {
			return nil, fmt.Errorf("%w: %x", ErrContextNotFound, ctx)
		}
		c, err := decodeContext(raws[i])
		if err != nil {
			return nil, fmt.Errorf("context %x: %w", ctx, err)
		} else if c.ID() != ctx {
			return nil, fmt.Errorf("%w: %x does not match its hash", ErrInvalidContext, ctx)
		}
		if r.cacheable() {
			b.tree.cache.add(cacheKey{b.roomID, ctx}, c, len(raws[i]))
		}
		read[ctx] = c
	}
	return read, nil
}

func compareHashes(a, b types.StateHash) int {
	return bytes.Compare(a[:], b[:])
}

func (b *Batch) context(r storeReader, ctx types.StateHash) (stateContext, error) {
	resolved, err := b.resolve(r, ctx)
	if err != nil {
		return stateContext{}, err
	}
	return resolved[0], nil
}

// TxnLookupEntries returns the entry of each tuple present in the context, with the membership of
// each member, reading the member and state maps together with one batched read per level.
func (b *Batch) TxnLookupEntries(
	txn fdb.ReadTransaction,
	ctx types.StateHash,
	tups []types.StateTup,
) (types.StateEntries, error) {
	return b.lookupEntries(b.store.reader(txn, b.roomID), ctx, tups)
}

func (b *Batch) lookupEntries(r storeReader, ctx types.StateHash, tups []types.StateTup) (types.StateEntries, error) {
	c, err := b.context(r, ctx)
	if err != nil {
		return nil, err
	}

	roots := c.roots()
	inMap := make([]int, len(tups))
	probes := make([]probe, len(tups))
	for i, tup := range tups {
		m, key := tupleKey(tup)
		inMap[i], probes[i] = m, probe{roots[m], key}
	}
	values, err := b.tree.lookup(r, probes)
	if err != nil {
		return nil, err
	}

	entries := make(types.StateEntries, len(tups))
	for i, value := range values {
		if value == nil {
			continue
		}
		if entries[tups[i]], err = decodeValue(inMap[i], value); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// TxnIterateEntries returns every entry of the context, with the membership of each member.
func (b *Batch) TxnIterateEntries(txn fdb.ReadTransaction, ctx types.StateHash) (types.StateEntries, error) {
	states, err := b.TxnIterateAll(txn, ctx)
	if err != nil {
		return nil, err
	}
	return states[0], nil
}

// TxnIterateAll returns every entry of each context, reading all of them together with one batched
// read per level.
func (b *Batch) TxnIterateAll(txn fdb.ReadTransaction, ctxs ...types.StateHash) ([]types.StateEntries, error) {
	return b.iterateAll(b.store.reader(txn, b.roomID), ctxs...)
}

// TxnIterateState returns the context's non-member state, reading none of its member map.
func (b *Batch) TxnIterateState(txn fdb.ReadTransaction, ctx types.StateHash) (types.StateMap, error) {
	r := b.store.reader(txn, b.roomID)
	c, err := b.context(r, ctx)
	if err != nil {
		return nil, err
	}
	stateMap := make(types.StateMap)
	err = b.tree.iterate(r, []types.StateHash{c.stateRoot}, func(_ int, e entry) error {
		tup, err := keyTuple(stateIndex, e.key)
		if err != nil {
			return err
		}
		stateMap[tup] = id.EventID(e.value)
		return nil
	})
	return stateMap, err
}

// TxnIterateMembers returns the context's members, reading none of its non-member state.
func (b *Batch) TxnIterateMembers(txn fdb.ReadTransaction, ctx types.StateHash) (map[id.UserID]types.StateEntry, error) {
	r := b.store.reader(txn, b.roomID)
	c, err := b.context(r, ctx)
	if err != nil {
		return nil, err
	}
	members := make(map[id.UserID]types.StateEntry)
	err = b.tree.iterate(r, []types.StateHash{c.memberRoot}, func(_ int, e entry) error {
		member, err := decodeValue(memberIndex, e.value)
		members[id.UserID(e.key)] = member
		return err
	})
	return members, err
}

// TxnMembersPage returns a page of the context's members: those of the leaves of its member map from
// the one starting at bucket from, until they hold at least limit members. It also returns the bucket
// the next page starts at, and false once the map is done. A page's bucket is only meaningful for the
// context it came from.
func (b *Batch) TxnMembersPage(
	txn fdb.ReadTransaction,
	ctx types.StateHash,
	from uint64,
	limit int,
) (map[id.UserID]types.StateEntry, uint64, bool, error) {
	r := b.store.reader(txn, b.roomID)
	c, err := b.context(r, ctx)
	if err != nil {
		return nil, 0, false, err
	}
	members := make(map[id.UserID]types.StateEntry)
	next, more, err := b.tree.iterateFrom(r, c.memberRoot, from, limit, func(e entry) error {
		member, err := decodeValue(memberIndex, e.value)
		members[id.UserID(e.key)] = member
		return err
	})
	return members, next, more, err
}

// TxnCount returns the number of tuples in the context, reading only the root page of each map.
func (b *Batch) TxnCount(txn fdb.ReadTransaction, ctx types.StateHash) (int, error) {
	r := b.store.reader(txn, b.roomID)
	c, err := b.context(r, ctx)
	if err != nil {
		return 0, err
	}
	counts, err := b.countAll(r, []stateContext{c})
	if err != nil {
		return 0, err
	}
	return counts[0], nil
}

// countAll returns the number of tuples in each context, reading the root pages of all their maps
// together.
func (b *Batch) countAll(r storeReader, contexts []stateContext) ([]int, error) {
	roots := make([]types.StateHash, 0, contextMaps*len(contexts))
	for _, c := range contexts {
		contextRoots := c.roots()
		roots = append(roots, contextRoots[:]...)
	}
	pages := make(pageSet, len(roots))
	if err := b.tree.fetch(r, pages, roots); err != nil {
		return nil, err
	}
	counts := make([]int, len(contexts))
	for i, c := range contexts {
		for _, root := range c.roots() {
			if p := pages[root]; p != nil {
				counts[i] += p.count
			}
		}
	}
	return counts, nil
}

// iterateAll returns the state of each context, reading all of them together with one batched read
// per level.
func (b *Batch) iterateAll(r storeReader, ctxs ...types.StateHash) ([]types.StateEntries, error) {
	resolved, err := b.resolve(r, ctxs...)
	if err != nil {
		return nil, err
	}

	roots := make([]types.StateHash, 0, contextMaps*len(resolved))
	states := make([]types.StateEntries, len(resolved))
	for i, c := range resolved {
		contextRoots := c.roots()
		roots = append(roots, contextRoots[:]...)
		states[i] = make(types.StateEntries)
	}
	err = b.tree.iterate(r, roots, func(root int, e entry) error {
		m := root % contextMaps
		tup, err := keyTuple(m, e.key)
		if err != nil {
			return err
		}
		states[root/contextMaps][tup], err = decodeValue(m, e.value)
		return err
	})
	return states, err
}

// TxnApply returns the context with delta applied, pending in the batch until written, an entry
// with an empty event ID removing the tuple. It fails with ErrInvalidEntry for a member set without
// its event's membership or another entry with one, and with ErrEntryTooLarge for an entry too large
// for a page.
func (b *Batch) TxnApply(
	txn fdb.ReadTransaction,
	ctx types.StateHash,
	delta types.StateEntries,
) (types.StateHash, error) {
	changes, err := encodeChanges(delta)
	if err != nil {
		return types.StateHash{}, err
	}
	r := b.store.reader(txn, b.roomID)
	c, err := b.context(r, ctx)
	if err != nil {
		return types.StateHash{}, err
	}

	var deltas [contextMaps]mapDelta
	for m, root := range c.roots() {
		deltas[m] = mapDelta{root: root, changes: changes[m]}
	}
	roots, err := b.tree.apply(r, deltas[:]...)
	if err != nil {
		return types.StateHash{}, err
	}

	next := stateContext{stateRoot: roots[stateIndex], memberRoot: roots[memberIndex]}
	if next == c {
		return ctx, nil
	}
	nextID := next.ID()
	if nextID != EmptyContext {
		b.contexts[nextID] = next
	}
	return nextID, nil
}

// Delta returns the changes TxnApply takes from a context holding from to one holding to.
func Delta(from, to types.StateEntries) types.StateEntries {
	delta := make(types.StateEntries)
	for tup, entry := range to {
		if from[tup] != entry {
			delta[tup] = entry
		}
	}
	for tup := range from {
		if _, found := to[tup]; !found {
			delta[tup] = types.StateEntry{}
		}
	}
	return delta
}

// TxnDiff returns the tuples whose event differs between two contexts, sorted by type and state key,
// with the memberships of member tuples.
func (b *Batch) TxnDiff(txn fdb.ReadTransaction, from, to types.StateHash) ([]types.StateChange, error) {
	diff, err := b.diffContexts(b.store.reader(txn, b.roomID), []types.StateHash{from, to})
	if err != nil {
		return nil, err
	}
	changes := make([]types.StateChange, 0, len(diff))
	for tup, entries := range diff {
		changes = append(changes, types.StateChange{
			StateTup:      tup,
			OldEventID:    entries[0].EventID,
			NewEventID:    entries[1].EventID,
			OldMembership: entries[0].Membership,
			NewMembership: entries[1].Membership,
		})
	}
	slices.SortFunc(changes, func(a, b types.StateChange) int {
		return a.Compare(b.StateTup)
	})
	return changes, nil
}

// TxnStateSince returns, sorted by type and state key, each tuple that one of tos sets to an event
// other than from's and outside skip, with the event of the last of tos to do so. Removed tuples are
// left out, and a context equal to from or to the one before it is not diffed.
func (b *Batch) TxnStateSince(
	txn fdb.ReadTransaction,
	from types.StateHash,
	tos []types.StateHash,
	skip map[id.EventID]struct{},
) ([]types.EventStateTup, error) {
	events := make(types.StateMap)
	for i, to := range tos {
		if to == from || i > 0 && to == tos[i-1] {
			continue
		}
		changes, err := b.TxnDiff(txn, from, to)
		if err != nil {
			return nil, err
		}
		for _, change := range changes {
			if _, skipped := skip[change.NewEventID]; change.NewEventID != "" && !skipped {
				events[change.StateTup] = change.NewEventID
			}
		}
	}
	since := make([]types.EventStateTup, 0, len(events))
	for tup, eventID := range events {
		since = append(since, types.EventStateTup{StateTup: tup, EventID: eventID})
	}
	slices.SortFunc(since, func(a, b types.EventStateTup) int {
		return a.Compare(b.StateTup)
	})
	return since, nil
}

// TxnWrite stores those of the given contexts that are pending in this batch and every pending page
// they reach. Each context passed must be pending or stored. The batch keeps its pending records so
// a retried transaction can write them again.
func (b *Batch) TxnWrite(txn fdb.Transaction, ctxs ...types.StateHash) {
	w := b.store.writer(txn, b.roomID)
	records := b.unwritten(ctxs)
	for _, h := range records.pages {
		w.writePage(h, b.tree.pending[h].raw)
	}
	for _, ctx := range records.contexts {
		w.writeContext(ctx, b.contexts[ctx].encode())
	}
}

// unwrittenRecords are what writing contexts stores: those pending in a batch and every pending page
// they reach, each after its children.
type unwrittenRecords struct {
	pages, contexts []types.StateHash
}

func (b *Batch) unwritten(ctxs []types.StateHash) unwrittenRecords {
	var records unwrittenRecords
	var roots []types.StateHash
	seen := make(map[types.StateHash]struct{}, len(ctxs))
	for _, ctx := range ctxs {
		if _, done := seen[ctx]; done {
			continue
		}
		seen[ctx] = struct{}{}
		if c, pending := b.contexts[ctx]; pending {
			records.contexts = append(records.contexts, ctx)
			roots = append(roots, c.stateRoot, c.memberRoot)
		}
	}
	records.pages = b.tree.unwritten(roots...)
	return records
}

// committed moves records a transaction committed out of the pending sets, into the cache.
func (b *Batch) committed(records unwrittenRecords) {
	b.tree.committed(records.pages)
	for _, ctx := range records.contexts {
		c := b.contexts[ctx]
		b.tree.cache.add(cacheKey{b.roomID, ctx}, c, len(c.encode()))
		delete(b.contexts, ctx)
	}
}
