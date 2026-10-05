// Package eventsendutil resolves room state using fixed or renewable transactions.
package eventsendutil

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
	"github.com/beeper/babbleserv/internal/stateres"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// contextSetKey identifies a resolution, which depends only on the distinct contexts it resolves.
func contextSetKey(contexts []types.StateHash) string {
	sorted := slices.Clone(contexts)
	slices.SortFunc(sorted, func(a, b types.StateHash) int {
		return bytes.Compare(a[:], b[:])
	})
	sorted = slices.Compact(sorted)
	var key strings.Builder
	for _, ctx := range sorted {
		key.Write(ctx[:])
	}
	return key.String()
}

type resolution struct {
	state types.StateHash
	err   error
}

// Resolver resolves the state after several events, either prev events or pending
// extremities, for one attempt of preparing a federated send. Batch events carry the contexts
// assigned so far in the attempt, stored events their own, so both are read from the event. An
// artifact job resolves with one of its own.
type Resolver struct {
	reads       resolutionReads
	stateBatch  *state.Batch
	roomID      id.RoomID
	roomVersion string
	// A resolution with more candidate events, or an input of more state tuples, is left to an
	// artifact job, unless zero
	maxCandidates, maxStateTuples int
	// Candidate events read per transaction, all of them in one when zero
	eventChunk int
	// State events of the inputs and candidate events of the last resolution run
	stateEvents, candidates int
	resolved                map[string]resolution
	// The state after prev events without state here, as the sending server gave it
	boundaries map[id.EventID]types.StateHash
}

// ResolverOptions sets the state batch and limits for a resolution. Zero limits are unbounded.
type ResolverOptions struct {
	StateBatch                    *state.Batch
	RoomID                        id.RoomID
	RoomVersion                   string
	MaxCandidates, MaxStateTuples int
}

func NewResolver(txn fdb.ReadTransaction, provider *events.TxnEventsProvider, graph *events.AuthGraph, options ResolverOptions) *Resolver {
	return newResolver(preparationReads{txn, provider, graph}, options)
}

func newResolver(reads resolutionReads, options ResolverOptions) *Resolver {
	return &Resolver{
		reads:          reads,
		stateBatch:     options.StateBatch,
		roomID:         options.RoomID,
		roomVersion:    options.RoomVersion,
		maxCandidates:  options.MaxCandidates,
		maxStateTuples: options.MaxStateTuples,
		resolved:       make(map[string]resolution),
	}
}

func (cr *Resolver) SetBoundaries(boundaries map[id.EventID]types.StateHash) {
	cr.boundaries = boundaries
}

func (cr *Resolver) Stats() (stateEvents, candidates int) {
	return cr.stateEvents, cr.candidates
}

// ResolutionRequest describes a resolution that exceeded an inline budget.
type ResolutionRequest struct {
	Contexts                []types.StateHash
	Candidates, StateTuples int
}

func (r *ResolutionRequest) Key() string { return contextSetKey(r.Contexts) }

func (r *ResolutionRequest) String() string {
	if r.StateTuples > 0 {
		return fmt.Sprintf("resolution of contexts %x of up to %d state tuples", r.Contexts, r.StateTuples)
	}
	return fmt.Sprintf("resolution of contexts %x with at least %d candidate events", r.Contexts, r.Candidates)
}

func (cr *Resolver) getEvent(eventID id.EventID) (ev *types.Event, err error) {
	err = cr.reads.Do(func() error {
		ev, err = cr.reads.Events().Get(eventID)
		return err
	})
	return ev, err
}

// afterState is zero for an event whose state is unknown here and was not given by the sending
// server. An event's own state is preferred over what the sending server gave.
func (cr *Resolver) afterState(eventID id.EventID) (types.StateHash, error) {
	ev, err := cr.getEvent(eventID)
	if err != nil {
		return types.StateHash{}, err
	} else if ev.HasStateIn(cr.roomID) {
		return ev.AfterState, nil
	}
	return cr.boundaries[eventID], nil
}

// PartitionPrevs splits prev events into those with a known state after them and the others.
func (cr *Resolver) PartitionPrevs(prevs []id.EventID) (known, unknown []id.EventID, err error) {
	for _, prevID := range prevs {
		if afterState, err := cr.afterState(prevID); err != nil {
			return nil, nil, err
		} else if afterState.IsZero() {
			unknown = append(unknown, prevID)
		} else {
			known = append(known, prevID)
		}
	}
	return known, unknown, nil
}

// StateBefore is the state before an event with the given prev events, each of which must have a
// known state after it.
func (cr *Resolver) StateBefore(prevs []id.EventID) (types.StateHash, *ResolutionRequest, error) {
	switch len(prevs) {
	case 0:
		return state.EmptyContext, nil, nil
	case 1:
		after, err := cr.afterState(prevs[0])
		return after, nil, err
	default:
		return cr.Resolve(prevs)
	}
}

func (cr *Resolver) Resolve(eventIDs []id.EventID) (types.StateHash, *ResolutionRequest, error) {
	contexts, err := cr.afterStates(eventIDs)
	if err != nil {
		return types.StateHash{}, nil, err
	}
	if len(contexts) == 1 {
		return contexts[0], nil, nil
	}

	key := contextSetKey(contexts)
	res, found := cr.resolved[key]
	if !found {
		var request *ResolutionRequest
		res.state, request, res.err = cr.resolveContexts(contexts)
		if request != nil {
			return types.StateHash{}, request, nil
		}
		cr.resolved[key] = res
	}
	if res.err != nil {
		return types.StateHash{}, nil, fmt.Errorf("failed to resolve state after %v: %w", eventIDs, res.err)
	}
	return res.state, nil, nil
}

// afterStates returns the distinct after-contexts of the events.
func (cr *Resolver) afterStates(eventIDs []id.EventID) ([]types.StateHash, error) {
	cr.reads.Events().WillGet(eventIDs...)
	contexts := make([]types.StateHash, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		afterState, err := cr.afterState(eventID)
		if err != nil {
			return nil, err
		} else if afterState.IsZero() {
			return nil, fmt.Errorf("no state after %s", eventID)
		}
		if !slices.Contains(contexts, afterState) {
			contexts = append(contexts, afterState)
		}
	}
	return contexts, nil
}

// resolveContexts takes the result an artifact job stored for resolving the contexts, or resolves
// them here, see ResolveContexts. Distinct contexts always conflict, so a room version before state
// resolution v2 fails before reading anything.
func (cr *Resolver) resolveContexts(contexts []types.StateHash) (types.StateHash, *ResolutionRequest, error) {
	if err := stateres.CheckAlgorithm(cr.roomVersion); err != nil {
		return types.StateHash{}, nil, err
	}
	if result, found, err := cr.stateBatch.TxnResolved(cr.reads.txn(), contexts)(); err != nil {
		return types.StateHash{}, nil, err
	} else if found {
		return result, nil, nil
	}
	return cr.ResolveContexts(contexts)
}

// ResolveContexts resolves the contexts from their conflicts and, when they have any, the auth
// difference of their states, see authDifference.
// It computes directly; the caller stages and stores the result.
func (cr *Resolver) ResolveContexts(contexts []types.StateHash) (types.StateHash, *ResolutionRequest, error) {
	conflicts, err := cr.stateBatch.TxnConflicts(cr.reads.txn(), contexts)
	if err != nil || len(conflicts) == 0 {
		return contexts[0], nil, err
	}
	difference, request, err := cr.authDifference(contexts)
	if err != nil || request != nil {
		return types.StateHash{}, request, err
	}
	return cr.resolveConflicts(contexts, conflicts, difference)
}

// authDifference returns the events in the auth chains of some of the contexts' states but not all,
// computed from the chain cover over every state event of the contexts, see
// events.AuthGraph.AuthChainDifference. A resolution whose largest input holds more state tuples than
// the resolver reads inline requests a resolution job, before any is read but the root pages of
// the inputs' maps.
func (cr *Resolver) authDifference(contexts []types.StateHash) ([]id.EventID, *ResolutionRequest, error) {
	if request, err := cr.checkStateTuples(contexts, func(ctx types.StateHash) (int, error) {
		return cr.stateBatch.TxnCount(cr.reads.txn(), ctx)
	}); err != nil || request != nil {
		return nil, request, err
	}
	states, err := cr.stateBatch.TxnIterateAll(cr.reads.txn(), contexts...)
	if err != nil {
		return nil, nil, err
	}
	stateSets := make([][]id.EventID, len(states))
	cr.stateEvents = 0
	for i, entries := range states {
		stateSets[i] = make([]id.EventID, 0, len(entries))
		for _, entry := range entries {
			stateSets[i] = append(stateSets[i], entry.EventID)
		}
		cr.stateEvents += len(entries)
	}
	difference, err := cr.reads.graph().AuthChainDifference(stateSets)
	return difference, nil, err
}

// checkStateTuples requests a job when an input holds more tuples than the resolver reads inline.
func (cr *Resolver) checkStateTuples(contexts []types.StateHash, count func(types.StateHash) (int, error)) (*ResolutionRequest, error) {
	if cr.maxStateTuples == 0 {
		return nil, nil
	}
	for _, ctx := range contexts {
		if tuples, err := count(ctx); err != nil {
			return nil, err
		} else if tuples > cr.maxStateTuples {
			return &ResolutionRequest{Contexts: contexts, StateTuples: tuples}, nil
		}
	}
	return nil, nil
}

// resolveConflicts resolves the contexts from what differs between them: their conflicts, their auth
// difference and in room version 12 the conflicted subgraph, reading the state they share from the
// first context on demand. A resolution exceeding the candidate budget requests a job as soon as
// the candidates are known, before reading any.
func (cr *Resolver) resolveConflicts(
	contexts []types.StateHash,
	conflicts map[types.StateTup][]id.EventID,
	difference []id.EventID,
) (types.StateHash, *ResolutionRequest, error) {
	if len(conflicts) == 0 {
		return contexts[0], nil, nil
	}
	var conflictedIDs []id.EventID
	for _, ids := range conflicts {
		for _, eventID := range ids {
			if eventID != "" {
				conflictedIDs = append(conflictedIDs, eventID)
			}
		}
	}
	candidates, request := cr.limitCandidates(contexts, conflictedIDs, difference)
	if request != nil {
		return types.StateHash{}, request, nil
	}
	var err error
	stateResV2_1 := util.RoomVersionHas(cr.roomVersion, func(impl gomatrixserverlib.IRoomVersion) bool {
		return impl.StateResAlgorithm() == gomatrixserverlib.StateResV2_1
	})
	var subgraph []id.EventID
	if stateResV2_1 {
		if subgraph, err = cr.reads.graph().ConflictedSubgraph(conflictedIDs); err != nil {
			return types.StateHash{}, nil, err
		}
		if candidates, request = cr.limitCandidates(contexts, candidates, subgraph); request != nil {
			return types.StateHash{}, request, nil
		}
	}
	cr.candidates = len(candidates)
	keys, authKeys, err := cr.readCandidates(candidates)
	if err != nil {
		return types.StateHash{}, nil, err
	}
	// Prefetched together, so the resolution rarely looks up a tuple of its own
	baseEntries, err := cr.stateBatch.TxnLookupEntries(cr.reads.txn(), contexts[0], slices.Collect(maps.Keys(keys)))
	if err != nil {
		return types.StateHash{}, nil, err
	}
	base := baseEntries.EventIDs()
	var authBase []id.EventID
	for tup := range keys {
		// Cache negative lookups too.
		if _, ok := base[tup]; !ok {
			base[tup] = ""
		}
		if _, conflicted := conflicts[tup]; !conflicted && authKeys[tup] && base[tup] != "" && !stateResV2_1 {
			authBase = append(authBase, base[tup])
		}
	}
	if err := cr.prefetchEvents(authBase); err != nil {
		return types.StateHash{}, nil, err
	}
	delta, err := stateres.ResolveSparse(cr.roomVersion, stateres.SparseInput{
		Conflicts: conflicts, AuthDifference: difference, ConflictedSubgraph: subgraph, Base: base,
		Lookup: func(keys []types.StateTup) (types.StateMap, error) {
			entries, err := cr.stateBatch.TxnLookupEntries(cr.reads.txn(), contexts[0], keys)
			return entries.EventIDs(), err
		},
	}, cr.getEvent)
	if err != nil {
		return types.StateHash{}, nil, err
	}
	entries, err := cr.withMemberships(delta)
	if err != nil {
		return types.StateHash{}, nil, err
	}
	result, err := cr.stateBatch.TxnApply(cr.reads.txn(), contexts[0], entries)
	return result, nil, err
}

// withMemberships gives each member a resolution sets the membership of its event, which the
// resolution has read already.
func (cr *Resolver) withMemberships(delta types.StateMap) (types.StateEntries, error) {
	entries := make(types.StateEntries, len(delta))
	for tup, eventID := range delta {
		if eventID == "" || tup.Type != event.StateMember {
			entries[tup] = types.StateEntry{EventID: eventID}
			continue
		}
		ev, err := cr.getEvent(eventID)
		if err != nil {
			return nil, err
		} else if ev == nil {
			return nil, fmt.Errorf("%w: resolved member event %s", types.ErrEventNotFound, eventID)
		}
		entries[tup] = ev.StateEntry()
	}
	return entries, nil
}

// limitCandidates returns the distinct candidates, or requests a job when they exceed the inline limit.
func (cr *Resolver) limitCandidates(contexts []types.StateHash, lists ...[]id.EventID) ([]id.EventID, *ResolutionRequest) {
	candidates := slices.Concat(lists...)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	if cr.maxCandidates > 0 && len(candidates) > cr.maxCandidates {
		return nil, &ResolutionRequest{Contexts: contexts, Candidates: len(candidates)}
	}
	return candidates, nil
}

// prefetchEvents starts reading events a resolution reads later. A job reads them now, eventChunk at
// a time, so that no transaction holds more reads than that and the events carry over.
func (cr *Resolver) prefetchEvents(eventIDs []id.EventID) error {
	if cr.eventChunk == 0 {
		cr.reads.Events().WillGet(eventIDs...)
		return nil
	}
	for part := range slices.Chunk(eventIDs, cr.eventChunk) {
		if err := cr.reads.Do(func() error {
			eventsProvider := cr.reads.Events()
			eventsProvider.WillGet(part...)
			for _, eventID := range part {
				if _, err := eventsProvider.Get(eventID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// readCandidates reads the candidate events of a resolution, eventChunk at a time, then prefetches
// their auth events. Returns the tuples the candidates are authorized against or replace, and those
// they are authorized against.
func (cr *Resolver) readCandidates(candidates []id.EventID) (keys map[types.StateTup]struct{}, authKeys map[types.StateTup]bool, err error) {
	keys = make(map[types.StateTup]struct{})
	authKeys = make(map[types.StateTup]bool)
	authEvents := make(map[id.EventID]struct{})
	chunk := cr.eventChunk
	if chunk == 0 {
		chunk = max(len(candidates), 1)
	}
	for part := range slices.Chunk(candidates, chunk) {
		if err := cr.reads.Do(func() error {
			eventsProvider := cr.reads.Events()
			eventsProvider.WillGet(part...)
			for _, eventID := range part {
				ev, err := eventsProvider.Get(eventID)
				if err != nil {
					return err
				}
				if ev == nil {
					continue
				}
				for _, authID := range ev.AuthEventIDs {
					authEvents[authID] = struct{}{}
				}
				for _, tup := range events.AuthStateTupsForEvent(ev) {
					authKeys[tup] = true
					keys[tup] = struct{}{}
				}
				if ev.StateKey != nil {
					keys[ev.StateTup()] = struct{}{}
				}
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	return keys, authKeys, cr.prefetchEvents(slices.Collect(maps.Keys(authEvents)))
}
