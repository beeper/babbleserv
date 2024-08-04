package rooms

import (
	"context"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
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

func (r *RoomsDatabase) SyncRoomEventsForUser(
	ctx context.Context,
	userID id.UserID,
	options SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup][]*types.Event, error) {
	return r.syncRoomEvents(
		ctx,
		options,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.users.TxnLookupUserMemberships(txn, userID)
		},
		func(txn fdb.ReadTransaction, fromVersion, toVersion tuple.Versionstamp) (types.MembershipChanges, error) {
			return r.users.TxnLookupUserMembershipChanges(txn, userID, fromVersion, toVersion)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, fromVersion, toVersion tuple.Versionstamp, eventsProvider *events.TxnEventsProvider) ([]types.EventIDTupWithVersion, error) {
			return r.events.TxnPaginateRoomEventIDTups(txn, roomID, fromVersion, toVersion, options.Limit, eventsProvider)
		},
	)
}

func (r *RoomsDatabase) SyncRoomEventsForServer(
	ctx context.Context,
	serverName string,
	options SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup][]*types.Event, error) {
	return r.syncRoomEvents(
		ctx,
		options,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.servers.TxnLookupServerMemberships(txn, serverName)
		},
		func(txn fdb.ReadTransaction, fromVersion, toVersion tuple.Versionstamp) (types.MembershipChanges, error) {
			return r.servers.TxnLookupServerMembershipChanges(txn, serverName, fromVersion, types.ZeroVersionstamp)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, fromVersion, toVersion tuple.Versionstamp, eventsProvider *events.TxnEventsProvider) ([]types.EventIDTupWithVersion, error) {
			return r.events.TxnPaginateLocalRoomEventIDTups(txn, roomID, fromVersion, toVersion, options.Limit, eventsProvider)
		},
	)
}

func (r *RoomsDatabase) syncRoomEvents(
	ctx context.Context,
	options SyncOptions,
	getCurrentMembershipsFunc func(fdb.ReadTransaction) (types.Memberships, error),
	getMembershipChanges func(fdb.ReadTransaction, tuple.Versionstamp, tuple.Versionstamp) (types.MembershipChanges, error),
	paginateRoomEventIDs func(fdb.ReadTransaction, id.RoomID, tuple.Versionstamp, tuple.Versionstamp, *events.TxnEventsProvider) ([]types.EventIDTupWithVersion, error),
) (tuple.Versionstamp, map[types.MembershipTup][]*types.Event, error) {
	// Bump the from version, FDB ranges are inclusive but we want events *after* the from version
	options.From.UserVersion += 1

	// Get current memberships and latest event version in transaction, this means the memberships
	// are validate at that version and we can thus fetch events up to that version for each room.
	var latestVersion tuple.Versionstamp
	memberships, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Memberships, error) {
		latestVersion = r.events.TxnGetLatestEventVersion(txn)
		latestVersion.UserVersion += 1 // FDB range ends are exclusive
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
	type membershipAndEvents struct {
		membership  types.MembershipTup
		eventIDTups []types.EventIDTupWithVersion
	}

	var wg sync.WaitGroup
	doneCh := make(chan struct{})
	resultsCh := make(chan membershipAndEvents)
	allResults := make([]membershipAndEvents, 0, len(membershipsWithRanges))

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
			if evIDTups, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]types.EventIDTupWithVersion, error) {
				evIDTups, err := paginateRoomEventIDs(txn, membershipTup.RoomID, vRange.from, vRange.to, nil)
				if err != nil {
					return nil, err
				}
				return evIDTups, nil
			}); err != nil {
				panic(err)
			} else {
				resultsCh <- membershipAndEvents{membershipTup, evIDTups}
			}
		}()
	}

	wg.Wait()
	close(resultsCh)
	<-doneCh

	// Now we have all our event IDs / versions, we need to combine into a single slice and select
	// the first up to our limit, discarding the rest. We'll also need a room -> membership map.
	roomIDToMembership := make(map[id.RoomID]types.MembershipTup, len(membershipsWithRanges))
	allEvTups := make([]types.EventIDTupWithVersion, 0, len(membershipsWithRanges)*options.Limit)
	for _, memAndEvs := range allResults {
		roomIDToMembership[memAndEvs.membership.RoomID] = memAndEvs.membership
		allEvTups = append(allEvTups, memAndEvs.eventIDTups...)
	}

	// Sort and grab the first events up to our limit (or the entire slice)
	types.SortEventIDTupWithVersions(allEvTups)
	selectCount := options.Limit
	if len(allEvTups) < selectCount {
		selectCount = len(allEvTups)
	}
	chosenEvIDTups := allEvTups[:selectCount]

	if len(chosenEvIDTups) > 0 {
		// If we have more than one event in this batch, we must now override the token we return
		// as the next batch position with the greatest in this batch.
		latestVersion = chosenEvIDTups[len(chosenEvIDTups)-1].Version
	}

	// We finally have the events we need, now let's fetch them!
	eventsByRoom := make(map[types.MembershipTup][]*types.Event, len(membershipsWithRanges))
	now := time.Now()

	if _, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)

		for _, evIDTup := range chosenEvIDTups {
			ev := eventsProvider.MustGet(evIDTup.EventID)
			ev.Unsigned = map[string]any{
				"age":      now.UnixMilli() - ev.Timestamp,
				"hs.order": util.Base64EncodeURLSafe(types.ValueForVersionstamp(evIDTup.Version)),
			}

			membershipTup := roomIDToMembership[evIDTup.RoomID]
			if _, found := eventsByRoom[membershipTup]; !found {
				eventsByRoom[membershipTup] = make([]*types.Event, 0, options.Limit)
			}
			eventsByRoom[membershipTup] = append(eventsByRoom[membershipTup], ev)
		}
		return nil, nil
	}); err != nil {
		return types.ZeroVersionstamp, nil, err
	}

	return latestVersion, eventsByRoom, nil
}
