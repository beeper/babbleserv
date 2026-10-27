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

// strategy groups a round's targets into units of work, run in order, each through its phases
type strategy interface {
	collect(ctx context.Context, run *jobRun, targets []*types.IngestTarget) ([]ingestWork, error)
}

// workKind bounds how many units of work of the kind the process runs at once
type workKind int

const (
	workConnected workKind = iota
	workGapFill
	workJoin
)

func pduWorkKind(gap bool) workKind {
	if gap {
		return workGapFill
	}
	return workConnected
}

// A phase of an ingestWork ending it early, its targets failed or replaced, returns errWorkDone
type ingestWork interface {
	kind() workKind
	// Phase A: fetch, verify and stage what the targets need from other servers
	fetch(ctx context.Context) error
	// Phase B: give the staged events and the targets their state
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

// legacyStrategy runs jobs of rooms without state DAGs, room versions 1 to 12
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

type workKinds struct {
	join func(*types.IngestTarget) ingestWork
	pdus func(pdus []*types.IngestTarget, gap bool) ingestWork
}

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

// Per federated send of a job's PDUs, as /send takes
const (
	maxPDUChunk      = 50
	maxPDUChunkBytes = 4 << 20
)

// roundWorks runs only the joins while this server is not in the room: should none publish, the PDUs
// would be dropped. Otherwise the PDUs go first, but for those citing a pending join, whose gap fill
// would pull the join from another server.
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

// splitCitingJoins takes the PDUs in topological order, to catch those citing a join transitively
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

// chunkPDUs caps chunks at maxCount and, past their first target, maxBytes
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
