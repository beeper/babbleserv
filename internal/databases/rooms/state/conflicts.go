package state

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type multiChange struct {
	key    []byte
	values [][]byte
}

// diffAll compares every root of each group to the group's first root, all groups sharing one
// batched read per level. Each key differing within a group has a value, nil when absent, for every
// root of the group, in no particular key order.
func (t *tree) diffAll(r pageReader, groups ...[]types.StateHash) ([][]multiChange, error) {
	var pairs []rootPair
	for _, roots := range groups {
		for i := 1; i < len(roots); i++ {
			pairs = append(pairs, rootPair{roots[0], roots[i]})
		}
	}
	diffs, err := t.diff(r, pairs...)
	if err != nil {
		return nil, err
	}

	out := make([][]multiChange, len(groups))
	pair := 0
	for g, roots := range groups {
		byKey := make(map[string]int)
		for i := 1; i < len(roots); i++ {
			for _, change := range diffs[pair] {
				j, ok := byKey[string(change.key)]
				if !ok {
					j = len(out[g])
					byKey[string(change.key)] = j
					values := make([][]byte, len(roots))
					for k := range values {
						values[k] = change.old
					}
					out[g] = append(out[g], multiChange{key: change.key, values: values})
				}
				out[g][j].values[i] = change.new
			}
			pair++
		}
	}
	return out, nil
}

// diffContexts returns each tuple whose event differs between the contexts, with its entry in every
// context in the order of ctxs and the empty entry where it is absent. All the comparisons share one
// batched read per level and identical subtrees are never enumerated. A member's value is a function
// of its event ID, so values differ exactly where event IDs do.
func (b *Batch) diffContexts(r storeReader, ctxs []types.StateHash) (map[types.StateTup][]types.StateEntry, error) {
	resolved, err := b.resolve(r, ctxs...)
	if err != nil {
		return nil, err
	}
	groups := make([][]types.StateHash, contextMaps)
	for _, c := range resolved {
		for m, root := range c.roots() {
			groups[m] = append(groups[m], root)
		}
	}
	diffs, err := b.tree.diffAll(r, groups...)
	if err != nil {
		return nil, err
	}

	out := make(map[types.StateTup][]types.StateEntry)
	for m, changes := range diffs {
		for _, change := range changes {
			tup, err := keyTuple(m, change.key)
			if err != nil {
				return nil, err
			}
			entries := make([]types.StateEntry, len(change.values))
			for i, value := range change.values {
				if entries[i], err = decodeValue(m, value); err != nil {
					return nil, err
				}
			}
			out[tup] = entries
		}
	}
	return out, nil
}

// TxnConflicts returns the tuples whose event differs between the contexts, each with one event ID
// per context in the order of ctxs and "" where the tuple is absent from that context.
func (b *Batch) TxnConflicts(txn fdb.ReadTransaction, ctxs []types.StateHash) (map[types.StateTup][]id.EventID, error) {
	diff, err := b.diffContexts(b.store.reader(txn, b.roomID), ctxs)
	if err != nil {
		return nil, err
	}
	conflicts := make(map[types.StateTup][]id.EventID, len(diff))
	for tup, entries := range diff {
		eventIDs := make([]id.EventID, len(entries))
		for i, entry := range entries {
			eventIDs[i] = entry.EventID
		}
		conflicts[tup] = eventIDs
	}
	return conflicts, nil
}
