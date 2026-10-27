package ingest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var errUnsupportedRoomVersion = errors.New("this server cannot ingest events of the room version")

// strategy is how a room version's jobs publish their targets. A round collects the room's targets
// and the strategy groups them into units of work, run in order, each through its phases.
type strategy interface {
	collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error)
}

// ingestWork publishes some of a room's targets: a join, or PDUs. A phase ending the work early, its
// targets failed or replaced, returns errWorkDone.
type ingestWork interface {
	// Phase A: fetch from other servers what the targets need, verify it and stage it
	fetch(ctx context.Context) error
	// Phase B: give the staged events and the targets the state they need
	build(ctx context.Context) error
	// Phase C: publish the targets, each cleared by the transaction publishing it
	publish(ctx context.Context) error
	targets() []*types.IngestTarget
}

func strategyFor(roomVersion string) (strategy, error) {
	if _, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion)); err != nil {
		return nil, fmt.Errorf("%w: %q", errUnsupportedRoomVersion, roomVersion)
	} else if util.RoomVersionHas(roomVersion, gomatrixserverlib.IRoomVersion.StateDAGs) {
		return nil, fmt.Errorf("%w: %s, state DAG rooms are not joined yet", errUnsupportedRoomVersion, roomVersion)
	}
	return legacyStrategy{}, nil
}

// legacyStrategy runs jobs of rooms without state DAGs, room versions 1 to 12: each join on the
// remote join path, oldest first, then the PDUs as federated sends in chunks: first those citing only
// stored events and each other, then those with gaps to fill
type legacyStrategy struct{}

func (legacyStrategy) collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error) {
	joins, pdus := splitTargets(targets)
	joined := false
	if len(joins) > 0 && len(pdus) > 0 {
		var err error
		if joined, err = run.jobs.db.Rooms.IsServerJoined(ctx, run.roomID, run.jobs.config.ServerName); err != nil {
			return nil, err
		}
	}
	var connected, gaps []*types.IngestTarget
	if len(joins) == 0 || joined {
		var err error
		if connected, gaps, err = run.jobs.splitConnectedTargets(ctx, pdus); err != nil {
			return nil, err
		}
	}
	return legacyWorks(run, joins, connected, gaps, joined), nil
}

// Most PDUs, and their bytes, a federated send of a job's PDUs takes, as /send does
const (
	maxPDUChunk      = 50
	maxPDUChunkBytes = 4 << 20
)

// legacyWorks are the units of work publishing a round's targets. While this server is not in the
// room, the PDUs wait for a later round without joins: one whose join was replaced or found gone
// publishes nothing, and the PDUs would be dropped as a room's this server is not in. Connected PDUs
// cite none with a gap, so they go first, and a gap fill failing holds back only the PDUs it is for.
func legacyWorks(run *jobRun, joins, connected, gaps []*types.IngestTarget, joined bool) []ingestWork {
	works := make([]ingestWork, 0, len(joins)+2)
	for _, target := range joins {
		works = append(works, &legacyJoin{run: run, target: target})
	}
	if len(joins) > 0 && !joined {
		return works
	}
	for _, chunk := range chunkPDUs(connected, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, &legacyPDUs{run: run, pdus: chunk})
	}
	for _, chunk := range chunkPDUs(gaps, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, &legacyPDUs{run: run, pdus: chunk, gap: true})
	}
	return works
}

// chunkPDUs splits PDU targets, in order, into chunks of at most maxCount targets and, past their
// first, maxBytes of events
func chunkPDUs(pdus []*types.IngestTarget, maxCount, maxBytes int) [][]*types.IngestTarget {
	var chunks [][]*types.IngestTarget
	size := 0
	for _, target := range pdus {
		evSize := len(target.Event.ToMsgpack())
		if n := len(chunks); n == 0 || len(chunks[n-1]) == maxCount || size+evSize > maxBytes {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], target)
		size += evSize
	}
	return chunks
}

// splitTargets returns the joins and the PDUs among targets, each oldest first
func splitTargets(targets []*types.IngestTarget) (joins, pdus []*types.IngestTarget) {
	for _, target := range targets {
		if target.Kind == types.IngestTargetJoin {
			joins = append(joins, target)
		} else {
			pdus = append(pdus, target)
		}
	}
	byEnqueued := func(a, b *types.IngestTarget) int {
		return cmp.Or(cmp.Compare(a.Enqueued, b.Enqueued), cmp.Compare(a.Event.ID, b.Event.ID))
	}
	slices.SortFunc(joins, byEnqueued)
	slices.SortFunc(pdus, byEnqueued)
	return joins, pdus
}
