package rooms

import (
	"context"
	"fmt"
	"math"
	"sync"

	"maunium.net/go/mautrix/event"
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

	if len(rcs) >= math.MaxUint16 {
		// Very unlikely! But safety first
		panic("too many rcs")
	}

	log := r.getTxnLogContext(ctx, "SendReceipts").
		Str("room_id", roomID.String()).
		Int("receipts", len(rcs)).
		Logger()

	if res, err := util.DoWriteTransactionWithVersion(ctx, r.db, func(txn fdb.Transaction) (*SendReceiptsResults, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)

		allowedReceipts := make([]*types.Receipt, 0, len(rcs))
		rejectedReceipts := make([]RejectedReceipt, 0)

		for i, rc := range rcs {
			if rc.RoomID != roomID {
				panic("wrong room id provided")
			}

			if !r.users.TxnIsUserJoinedRoom(txn, rc.UserID, rc.RoomID) {
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
				if ev, err := eventsProvider.Get(rc.EventID); err != nil {
					return nil, err
				} else if ev == nil {
					log.Warn().
						Stringer("user_id", rc.UserID).
						Stringer("room_id", rc.RoomID).
						Stringer("event_id", rc.EventID).
						Msg("Rejecting receipt from local user for unknown event")
					rejectedReceipts = append(rejectedReceipts, RejectedReceipt{rc, types.ErrEventNotFound})
					continue
				}
			}

			evVersion := r.events.TxnLookupVersionForEventID(txn, rc.EventID)
			if evVersion == types.ZeroVersionstamp {
				log.Warn().
					Str("event_id", rc.EventID.String()).
					Msg("Saving receipt with unknown event version")
			}
			rc.EventVersion = types.Version(evVersion)

			// Receipts are stored over 3x streams:
			// 1. public receipts (m.read) by room/version (for sync)
			// 2. public receipts from local users by room/version (for federation)
			// 3. private receipts (m.read.private) by user/version (for sync)
			// We also store receipt (user, room, thread, type) -> versionstamp, this is used to
			// ensure we only have one copy of each receipt per user/room/thread/type per stream,
			// when adding receipts we remove the previous in the stream.

			versionKey := r.receipts.KeyForReceiptVersion(rc)
			prevVersion := types.ZeroVersionstamp
			if b := txn.Get(versionKey).MustGet(); b != nil {
				prevVersion = types.MustBytesToVersionstamp(b)
			}

			version := tuple.IncompleteVersionstamp(uint16(i))
			receiptTupBytes := rc.ToBytes()

			switch rc.Type {
			case event.ReceiptTypeRead:
				// Store receipt in the room/version
				txn.SetVersionstampedKey(r.receipts.KeyForRoomVersion(rc.RoomID, version), receiptTupBytes)
				if prevVersion != types.ZeroVersionstamp {
					// Clear any previous value
					txn.Clear(r.receipts.KeyForRoomVersion(rc.RoomID, prevVersion))
				}
				if rc.UserID.Homeserver() == r.config.ServerName {
					// If user is local, add to the local stream
					txn.Set(r.receipts.KeyForLocalRoomVersion(rc.RoomID, version), receiptTupBytes)
					if prevVersion != types.ZeroVersionstamp {
						// Clear any previous value
						txn.Clear(r.receipts.KeyForLocalRoomVersion(rc.RoomID, prevVersion))
					}
				}
			case event.ReceiptTypeReadPrivate:
				txn.Set(r.receipts.KeyForUserVersion(rc.UserID, version), receiptTupBytes)
				if prevVersion != types.ZeroVersionstamp {
					// Clear any previous value
					txn.Clear(r.receipts.KeyForUserVersion(rc.UserID, prevVersion))
				}
			default:
				rejectedReceipts = append(rejectedReceipts, RejectedReceipt{rc, fmt.Errorf("invalid receipt type: %s", rc.Type)})
				continue
			}

			// Finally update the last version
			txn.SetVersionstampedValue(versionKey, types.MustVersionstampToBytes(version))

			allowedReceipts = append(allowedReceipts, rc)
		}

		// Bump the room version ahead of any receipts sent (max userID part of versionstamp)
		version := tuple.IncompleteVersionstamp(uint16(math.MaxUint16))
		txn.SetVersionstampedValue(r.KeyForRoomVersion(roomID), types.MustVersionstampToBytes(version))

		return &SendReceiptsResults{
			versionstampFut: txn.GetVersionstamp(),

			Allowed:  allowedReceipts,
			Rejected: rejectedReceipts,
		}, nil
	}); err != nil {
		return nil, err
	} else {
		r.notifier.SendChange(res.change)

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
