package rooms

import (
	"context"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type SyncOptions struct {
	// Starting position to get events *after*
	From tuple.Versionstamp
	// Limit of events returned
	Limit int
}

func (r *RoomsDatabase) SyncRoomsForUser(
	ctx context.Context,
	userID id.UserID,
	options SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	return r.syncRoomEvents(
		ctx,
		options,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.users.TxnLookupUserMemberships(txn, userID)
		},
		func(txn fdb.ReadTransaction, fromVersion, toVersion tuple.Versionstamp) (types.MembershipChanges, error) {
			return r.users.TxnLookupUserMembershipChanges(txn, userID, fromVersion, toVersion)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, fromVersion, toVersion tuple.Versionstamp, eventsProvider *events.TxnEventsProvider) ([]SuperStreamItem, error) {
			return r.txnPaginateRoomSuperStream(txn, roomID, fromVersion, toVersion, options.Limit, eventsProvider)
		},
	)
}

func (r *RoomsDatabase) SyncRoomsForServer(
	ctx context.Context,
	serverName string,
	options SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	return r.syncRoomEvents(
		ctx,
		options,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.servers.TxnLookupServerMemberships(txn, serverName)
		},
		func(txn fdb.ReadTransaction, fromVersion, toVersion tuple.Versionstamp) (types.MembershipChanges, error) {
			return r.servers.TxnLookupServerMembershipChanges(txn, serverName, fromVersion, types.ZeroVersionstamp)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, fromVersion, toVersion tuple.Versionstamp, eventsProvider *events.TxnEventsProvider) ([]SuperStreamItem, error) {
			return r.txnPaginateRoomLocalSuperStream(txn, roomID, fromVersion, toVersion, options.Limit, eventsProvider)
		},
	)
}

// Implements rooms sync for events and read receipts
func (r *RoomsDatabase) syncRoomEvents(
	ctx context.Context,
	options SyncOptions,
	getCurrentMembershipsFunc func(fdb.ReadTransaction) (types.Memberships, error),
	getMembershipChanges func(fdb.ReadTransaction, tuple.Versionstamp, tuple.Versionstamp) (types.MembershipChanges, error),
	paginateRoomSuperStream func(fdb.ReadTransaction, id.RoomID, tuple.Versionstamp, tuple.Versionstamp, *events.TxnEventsProvider) ([]SuperStreamItem, error),
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	// Bump the from version, FDB range starts are inclusive but we want events *after* the version
	options.From.UserVersion += 1

	// Get current memberships and latest event version in transaction, this means the memberships
	// are valid at that version and we can thus fetch events up to that version for each room.
	var latestVersion tuple.Versionstamp
	memberships, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		latestVersion = util.TxnGetLatestWriteVersion(ctx, txn)
		return getCurrentMembershipsFunc(txn)
	})
	if err != nil {
		return types.ZeroVersionstamp, nil, err
	}

	type versionRange struct {
		from, to tuple.Versionstamp
	}

	membershipsWithRanges := make(map[types.MembershipTup]*versionRange, len(memberships))
	for _, membershipTup := range memberships {
		membershipsWithRanges[membershipTup] = &versionRange{options.From, latestVersion}
	}

	// Get membership changes options.From -> toVersion
	membershipChanges, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.MembershipChanges, error) {
		return getMembershipChanges(txn, options.From, latestVersion)
	})
	for _, membershipChange := range membershipChanges {
		vRange, found := membershipsWithRanges[membershipChange.MembershipTup]
		if !found {
			// TODO: does this logic (default from/latest) actually make sense? Should this even
			// ever happen? If you left + forgot a room. You are not a member any more, so drop?
			vRange = &versionRange{options.From, latestVersion}
			membershipsWithRanges[membershipChange.MembershipTup] = vRange
		}
		switch membershipChange.Membership {
		case event.MembershipJoin:
			// If we joined the room, only get events since the join version
			vRange.from = membershipChange.Version
		default:
			// For any non-join membership, don't get events after that version
			vRange.to = membershipChange.Version
		}
	}

	// Now we're going to fetch up to the limit event ID/version tups in each room
	type membershipAndItems struct {
		membership types.MembershipTup
		items      []SuperStreamItem
	}

	var wg sync.WaitGroup
	doneCh := make(chan struct{})
	resultsCh := make(chan membershipAndItems)
	allResults := make([]membershipAndItems, 0, len(membershipsWithRanges))

	go func() {
		for results := range resultsCh {
			allResults = append(allResults, results)
		}
		doneCh <- struct{}{}
	}()

	for membershipTup, vRange := range membershipsWithRanges {
		wg.Add(1)
		go func() {
			defer wg.Done()

			zerolog.Ctx(ctx).Debug().
				Str("room_id", membershipTup.RoomID.String()).
				Str("version_from", vRange.from.String()).
				Str("version_to", vRange.to.String()).
				Msg("Paginating room super stream")

			items, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]SuperStreamItem, error) {
				return paginateRoomSuperStream(txn, membershipTup.RoomID, vRange.from, vRange.to, nil)
			})
			if err != nil {
				panic(err)
			}

			resultsCh <- membershipAndItems{membershipTup, items}
		}()
	}

	wg.Wait()
	close(resultsCh)
	<-doneCh

	// Now we have all our event IDs / versions, we need to combine into a single slice and select
	// the first up to our limit, discarding the rest. We'll also need a room -> membership map.
	roomIDToMembership := make(map[id.RoomID]types.MembershipTup, len(membershipsWithRanges))
	allItems := make([]SuperStreamItem, 0, len(membershipsWithRanges)*options.Limit)
	for _, memAndEvs := range allResults {
		roomIDToMembership[memAndEvs.membership.RoomID] = memAndEvs.membership
		allItems = append(allItems, memAndEvs.items...)
	}

	// Sort and grab the first events up to our limit (or the entire slice)
	types.SortVersioners(allItems)
	items := allItems[:util.MinInt(options.Limit, len(allItems))]

	if len(items) == options.Limit {
		// If our batch is full override the next batch to the greatest of this one as we're not
		// up to date with latestVersion.
		latestVersion = items[len(items)-1].Version
	}

	// We finally have the events we need, now let's fetch them!
	rooms := make(map[types.MembershipTup]*types.SyncRoom, len(membershipsWithRanges))
	now := time.Now()

	getSyncRoom := func(roomID id.RoomID) *types.SyncRoom {
		membershipTup := roomIDToMembership[roomID]
		if _, found := rooms[membershipTup]; !found {
			rooms[membershipTup] = &types.SyncRoom{}
		}
		return rooms[membershipTup]
	}

	if _, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)

		for _, item := range items {
			switch item.Type {
			case SuperStreamReceipt:
				room := getSyncRoom(item.Receipt.RoomID)
				room.Receipts = append(room.Receipts, item.Receipt)
			case SuperStreamEvent:
				evIDTup := item.EventIDTup
				ev := eventsProvider.MustGet(evIDTup.EventID)
				ev.Unsigned = map[string]any{
					"age":      now.UnixMilli() - ev.Timestamp,
					"hs.order": util.Base64EncodeURLSafe(types.VersionstampToValue(item.Version)),
				}
				room := getSyncRoom(evIDTup.RoomID)
				room.TimelineEvents = append(room.TimelineEvents, ev)
			}
		}

		return nil, nil
	}); err != nil {
		return types.ZeroVersionstamp, nil, err
	}

	return latestVersion, rooms, nil
}
