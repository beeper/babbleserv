package rooms

import (
	"context"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) InitRoomsForUser(
	ctx context.Context,
	userID id.UserID,
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	// Get current memberships and latest event version in transaction, this means the memberships
	// are valid at that version.
	var latestVersion tuple.Versionstamp
	memberships, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		latestVersion = util.TxnGetLatestWriteVersion(ctx, txn)
		return r.users.TxnLookupUserMemberships(txn, userID)
	})
	if err != nil {
		return types.ZeroVersionstamp, nil, err
	}

	// Now for each room fetch the current state of that room, plus any current (public) receipts,
	// note that thie state may be different from state at the start. Because we use latestVersion
	// for the next (incremental) sync any state changes may be re-delivered, which should be fine.
	type membershipAndSyncRoom struct {
		membership types.MembershipTup
		room       *types.SyncRoom
	}

	var wg sync.WaitGroup
	doneCh := make(chan struct{})
	resultsCh := make(chan membershipAndSyncRoom)
	allResults := make([]membershipAndSyncRoom, 0, len(memberships))

	go func() {
		for results := range resultsCh {
			allResults = append(allResults, results)
		}
		doneCh <- struct{}{}
	}()

	for roomID, membershipTup := range memberships {
		wg.Add(1)
		go func() {
			defer wg.Done()

			syncRoom := &types.SyncRoom{}

			syncRoom.StateEvents, err = r.GetCurrentRoomStateEvents(ctx, roomID)
			if err != nil {
				panic(err)
			}

			// Receipts may not be exactly aligned with the state events since we're using two txns
			syncRoom.Receipts, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]*types.Receipt, error) {
				return r.receipts.TxnGetCurrentReceiptsForRoom(txn, roomID, event.ReceiptTypeRead)
			})
			if err != nil {
				panic(err)
			}

			resultsCh <- membershipAndSyncRoom{membershipTup, syncRoom}
		}()
	}

	wg.Wait()
	close(resultsCh)
	<-doneCh

	roomEvents := make(map[types.MembershipTup]*types.SyncRoom, len(memberships))

	for _, result := range allResults {
		roomEvents[result.membership] = result.room
	}

	return latestVersion, roomEvents, nil
}
