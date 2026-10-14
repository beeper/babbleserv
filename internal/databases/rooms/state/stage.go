package state

import (
	"context"
	"fmt"
	"math"

	"github.com/beeper/babbleserv/internal/types"
)

// Stage writes the given contexts that are pending in this batch and every pending page they reach,
// ahead of the transaction using them, counting key and value bytes against the budget, a record
// larger than the budget alone. Pages go first, each after its children, then context rows, so a
// staged record only references stored ones. A committed transaction's records move from the
// pending sets into the cache.
func (b *Batch) Stage(ctx context.Context, budget int, ctxs ...types.StateHash) error {
	for _, chunk := range b.planStaging(budget, ctxs).chunks {
		if err := b.store.stage(ctx, b.roomID, func(w storeWriter) error {
			for _, h := range chunk.pages {
				w.writePage(h, b.tree.pending[h].raw)
			}
			for _, h := range chunk.contexts {
				w.writeContext(h, b.contexts[h].encode())
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to stage room state: %w", err)
		}
		b.committed(chunk.unwrittenRecords)
	}
	return nil
}

// PendingBytes counts key and value bytes for pending contexts and every pending page they reach.
func (b *Batch) PendingBytes(ctxs ...types.StateHash) int {
	total := 0
	for _, chunk := range b.planStaging(math.MaxInt, ctxs).chunks {
		total += chunk.bytes
	}
	return total
}

func (b *Batch) planStaging(budget int, ctxs []types.StateHash) *stagingPlan {
	records := b.unwritten(ctxs)
	keySize := b.store.keySize(b.roomID)
	plan := &stagingPlan{budget: budget}
	for _, h := range records.pages {
		chunk := plan.add(keySize + len(b.tree.pending[h].raw))
		chunk.pages = append(chunk.pages, h)
	}
	for _, h := range records.contexts {
		chunk := plan.add(keySize + len(b.contexts[h].encode()))
		chunk.contexts = append(chunk.contexts, h)
	}
	return plan
}

type stagingChunk struct {
	unwrittenRecords
	bytes int
}

type stagingPlan struct {
	budget int
	chunks []*stagingChunk
}

// add returns the chunk a record of this many bytes goes in: the last one while it stays within the
// budget, otherwise a new one.
func (p *stagingPlan) add(bytes int) *stagingChunk {
	if n := len(p.chunks); n == 0 || p.chunks[n-1].bytes+bytes > p.budget {
		p.chunks = append(p.chunks, &stagingChunk{})
	}
	chunk := p.chunks[len(p.chunks)-1]
	chunk.bytes += bytes
	return chunk
}
