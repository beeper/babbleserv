package state

import (
	"context"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
)

// resolutionKey identifies the resolution of contexts whatever their order and repeats, as the
// result depends on neither.
func resolutionKey(ctxs []types.StateHash) types.StateHash {
	sorted := slices.Compact(slices.SortedFunc(slices.Values(ctxs), compareHashes))
	raw := make([]byte, 0, len(sorted)*len(types.StateHash{}))
	for _, ctx := range sorted {
		raw = append(raw, ctx[:]...)
	}
	return hashOf(raw)
}

// TxnResolved starts reading the result an artifact job stored for resolving the contexts, and
// returns a function waiting for it. A result is stored only once its context is, so it is readable
// wherever it is found. Only resolutions with conflicts are stored.
func (b *Batch) TxnResolved(txn fdb.ReadTransaction, ctxs []types.StateHash) func() (types.StateHash, bool, error) {
	wait := b.store.reader(txn, b.roomID).readResolution(resolutionKey(ctxs))
	return func() (types.StateHash, bool, error) {
		raw, err := wait()
		if err != nil || raw == nil {
			return types.StateHash{}, false, err
		}
		tup, err := tuple.Unpack(raw)
		if err != nil || len(tup) != 1 {
			return types.StateHash{}, false, fmt.Errorf("%w: invalid resolution result", ErrInvalidContext)
		}
		result, err := hashFromElement(tup[0])
		if err != nil || result.IsZero() {
			return types.StateHash{}, false, fmt.Errorf("%w: invalid resolution result", ErrInvalidContext)
		}
		return result, true, nil
	}
}

// StoreResolved stores the result of resolving contexts with conflicts in a write transaction of its
// own. A result pending in this batch must be staged first.
func (b *Batch) StoreResolved(ctx context.Context, ctxs []types.StateHash, result types.StateHash) error {
	if records := b.unwritten([]types.StateHash{result}); len(records.contexts) > 0 {
		return fmt.Errorf("resolution result %x is not staged", result)
	}
	return b.store.stage(ctx, b.roomID, func(w storeWriter) error {
		w.writeResolution(resolutionKey(ctxs), tuple.Tuple{result[:]}.Pack())
		return nil
	})
}
