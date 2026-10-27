package rooms

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// ErrRoomIngesting refuses a send that would publish past a room's ingest job, which publishes the
// room's PDUs while it runs
var ErrRoomIngesting = errors.New("the room has an ingest job")

// Targets read per transaction
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

// txnEnsureIngestJob writes the room's job unless it has one. Every target write calls it, so no
// target is left without a job to publish it.
func (r *RoomsDatabase) txnEnsureIngestJob(txn fdb.Transaction, roomID id.RoomID, roomVersion string) error {
	job, err := r.txnGetIngestJob(txn, roomID)
	if err != nil || job != nil {
		return err
	}
	job = &types.IngestJob{RoomVersion: roomVersion, Created: time.Now().UnixMilli()}
	txn.Set(r.keyForIngestJob(roomID), job.ToMsgpack())
	return nil
}

func (r *RoomsDatabase) txnSetIngestTarget(txn fdb.Transaction, target *types.IngestTarget) {
	txn.Set(r.keyForIngestTarget(target.Event.RoomID, target.Event.ID), target.ToMsgpack())
}

func joinUser(target *types.IngestTarget) id.UserID {
	return id.UserID(*target.Event.StateKey)
}

// EnqueueIngestJoin writes a local user's join as a target of the room's job, with the user's iju
// naming it. When the user has a join pending already, nothing is written and that join is returned
// to wait on instead.
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

// txnPendingIngestJoin returns the join target the user's iju names, if it has not failed
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

// GetPendingIngestJoin returns the join of the user to the room that the room's job has pending
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

// WatchIngestJoin returns the user's join to the room through its job, nil once published, and a
// watch firing when that changes
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

// EnqueueIngestPDUs writes PDUs from origin as targets of the room's job, other than those stored
// or targets already
func (r *RoomsDatabase) EnqueueIngestPDUs(ctx context.Context, roomID id.RoomID, origin string, evs []*types.Event) error {
	if len(evs) == 0 {
		return nil
	}
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
		if err := r.txnEnsureIngestJob(txn, roomID, evs[0].RoomVersion); err != nil {
			return nil, err
		}
		versions := make([]fdb.FutureByteSlice, len(evs))
		targets := make([]fdb.FutureByteSlice, len(evs))
		for i, ev := range evs {
			versions[i] = txn.Get(r.events.KeyForIDToVersion(ev.ID))
			targets[i] = txn.Get(r.keyForIngestTarget(roomID, ev.ID))
		}
		now := time.Now().UnixMilli()
		for i, ev := range evs {
			if ev.RoomID != roomID || versions[i].MustGet() != nil || targets[i].MustGet() != nil {
				continue
			}
			r.txnSetIngestTarget(txn, &types.IngestTarget{
				Kind:     types.IngestTargetPDU,
				Event:    ev,
				Servers:  []string{origin},
				Enqueued: now,
			})
		}
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) GetIngestJob(ctx context.Context, roomID id.RoomID) (*types.IngestJob, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.IngestJob, error) {
		return r.txnGetIngestJob(txn, roomID)
	})
}

// IngestJobRoomIDs pages through the rooms with an ingest job, from after the given room
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

// GetIngestTargets returns a room's job and its targets, reading them over several transactions,
// or no job
func (r *RoomsDatabase) GetIngestTargets(ctx context.Context, roomID id.RoomID) (*types.IngestJob, []*types.IngestTarget, error) {
	job, err := r.GetIngestJob(ctx, roomID)
	if err != nil || job == nil {
		return nil, nil, err
	}
	var targets []*types.IngestTarget
	begin, end := r.ingestTargets.Sub(roomID.String()).FDBRangeKeys()
	for {
		page, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.IngestTarget, error) {
			kvs, err := txn.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{Limit: ingestTargetsPage}).GetSliceWithError()
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
		})
		if err != nil {
			return nil, nil, err
		}
		targets = append(targets, page...)
		if len(page) < ingestTargetsPage {
			return job, targets, nil
		}
		last := page[len(page)-1].Event
		begin = fdb.Key(append(r.keyForIngestTarget(roomID, last.ID), 0))
	}
}

// UpdateIngestTarget changes a target as long as it exists, returning it as it was before, or nil
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

// FailIngestTarget removes a target that failed, recording a join's failure in its user's iju. It
// writes only while the target exists, so a runner that lost the job cannot fail a join another
// runner published.
func (r *RoomsDatabase) FailIngestTarget(ctx context.Context, roomID id.RoomID, eventID id.EventID, failure *types.IngestFailure) (bool, error) {
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (bool, error) {
		target, err := r.txnGetIngestTarget(txn, roomID, eventID)
		if err != nil || target == nil {
			return false, err
		}
		txn.Clear(r.keyForIngestTarget(roomID, eventID))
		if target.Kind != types.IngestTargetJoin {
			return true, nil
		}
		userID := joinUser(target)
		if join, err := r.txnGetIngestJoin(txn, roomID, userID); err != nil {
			return false, err
		} else if join != nil && join.EventID == eventID {
			txn.Set(r.keyForIngestJoin(roomID, userID), (&types.IngestJoin{EventID: eventID, Failure: failure}).ToMsgpack())
		}
		return true, nil
	})
}

// ClearIngestTargets removes targets the job is done with that their publish did not remove, with
// the iju of a join naming one
func (r *RoomsDatabase) ClearIngestTargets(ctx context.Context, roomID id.RoomID, eventIDs []id.EventID) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
		for _, eventID := range eventIDs {
			target, err := r.txnGetIngestTarget(txn, roomID, eventID)
			if err != nil {
				return nil, err
			} else if target != nil {
				if err := r.txnClearIngestTarget(txn, target); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) txnClearIngestTarget(txn fdb.Transaction, target *types.IngestTarget) error {
	roomID := target.Event.RoomID
	txn.Clear(r.keyForIngestTarget(roomID, target.Event.ID))
	if target.Kind != types.IngestTargetJoin {
		return nil
	}
	userID := joinUser(target)
	if join, err := r.txnGetIngestJoin(txn, roomID, userID); err != nil {
		return err
	} else if join != nil && join.EventID == target.Event.ID {
		txn.Clear(r.keyForIngestJoin(roomID, userID))
	}
	return nil
}

// txnClearPublishedTargets removes, in the transaction publishing them, the targets a publish is
// done with: stored PDUs whatever their outcome, accepted joins. A join stored otherwise is left for
// the job to fail or follow with a new join.
func (r *RoomsDatabase) txnClearPublishedTargets(
	txn fdb.Transaction,
	evs []*types.Event,
	targets map[id.EventID]types.IngestTargetKind,
) error {
	for _, target := range publishedTargets(evs, targets) {
		if err := r.txnClearIngestTarget(txn, target); err != nil {
			return err
		}
	}
	return nil
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

// ReplaceIngestJoin replaces a join target with a new join event of the same user, from a fresh
// make_join, as long as the user's iju still names the old one
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
			Kind:     types.IngestTargetJoin,
			Event:    joinEv,
			Servers:  servers,
			Enqueued: old.Enqueued,
		})
		txn.Set(r.keyForIngestJoin(roomID, userID), (&types.IngestJoin{EventID: joinEv.ID}).ToMsgpack())
		return true, nil
	})
}

// FinishIngestJob deletes a room's job once it has no targets. It reads the room's target range, so
// a target written meanwhile conflicts with the delete and the job carries on.
func (r *RoomsDatabase) FinishIngestJob(ctx context.Context, roomID id.RoomID) (bool, error) {
	return util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (bool, error) {
		kvs, err := txn.GetRange(r.ingestTargets.Sub(roomID.String()), fdb.RangeOptions{Limit: 1}).GetSliceWithError()
		if err != nil || len(kvs) > 0 {
			return false, err
		}
		txn.Clear(r.keyForIngestJob(roomID))
		return true, nil
	})
}

// txnCheckNoIngestJob fails with ErrRoomIngesting while the room has a job. A normal read, so a job
// written meanwhile conflicts with the transaction.
func (r *RoomsDatabase) txnCheckNoIngestJob(txn fdb.Transaction, roomID id.RoomID) error {
	if b, err := txn.Get(r.keyForIngestJob(roomID)).Get(); err != nil {
		return err
	} else if b != nil {
		return fmt.Errorf("%w: %s", ErrRoomIngesting, roomID)
	}
	return nil
}
