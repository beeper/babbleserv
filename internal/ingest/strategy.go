package ingest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/id"

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
		return stateDAGStrategy{}, nil
	}
	return legacyStrategy{}, nil
}

// legacyStrategy runs jobs of rooms without state DAGs, room versions 1 to 12: each join on the
// remote join path, oldest first, and the PDUs as federated sends in chunks, those citing only stored
// events and each other apart from those with gaps to fill
type legacyStrategy struct{}

func (legacyStrategy) collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error) {
	return collectWorks(ctx, run, targets, legacyWorkKinds(run))
}

func legacyWorkKinds(run *jobRun) workKinds {
	return workKinds{
		join: func(target *types.IngestTarget) ingestWork {
			return &legacyJoin{joinWork{run: run, target: target}}
		},
		pdus: func(pdus []*types.IngestTarget, gap bool) ingestWork {
			return &legacyPDUs{run: run, pdus: pdus, gap: gap}
		},
	}
}

// workKinds makes a room version's units of work of a round's targets: a join, or a chunk of PDUs,
// connected or with gaps to fill
type workKinds struct {
	join func(*types.IngestTarget) ingestWork
	pdus func(pdus []*types.IngestTarget, gap bool) ingestWork
}

// collectWorks groups a round's targets into units of work, see roundWorks, splitting the PDUs into
// those connected and those with gaps while this server is in the room or no join is pending
func collectWorks(ctx context.Context, run *jobRun, targets []*types.IngestTarget, kinds workKinds) ([]ingestWork, error) {
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
	return roundWorks(kinds, joins, connected, gaps, joined), nil
}

// Most PDUs, and their bytes, a federated send of a job's PDUs takes, as /send does
const (
	maxPDUChunk      = 50
	maxPDUChunkBytes = 4 << 20
)

// roundWorks are the units of work publishing a round's targets. While this server is not in the
// room, the PDUs wait for a later round without joins: one whose join was replaced or found gone
// publishes nothing, and the PDUs would be dropped as a room's this server is not in. In a room it is
// in, a member's join is a whole send_join, so the PDUs go first but for those citing a pending join,
// whose gap fill would pull the join from another server: connected PDUs cite none with a gap, and
// the PDUs with gaps fill them before the joins.
func roundWorks(kinds workKinds, joins, connected, gaps []*types.IngestTarget, joined bool) []ingestWork {
	works := make([]ingestWork, 0, len(joins)+2)
	joinWorks := make([]ingestWork, len(joins))
	for i, target := range joins {
		joinWorks[i] = kinds.join(target)
	}
	if len(joins) > 0 && !joined {
		return joinWorks
	}
	for _, chunk := range chunkPDUs(connected, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, kinds.pdus(chunk, false))
	}
	beforeJoins, afterJoins := splitCitingJoins(gaps, joins)
	for _, chunk := range chunkPDUs(beforeJoins, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, kinds.pdus(chunk, true))
	}
	works = append(works, joinWorks...)
	for _, chunk := range chunkPDUs(afterJoins, maxPDUChunk, maxPDUChunkBytes) {
		works = append(works, kinds.pdus(chunk, true))
	}
	return works
}

// splitCitingJoins splits PDU targets in topological order into those citing neither a join target
// nor a PDU citing one, and the rest, keeping the order in both
func splitCitingJoins(pdus, joins []*types.IngestTarget) (independent, citing []*types.IngestTarget) {
	if len(joins) == 0 {
		return pdus, nil
	}
	citingIDs := make(map[id.EventID]struct{}, len(joins))
	for _, join := range joins {
		citingIDs[join.Event.ID] = struct{}{}
	}
	for _, target := range pdus {
		if citesAny(target.Event, citingIDs) {
			citingIDs[target.Event.ID] = struct{}{}
			citing = append(citing, target)
		} else {
			independent = append(independent, target)
		}
	}
	return independent, citing
}

// chunkPDUs splits PDU targets, in order, into chunks of at most maxCount targets and, past their
// first, maxBytes of events
func chunkPDUs(pdus []*types.IngestTarget, maxCount, maxBytes int) [][]*types.IngestTarget {
	var chunks [][]*types.IngestTarget
	size := 0
	for _, target := range pdus {
		if n := len(chunks); n == 0 || len(chunks[n-1]) == maxCount || size+target.Size > maxBytes {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], target)
		size += target.Size
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
