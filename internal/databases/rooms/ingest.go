package rooms

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// ErrRoomIngesting refuses a send that would publish past the room's ingest job
var ErrRoomIngesting = errors.New("the room has an ingest job")

// ErrIngestTargetGone means another runner failed or replaced the join's target meanwhile
var ErrIngestTargetGone = errors.New("the join's ingest target is gone")

const ingestTargetsPage = 500

func (r *RoomsDatabase) keyForIngestJob(roomID id.RoomID) fdb.Key {
	return r.ingestJobs.Pack(tuple.Tuple{roomID.String()})
}

func (r *RoomsDatabase) keyForIngestTarget(roomID id.RoomID, eventID id.EventID) fdb.Key {
	return r.ingestTargets.Pack(tuple.Tuple{roomID.String(), eventID.String()})
}

func (r *RoomsDatabase) keyForIngestJoin(roomID id.RoomID, userID id.UserID) fdb.Key {
	return r.ingestJoins.Pack(tuple.Tuple{roomID.String(), userID.String()})
}

const (
	ingestQueuePDUs  = "pdus"
	ingestQueueBytes = "bytes"
)

func (r *RoomsDatabase) keyForIngestQueue(roomID id.RoomID, counter string) fdb.Key {
	return r.ingestQueues.Pack(tuple.Tuple{roomID.String(), counter})
}

func (r *RoomsDatabase) txnGetIngestJob(txn fdb.ReadTransaction, roomID id.RoomID) (*types.IngestJob, error) {
	b, err := txn.Get(r.keyForIngestJob(roomID)).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return types.NewIngestJobFromBytes(b, roomID)
}

func (r *RoomsDatabase) txnGetIngestTarget(txn fdb.ReadTransaction, roomID id.RoomID, eventID id.EventID) (*types.IngestTarget, error) {
	b, err := txn.Get(r.keyForIngestTarget(roomID, eventID)).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return types.NewIngestTargetFromBytes(b, eventID)
}

func (r *RoomsDatabase) txnGetIngestJoin(txn fdb.ReadTransaction, roomID id.RoomID, userID id.UserID) (*types.IngestJoin, error) {
	b, err := txn.Get(r.keyForIngestJoin(roomID, userID)).Get()
	if err != nil || b == nil {
		return nil, err
	}
	return types.NewIngestJoinFromBytes(b)
}

// Every target write calls this, so no target is left without a job to publish it
func (r *RoomsDatabase) txnEnsureIngestJob(txn fdb.Transaction, roomID id.RoomID, roomVersion string) error {
	job, err := r.txnGetIngestJob(txn, roomID)
	if err != nil || job != nil {
		return err
	}
	txn.Set(r.keyForIngestJob(roomID), (&types.IngestJob{RoomVersion: roomVersion}).ToMsgpack())
	return nil
}

func (r *RoomsDatabase) txnSetIngestTarget(txn fdb.Transaction, target *types.IngestTarget) {
	txn.Set(r.keyForIngestTarget(target.Event.RoomID, target.Event.ID), target.ToMsgpack())
}

// Atomic adds, so target writes do not conflict over the counters
func (r *RoomsDatabase) txnAddQueued(txn fdb.Transaction, roomID id.RoomID, delta types.IngestQueue) {
	if delta.PDUs != 0 {
		txn.Add(r.keyForIngestQueue(roomID, ingestQueuePDUs), counterDelta(delta.PDUs))
	}
	if delta.Bytes != 0 {
		txn.Add(r.keyForIngestQueue(roomID, ingestQueueBytes), counterDelta(delta.Bytes))
	}
}

func queuedBy(target *types.IngestTarget) types.IngestQueue {
	if target.Kind != types.IngestTargetPDU {
		return types.IngestQueue{}
	}
	return types.IngestQueue{PDUs: 1, Bytes: int64(target.Size)}
}

// FoundationDB adds little-endian two's complement integers
func counterDelta(delta int64) []byte {
	return binary.LittleEndian.AppendUint64(nil, uint64(delta))
}

func counterValue(b []byte) int64 {
	if len(b) != 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

func (r *RoomsDatabase) GetIngestQueue(ctx context.Context, roomID id.RoomID) (types.IngestQueue, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.IngestQueue, error) {
		pdus := txn.Get(r.keyForIngestQueue(roomID, ingestQueuePDUs))
		bytes := txn.Get(r.keyForIngestQueue(roomID, ingestQueueBytes))
		pdusB, err := pdus.Get()
		if err != nil {
			return types.IngestQueue{}, err
		}
		bytesB, err := bytes.Get()
		if err != nil {
			return types.IngestQueue{}, err
		}
		return types.IngestQueue{PDUs: counterValue(pdusB), Bytes: counterValue(bytesB)}, nil
	})
}

func joinUser(target *types.IngestTarget) id.UserID {
	return id.UserID(*target.Event.StateKey)
}

// EnqueueIngestJoin returns the user's already pending join to wait on instead, if there is one
func (r *RoomsDatabase) EnqueueIngestJoin(ctx context.Context, joinEv *types.Event, servers []string) (id.EventID, error) {
	if joinEv.Type != event.StateMember || joinEv.StateKey == nil || joinEv.Membership() != event.MembershipJoin {
		return "", fmt.Errorf("event %s is not a join", joinEv.ID)
	}
	userID := id.UserID(*joinEv.StateKey)
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (id.EventID, error) {
		if pending, err := r.txnPendingIngestJoin(txn, joinEv.RoomID, userID); err != nil || pending != "" {
			return pending, err
		} else if err := r.txnEnsureIngestJob(txn, joinEv.RoomID, joinEv.RoomVersion); err != nil {
			return "", err
		}
		r.txnSetIngestTarget(txn, &types.IngestTarget{
			Kind:     types.IngestTargetJoin,
			Event:    joinEv,
			Servers:  servers,
			Enqueued: time.Now().UnixMilli(),
		})
		txn.Set(r.keyForIngestJoin(joinEv.RoomID, userID), (&types.IngestJoin{EventID: joinEv.ID}).ToMsgpack())
		return joinEv.ID, nil
	})
}

func (r *RoomsDatabase) txnPendingIngestJoin(txn fdb.ReadTransaction, roomID id.RoomID, userID id.UserID) (id.EventID, error) {
	join, err := r.txnGetIngestJoin(txn, roomID, userID)
	if err != nil || join == nil || join.Failure != nil {
		return "", err
	} else if target, err := r.txnGetIngestTarget(txn, roomID, join.EventID); err != nil || target == nil {
		// Publishing or failing a target updates iju in the same transaction, so this is never reached
		return "", err
	}
	return join.EventID, nil
}

func (r *RoomsDatabase) GetPendingIngestJoin(ctx context.Context, roomID id.RoomID, userID id.UserID) (id.EventID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (id.EventID, error) {
		return r.txnPendingIngestJoin(txn, roomID, userID)
	})
}

func (r *RoomsDatabase) GetIngestJoin(ctx context.Context, roomID id.RoomID, userID id.UserID) (*types.IngestJoin, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.IngestJoin, error) {
		return r.txnGetIngestJoin(txn, roomID, userID)
	})
}

// WatchIngestJoin returns the user's join, nil once published, with a watch firing when that changes
func (r *RoomsDatabase) WatchIngestJoin(ctx context.Context, roomID id.RoomID, userID id.UserID) (*types.IngestJoin, fdb.FutureNil, error) {
	type watched struct {
		join  *types.IngestJoin
		watch fdb.FutureNil
	}
	res, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (watched, error) {
		join, err := r.txnGetIngestJoin(txn, roomID, userID)
		if err != nil {
			return watched{}, err
		}
		return watched{join, txn.Watch(r.keyForIngestJoin(roomID, userID))}, nil
	})
	return res.join, res.watch, err
}

func (r *RoomsDatabase) EnqueueIngestPDUs(ctx context.Context, roomID id.RoomID, origin string, evs []*types.Event) error {
	now := time.Now().UnixMilli()
	targets := make([]*types.IngestTarget, len(evs))
	for i, ev := range evs {
		targets[i] = &types.IngestTarget{
			Kind:     types.IngestTargetPDU,
			Event:    ev,
			Servers:  []string{origin},
			Enqueued: now,
			Size:     len(ev.ToMsgpack()),
		}
	}
	return r.enqueueIngestPDUs(ctx, roomID, targets)
}

func (r *RoomsDatabase) EnqueuePulledIngestPDUs(ctx context.Context, roomID id.RoomID, targets []*types.IngestTarget) error {
	for _, target := range targets {
		if target.Kind != types.IngestTargetPDU || !target.Pulled {
			return fmt.Errorf("target %s is not a pulled PDU", target.Event.ID)
		}
	}
	return r.enqueueIngestPDUs(ctx, roomID, targets)
}

// Per transaction, well within FoundationDB's 10 MB transaction limit
const maxEnqueueBytes = 4 << 20

func (r *RoomsDatabase) enqueueIngestPDUs(ctx context.Context, roomID id.RoomID, targets []*types.IngestTarget) error {
	for _, chunk := range chunkIngestTargets(targets, maxEnqueueBytes) {
		if err := r.enqueueIngestPDUChunk(ctx, roomID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// A chunk's first target may alone exceed maxBytes
func chunkIngestTargets(targets []*types.IngestTarget, maxBytes int) [][]*types.IngestTarget {
	var chunks [][]*types.IngestTarget
	size := 0
	for _, target := range targets {
		if n := len(chunks); n == 0 || size+target.Size > maxBytes {
			chunks = append(chunks, nil)
			size = 0
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], target)
		size += target.Size
	}
	return chunks
}

// In a state DAG room a stored event is skipped only once published with state: the federated
// pipeline publishes one replayed but unpublished, or staged without state, as a new event.
func (r *RoomsDatabase) enqueueIngestPDUChunk(ctx context.Context, roomID id.RoomID, targets []*types.IngestTarget) error {
	stateDAGs := targets[0].Event.StateDAGs()
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
		if err := r.txnEnsureIngestJob(txn, roomID, targets[0].Event.RoomVersion); err != nil {
			return nil, err
		}
		versions := make([]fdb.FutureByteSlice, len(targets))
		existing := make([]fdb.FutureByteSlice, len(targets))
		for i, target := range targets {
			versions[i] = txn.Get(r.events.KeyForIDToVersion(target.Event.ID))
			existing[i] = txn.Get(r.keyForIngestTarget(roomID, target.Event.ID))
		}
		storedEvents := r.events.NewTxnEventsProvider(ctx, txn)
		if stateDAGs {
			for i, target := range targets {
				if versions[i].MustGet() != nil {
					storedEvents.WillGet(target.Event.ID)
				}
			}
		}
		written := make(map[id.EventID]struct{}, len(targets))
		var queued types.IngestQueue
		for i, target := range targets {
			if _, found := written[target.Event.ID]; found || target.Event.RoomID != roomID || existing[i].MustGet() != nil {
				continue
			} else if versions[i].MustGet() != nil {
				if !stateDAGs {
					continue
				} else if storedEv, err := storedEvents.Get(target.Event.ID); err != nil {
					return nil, err
				} else if storedEv.HasStateIn(roomID) && !storedEv.Unpublished {
					continue
				}
			}
			written[target.Event.ID] = struct{}{}
			r.txnSetIngestTarget(txn, target)
			queued = queued.Add(queuedBy(target))
		}
		r.txnAddQueued(txn, roomID, queued)
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) GetIngestJob(ctx context.Context, roomID id.RoomID) (*types.IngestJob, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.IngestJob, error) {
		return r.txnGetIngestJob(txn, roomID)
	})
}

func (r *RoomsDatabase) IngestJobRoomIDs(ctx context.Context, after id.RoomID, limit int) ([]id.RoomID, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]id.RoomID, error) {
		begin, end := r.ingestJobs.FDBRangeKeys()
		if after != "" {
			begin = fdb.Key(append(r.keyForIngestJob(after), 0))
		}
		kvs, err := txn.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: limit}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		roomIDs := make([]id.RoomID, 0, len(kvs))
		for _, kv := range kvs {
			tup, err := r.ingestJobs.Unpack(kv.Key)
			if err != nil {
				return nil, err
			}
			roomIDs = append(roomIDs, id.RoomID(tup[0].(string)))
		}
		return roomIDs, nil
	})
}

// FoundationDB's transaction_too_old: a read version past the five seconds it keeps
const fdbTransactionTooOld = 1007

const ingestTargetsAttemptsAtOneVersion = 3

// GetIngestTargets reads the targets over several transactions at the first one's read version, so
// targets written together, as a /send's PDUs, are seen all or none; a round seeing only some would
// fill the gaps of those citing the others. Reads outlasting that version start again, a few times,
// before each page is read at its own version and the targets citing one not read are left out.
func (r *RoomsDatabase) GetIngestTargets(ctx context.Context, roomID id.RoomID) (*types.IngestJob, []*types.IngestTarget, error) {
	job, err := r.GetIngestJob(ctx, roomID)
	if err != nil || job == nil {
		return nil, nil, err
	}
	for range ingestTargetsAttemptsAtOneVersion {
		targets, err := r.readIngestTargets(ctx, roomID, true)
		var fdbErr fdb.Error
		if !errors.As(err, &fdbErr) || fdbErr.Code != fdbTransactionTooOld {
			return job, targets, err
		}
	}
	zerolog.Ctx(ctx).Warn().Msg("Ingest targets take longer to read than a read version lasts, reading them a page at a time")
	targets, err := r.readIngestTargets(ctx, roomID, false)
	if err != nil {
		return nil, nil, err
	}
	unread, err := r.unreadIngestTargets(ctx, roomID, targets)
	if err != nil {
		return nil, nil, err
	}
	return job, withoutCitersOf(targets, unread), nil
}

func (r *RoomsDatabase) readIngestTargets(ctx context.Context, roomID id.RoomID, oneVersion bool) ([]*types.IngestTarget, error) {
	var version int64
	var targets []*types.IngestTarget
	begin, end := r.ingestTargets.Sub(roomID.String()).FDBRangeKeys()
	for {
		if !oneVersion {
			version = 0
		}
		page, err := r.readIngestTargetsPage(ctx, &version, fdb.KeyRange{Begin: begin, End: end})
		if err != nil {
			return nil, err
		}
		targets = append(targets, page...)
		if len(page) < ingestTargetsPage {
			return targets, nil
		}
		last := page[len(page)-1].Event
		begin = fdb.Key(append(r.keyForIngestTarget(roomID, last.ID), 0))
	}
}

// Pages after the first retry at the first's version, but for transaction_too_old, which would recur
func (r *RoomsDatabase) readIngestTargetsPage(ctx context.Context, version *int64, keys fdb.KeyRange) ([]*types.IngestTarget, error) {
	if *version == 0 {
		return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.IngestTarget, error) {
			var err error
			if *version, err = txn.GetReadVersion().Get(); err != nil {
				return nil, err
			}
			return r.txnIngestTargetsPage(txn, keys)
		})
	}
	txn, err := r.db.CreateTransaction()
	if err != nil {
		return nil, err
	}
	defer txn.Cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		txn.SetReadVersion(*version)
		page, err := r.txnIngestTargetsPage(txn.Snapshot(), keys)
		var fdbErr fdb.Error
		if !errors.As(err, &fdbErr) || fdbErr.Code == fdbTransactionTooOld {
			return page, err
		} else if err := txn.OnError(fdbErr).Get(); err != nil {
			return nil, err
		}
	}
}

// unreadIngestTargets returns the targets those read cite that were not read with them
func (r *RoomsDatabase) unreadIngestTargets(ctx context.Context, roomID id.RoomID, targets []*types.IngestTarget) (map[id.EventID]struct{}, error) {
	read := make(map[id.EventID]struct{}, len(targets))
	for _, target := range targets {
		read[target.Event.ID] = struct{}{}
	}
	var cited []id.EventID
	for _, target := range targets {
		for _, eventID := range slices.Concat(target.Event.PrevEventIDs, target.Event.AuthDependencyIDs()) {
			if _, found := read[eventID]; !found {
				cited = append(cited, eventID)
			}
		}
	}
	slices.Sort(cited)
	unread := make(map[id.EventID]struct{})
	for chunk := range slices.Chunk(slices.Compact(cited), ingestTargetsPage) {
		if _, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
			futures := make([]fdb.FutureByteSlice, len(chunk))
			for i, eventID := range chunk {
				futures[i] = txn.Get(r.keyForIngestTarget(roomID, eventID))
			}
			for i, future := range futures {
				if b, err := future.Get(); err != nil {
					return nil, err
				} else if b != nil {
					unread[chunk[i]] = struct{}{}
				}
			}
			return nil, nil
		}); err != nil {
			return nil, err
		}
	}
	return unread, nil
}

// withoutCitersOf leaves out the targets citing an unread one, directly or through those left out
func withoutCitersOf(targets []*types.IngestTarget, unread map[id.EventID]struct{}) []*types.IngestTarget {
	if len(unread) == 0 {
		return targets
	}
	out := maps.Clone(unread)
	evs := make([]*types.Event, len(targets))
	for i, target := range targets {
		evs[i] = target.Event
	}
	for _, ev := range SortEventsTopologically(evs) {
		if slices.ContainsFunc(slices.Concat(ev.PrevEventIDs, ev.AuthDependencyIDs()), func(eventID id.EventID) bool {
			_, found := out[eventID]
			return found
		}) {
			out[ev.ID] = struct{}{}
		}
	}
	return slices.DeleteFunc(slices.Clone(targets), func(target *types.IngestTarget) bool {
		_, found := out[target.Event.ID]
		return found
	})
}

func (r *RoomsDatabase) txnIngestTargetsPage(txn fdb.ReadTransaction, keys fdb.KeyRange) ([]*types.IngestTarget, error) {
	kvs, err := txn.GetRange(keys, fdb.RangeOptions{Limit: ingestTargetsPage}).GetSliceWithError()
	if err != nil {
		return nil, err
	}
	page := make([]*types.IngestTarget, 0, len(kvs))
	for _, kv := range kvs {
		tup, err := r.ingestTargets.Unpack(kv.Key)
		if err != nil {
			return nil, err
		}
		target, err := types.NewIngestTargetFromBytes(kv.Value, id.EventID(tup[1].(string)))
		if err != nil {
			return nil, fmt.Errorf("invalid ingest target %s: %w", tup[1], err)
		}
		page = append(page, target)
	}
	return page, nil
}

// UpdateIngestTarget returns the target as it was before the update, or nil if it is gone
func (r *RoomsDatabase) UpdateIngestTarget(
	ctx context.Context,
	roomID id.RoomID,
	eventID id.EventID,
	update func(*types.IngestTarget),
) (*types.IngestTarget, error) {
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*types.IngestTarget, error) {
		target, err := r.txnGetIngestTarget(txn, roomID, eventID)
		if err != nil || target == nil {
			return nil, err
		}
		before := *target
		update(target)
		r.txnSetIngestTarget(txn, target)
		return &before, nil
	})
}

// FailIngestTarget records a join's failure in its user's iju, only while the target exists, so a
// runner that lost the job cannot fail a join another runner published
func (r *RoomsDatabase) FailIngestTarget(ctx context.Context, roomID id.RoomID, eventID id.EventID, failure *types.IngestFailure) (bool, error) {
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (bool, error) {
		cleared, err := r.txnClearIngestTargets(txn, roomID, []id.EventID{eventID})
		if err != nil || len(cleared) == 0 {
			return false, err
		} else if cleared[0].Kind != types.IngestTargetJoin {
			return true, nil
		}
		userID := joinUser(cleared[0])
		if join, err := r.txnGetIngestJoin(txn, roomID, userID); err != nil {
			return false, err
		} else if join != nil && join.EventID == eventID {
			txn.Set(r.keyForIngestJoin(roomID, userID), (&types.IngestJoin{EventID: eventID, Failure: failure}).ToMsgpack())
		}
		return true, nil
	})
}

// ClearIngestTargets also clears the iju of a join naming one of the targets
func (r *RoomsDatabase) ClearIngestTargets(ctx context.Context, roomID id.RoomID, eventIDs []id.EventID) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, r.txnClearDoneTargets(txn, roomID, eventIDs)
	})
	return err
}

// Normal reads, so two runners cannot both take the same PDU out of the queue
func (r *RoomsDatabase) txnClearIngestTargets(txn fdb.Transaction, roomID id.RoomID, eventIDs []id.EventID) ([]*types.IngestTarget, error) {
	eventIDs = slices.Compact(slices.Sorted(slices.Values(eventIDs)))
	futures := make([]fdb.FutureByteSlice, len(eventIDs))
	for i, eventID := range eventIDs {
		futures[i] = txn.Get(r.keyForIngestTarget(roomID, eventID))
	}
	cleared := make([]*types.IngestTarget, 0, len(eventIDs))
	var dequeued types.IngestQueue
	for i, future := range futures {
		b, err := future.Get()
		if err != nil {
			return nil, err
		} else if b == nil {
			continue
		}
		target, err := types.NewIngestTargetFromBytes(b, eventIDs[i])
		if err != nil {
			return nil, err
		}
		txn.Clear(r.keyForIngestTarget(roomID, eventIDs[i]))
		dequeued = dequeued.Add(queuedBy(target))
		cleared = append(cleared, target)
	}
	r.txnAddQueued(txn, roomID, types.IngestQueue{PDUs: -dequeued.PDUs, Bytes: -dequeued.Bytes})
	return cleared, nil
}

func (r *RoomsDatabase) txnClearDoneTargets(txn fdb.Transaction, roomID id.RoomID, eventIDs []id.EventID) error {
	cleared, err := r.txnClearIngestTargets(txn, roomID, eventIDs)
	if err != nil {
		return err
	}
	type joinRead struct {
		target *types.IngestTarget
		join   fdb.FutureByteSlice
	}
	var joins []joinRead
	for _, target := range cleared {
		if target.Kind == types.IngestTargetJoin {
			joins = append(joins, joinRead{target, txn.Get(r.keyForIngestJoin(roomID, joinUser(target)))})
		}
	}
	for _, read := range joins {
		b, err := read.join.Get()
		if err != nil {
			return err
		} else if b == nil {
			continue
		}
		join, err := types.NewIngestJoinFromBytes(b)
		if err != nil {
			return err
		} else if join.EventID == read.target.Event.ID {
			txn.Clear(r.keyForIngestJoin(roomID, joinUser(read.target)))
		}
	}
	return nil
}

// Clears stored PDUs whatever their outcome but joins only once accepted; a join stored otherwise is
// left for the job to fail or follow with a new join.
func (r *RoomsDatabase) txnClearPublishedTargets(
	txn fdb.Transaction,
	roomID id.RoomID,
	evs []*types.Event,
	targets map[id.EventID]types.IngestTargetKind,
) error {
	var joins []id.EventID
	for eventID, kind := range targets {
		if kind == types.IngestTargetJoin {
			joins = append(joins, eventID)
		}
	}
	futures := make([]fdb.FutureByteSlice, len(joins))
	for i, eventID := range joins {
		futures[i] = txn.Get(r.keyForIngestTarget(roomID, eventID))
	}
	for i, future := range futures {
		if b, err := future.Get(); err != nil {
			return err
		} else if b == nil {
			return fmt.Errorf("%w: %s", ErrIngestTargetGone, joins[i])
		}
	}
	return r.txnClearDoneTargets(txn, roomID, targetIDs(publishedTargets(evs, targets)))
}

// Runs in the transaction publishing the local join standing in for the target
func (r *RoomsDatabase) txnTakeIngestJoinTarget(txn fdb.Transaction, roomID id.RoomID, eventID id.EventID) error {
	if b, err := txn.Get(r.keyForIngestTarget(roomID, eventID)).Get(); err != nil {
		return err
	} else if b == nil {
		return fmt.Errorf("%w: %s", ErrIngestTargetGone, eventID)
	}
	return r.txnClearDoneTargets(txn, roomID, []id.EventID{eventID})
}

func targetIDs(targets []*types.IngestTarget) []id.EventID {
	ids := make([]id.EventID, len(targets))
	for i, target := range targets {
		ids[i] = target.Event.ID
	}
	return ids
}

func publishedTargets(evs []*types.Event, targets map[id.EventID]types.IngestTargetKind) []*types.IngestTarget {
	var published []*types.IngestTarget
	for _, ev := range evs {
		kind, found := targets[ev.ID]
		if found && (kind != types.IngestTargetJoin || !ev.Rejected && !ev.SoftFailed) {
			published = append(published, &types.IngestTarget{Kind: kind, Event: ev})
		}
	}
	return published
}

// ReplaceIngestJoin swaps in a join from a fresh make_join while the user's iju still names the old one
func (r *RoomsDatabase) ReplaceIngestJoin(ctx context.Context, old *types.IngestTarget, joinEv *types.Event, servers []string) (bool, error) {
	userID := joinUser(old)
	if joinEv.StateKey == nil || id.UserID(*joinEv.StateKey) != userID || joinEv.RoomID != old.Event.RoomID {
		return false, fmt.Errorf("event %s is not a join of %s to %s", joinEv.ID, userID, old.Event.RoomID)
	}
	roomID := joinEv.RoomID
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (bool, error) {
		join, err := r.txnGetIngestJoin(txn, roomID, userID)
		if err != nil {
			return false, err
		} else if join == nil || join.EventID != old.Event.ID || join.Failure != nil {
			return false, nil
		}
		txn.Clear(r.keyForIngestTarget(roomID, old.Event.ID))
		r.txnSetIngestTarget(txn, &types.IngestTarget{
			Kind:         types.IngestTargetJoin,
			Event:        joinEv,
			Servers:      servers,
			Enqueued:     old.Enqueued,
			Replacements: old.Replacements + 1,
		})
		txn.Set(r.keyForIngestJoin(roomID, userID), (&types.IngestJoin{EventID: joinEv.ID}).ToMsgpack())
		return true, nil
	})
}

// FinishIngestJob reads the room's target range, so a target written meanwhile conflicts with the
// delete and the job carries on
func (r *RoomsDatabase) FinishIngestJob(ctx context.Context, roomID id.RoomID) (bool, error) {
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (bool, error) {
		kvs, err := txn.GetRange(r.ingestTargets.Sub(roomID.String()), fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err != nil || len(kvs) > 0 {
			return false, err
		}
		txn.Clear(r.keyForIngestJob(roomID))
		txn.ClearRange(r.ingestQueues.Sub(roomID.String()))
		return true, nil
	})
}

// A normal read, so a job written meanwhile conflicts with the transaction
func (r *RoomsDatabase) txnCheckNoIngestJob(txn fdb.Transaction, roomID id.RoomID) error {
	if b, err := txn.Get(r.keyForIngestJob(roomID)).Get(); err != nil {
		return err
	} else if b != nil {
		return fmt.Errorf("%w: %s", ErrRoomIngesting, roomID)
	}
	return nil
}
