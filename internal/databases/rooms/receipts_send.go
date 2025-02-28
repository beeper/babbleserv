package rooms

import (
	"context"
	"sync"

	"maunium.net/go/mautrix/id"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SendReceiptsResults struct {
	versionstampFut fdb.FutureKey
	change          notifier.Change

	Allowed  []*types.Receipt
	Rejected []RejectedReceipt
}

type RejectedReceipt struct {
	ReceiptTup *types.Receipt
	Error      error
}

func (r *RoomsDatabase) SendReceipts(
	ctx context.Context,
	roomID id.RoomID,
	rcs []*types.Receipt,
) (*SendReceiptsResults, error) {
	lock, _ := r.roomLocks.GetOrSet(roomID, &sync.Mutex{})
	lock.Lock()
	defer lock.Unlock()

	log := r.getTxnLogContext(ctx, "SendReceipts").
		Str("room_id", roomID.String()).
		Int("receipts", len(rcs)).
		Logger()

	if res, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (*SendReceiptsResults, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)

		allowedReceipts := make([]*types.Receipt, 0, len(rcs))
		rejectedReceipts := make([]RejectedReceipt, 0)

		for i, rc := range rcs {
			if rc.RoomID != roomID {
				panic("wrong room id provided")
			}

			if !r.users.TxnMustIsUserInRoom(txn, rc.UserID, rc.RoomID) {
				// Change from spec: silently ignore receipts for rooms the user is not a member of
				log.Warn().
					Stringer("user_id", rc.UserID).
					Stringer("room_id", rc.RoomID).
					Msg("Ignoring receipt where user is not in room")
				rejectedReceipts = append(rejectedReceipts, RejectedReceipt{rc, types.ErrUserNotInRoom})
				continue
			}

			if rc.UserID.Homeserver() == r.config.ServerName {
				// If user is local, check the event exists - a local should only ever know about
				// events we know about, or they'd not get them over sync.
				if _, err := eventsProvider.Get(rc.EventID); err != nil {
					log.Warn().
						Stringer("user_id", rc.UserID).
						Stringer("room_id", rc.RoomID).
						Stringer("event_id", rc.EventID).
						Msg("Ignoring receipt from local user for unknown event")
					rejectedReceipts = append(rejectedReceipts, RejectedReceipt{rc, types.ErrEventNotFound})
					continue
				}
			}

			kv := r.receipts.KeyValueForReceipt(rc)
			txn.Set(kv.Key, kv.Value)

			version := tuple.IncompleteVersionstamp(uint16(i))
			r.txnAddReceiptToSuperStream(txn, rc, version)

			allowedReceipts = append(allowedReceipts, rc)
		}

		return &SendReceiptsResults{
			versionstampFut: txn.GetVersionstamp(),

			Allowed:  allowedReceipts,
			Rejected: rejectedReceipts,
		}, nil
	}); err != nil {
		return nil, err
	} else {
		r.notifiers.Rooms.SendChange(res.change)

		for _, r := range res.Rejected {
			log.Warn().Err(r.Error).Str("user_id", r.ReceiptTup.UserID.String()).Msg("Receipt rejected")
		}

		rlog := log.Info().
			Int("receipts_allowed", len(res.Allowed)).
			Int("receipts_rejected", len(res.Rejected))
		if len(res.Allowed) > 0 {
			rlog = rlog.Str("versionstamp", res.versionstampFut.MustGet().String())
		}
		rlog.Msg("Sent receipts")

		return res, nil
	}
}
