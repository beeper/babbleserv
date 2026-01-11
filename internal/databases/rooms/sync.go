package rooms

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (r *RoomsDatabase) SyncRoomsForUser(
	ctx context.Context,
	userID id.UserID,
	fromVersion tuple.Versionstamp,
	syncOpts types.SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	if syncOpts.UserID != userID { // extra paranoid check, TODO: fix this silliness!
		panic("sync user ID and options don't match")
	}
	return r.syncRoomEvents(
		ctx,
		fromVersion,
		syncOpts,
		nil,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.users.TxnLookupUserMemberships(txn, userID), nil
		},
		func(txn fdb.ReadTransaction, options types.PaginationOptions) (types.MembershipChanges, error) {
			return r.users.TxnLookupUserMembershipChanges(txn, userID, options), nil
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions, eventsProvider *events.TxnEventsProvider) []types.EventTupWithVersion {
			return r.events.TxnPaginateRoomEventTups(txn, roomID, options, eventsProvider, syncOpts.GetTimelineFilter())
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions, eventsProvider *events.TxnEventsProvider) []types.EventStateTupWithVersion {
			return r.events.TxnPaginateRoomStateEventTups(txn, roomID, options, eventsProvider)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions) ([]*types.ReceiptWithVersion, error) {
			rcs, err := r.receipts.TxnPaginateRoomReceipts(txn, roomID, options)
			if err != nil {
				return nil, err
			}
			privateRcs, err := r.receipts.TxnPaginateUserPrivateReceipts(txn, userID, options)
			if err != nil {
				return nil, err
			}
			return append(rcs, privateRcs...), nil
		},
	)
}

func (r *RoomsDatabase) SyncRoomsForServer(
	ctx context.Context,
	serverName string,
	fromVersion tuple.Versionstamp,
	syncOpts types.SyncOptions,
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	// Ensure we're configured for S2S sync
	syncOpts.IsServerToServer = true

	return r.syncRoomEvents(
		ctx,
		fromVersion,
		syncOpts,
		nil,
		func(txn fdb.ReadTransaction) (types.Memberships, error) {
			return r.servers.TxnLookupServerMemberships(txn, serverName)
		},
		func(txn fdb.ReadTransaction, options types.PaginationOptions) (types.MembershipChanges, error) {
			return r.servers.TxnLookupServerMembershipChanges(txn, serverName, options)
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions, eventsProvider *events.TxnEventsProvider) []types.EventTupWithVersion {
			return r.events.TxnPaginateLocalRoomEventTups(txn, roomID, options, eventsProvider, syncOpts.GetTimelineFilter())
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions, eventsProvider *events.TxnEventsProvider) []types.EventStateTupWithVersion {
			panic("server sync should never have gaps in state")
		},
		func(txn fdb.ReadTransaction, roomID id.RoomID, options types.PaginationOptions) ([]*types.ReceiptWithVersion, error) {
			return r.receipts.TxnPaginateLocalRoomReceipts(txn, roomID, options)
		},
	)
}

// Implements rooms sync for events and read receipts
func (r *RoomsDatabase) syncRoomEvents(
	ctx context.Context,
	fromVersion tuple.Versionstamp,
	options types.SyncOptions,
	lastSentPositions map[id.RoomID]tuple.Versionstamp,
	getCurrentMembershipsFunc func(fdb.ReadTransaction) (types.Memberships, error),
	getMembershipChanges func(fdb.ReadTransaction, types.PaginationOptions) (types.MembershipChanges, error),
	paginateRoomEvents func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions, *events.TxnEventsProvider) []types.EventTupWithVersion,
	paginateRoomStateEvents func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions, *events.TxnEventsProvider) []types.EventStateTupWithVersion,
	paginateRoomReceipts func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions) ([]*types.ReceiptWithVersion, error),
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	// First stage - select the rooms, data and version range of each, we're going to pull for this
	// request this changes depending on which sync mode we're using.
	// - if sliding: most recently updated X rooms, from last sent pos -> latest
	// - if legacy or streaming: all rooms, from since -> latest
	// Care must be taken to fetch room version data in a single transaction. We must also account
	// for membership changes between since -> latest and appropriately trim the version ranges of
	// those rooms as we join/unjoin the room.

	// Get the latest rooms database version, the current memberships and the latest version of each
	// room in a single txn so they are all consistent with each other.
	var memberships types.Memberships
	var roomVersions map[id.RoomID]tuple.Versionstamp
	latestVersion, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (tuple.Versionstamp, error) {
		latestVersion := util.TxnGetLatestWriteVersion(txn)
		var err error
		memberships, err = getCurrentMembershipsFunc(txn)
		if err != nil {
			return latestVersion, err
		}
		roomVersions, err = r.txnGetVersionsForMemberships(txn, memberships)
		if err != nil {
			return latestVersion, err
		}
		return latestVersion, nil
	})
	if err != nil {
		return types.ZeroVersionstamp, nil, err
	}

	isInitSync := fromVersion == types.ZeroVersionstamp

	// Calculate map of room membership -> sync config
	roomsToSync := make(map[types.MembershipTup]*roomSyncConfig, len(memberships))
	if options.Mode == types.SyncModeSliding {
		// TODO: calculate *joined* room IDs based on the lists + room subscriptions
		// for roomID
		// from = lastSentVersion
		// initial = no lastSentVersion
	} else {
		// Legacy or streaming sync: select all joined rooms, init if zero from, always include receipts
		for _, membershipTup := range memberships {
			if !isInitSync {
				// We only care about joined rooms for incremental sync, any other relevant membership
				// events (leave/ban/knock) come via membership changes.
				if membershipTup.Membership != event.MembershipJoin {
					continue
				}
				// Skip rooms that have no changes since our from version, safe even below because
				// this guarantees no membership changes in the room as well.
				roomVersion := roomVersions[membershipTup.RoomID]
				if types.VersionIsAtOrBefore(roomVersion, fromVersion) {
					zerolog.Ctx(ctx).Trace().Any("membership_tup", membershipTup).Msg("Skip room with no changes")
					continue
				}
			}

			// Only get receipts if joined
			includeReceipts := false
			if membershipTup.Membership == event.MembershipJoin {
				includeReceipts = true
			}

			roomsToSync[membershipTup] = &roomSyncConfig{
				from:            fromVersion,
				to:              latestVersion,
				isInitial:       isInitSync,
				includeReceipts: includeReceipts,
			}
		}
	}

	// If not init: Get membership changes options.From -> toVersion
	if !isInitSync {
		membershipChanges, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.MembershipChanges, error) {
			return getMembershipChanges(txn, types.PaginationOptions{
				From: fromVersion,
				To:   latestVersion,
			})
		})
		if err != nil {
			return types.ZeroVersionstamp, nil, err
		}

		for _, membershipChange := range membershipChanges {
			if _, found := roomsToSync[membershipChange.MembershipTup]; !found {
				// This means we're not joined to the room currently, so place an empty sync config
				// as we have to at least pass the membership event down.
				roomsToSync[membershipChange.MembershipTup] = &roomSyncConfig{}
			}
			roomConfig := roomsToSync[membershipChange.MembershipTup]

			switch membershipChange.Membership {
			case event.MembershipJoin:
				// This means we joined at this point, after the since token, so fetch events from,
				// and including, the membership join and flag the room as initial.
				from := membershipChange.Version
				from.UserVersion-- // ensure we include the membership event itself
				roomConfig.from = from
				roomConfig.to = latestVersion
				if !options.IsServerToServer {
					roomConfig.isInitial = true
				}
			case event.MembershipLeave, event.MembershipBan:
				// This means we left at this point, after the since token, so fetch events until
				// the leave event.
				// TODO: unused currently, since non-joins -> syncRoomNotJoinedTups
				roomConfig.from = fromVersion
				roomConfig.to = membershipChange.Version
			}
		}
	}

	// Second stage - find the event IDs and receipts to send down for each room. Depends on the
	// input room range and sync mode.
	// - if streaming: fetch oldest -> newest per room, up to <limit>
	// - if sliding or legacy: fetch most recent <timeline_limit>+1 per our version range, if we have
	//   the +1, we also need to fetch room state events sent from since -> oldest timeline event.
	// For receipts in either case we just get everything in the range.

	roomResults := make(map[types.MembershipTup]*roomSyncResult, len(roomsToSync))

	var wg sync.WaitGroup
	for membershipTup, roomConfig := range roomsToSync {
		res := &roomSyncResult{
			roomSyncConfig: roomConfig,
		}
		roomResults[membershipTup] = res

		if membershipTup.Membership != event.MembershipJoin {
			// For non-joined rooms we just fetch the membership event
			wg.Add(1)
			go func(membershipTup types.MembershipTup, res *roomSyncResult) {
				defer wg.Done()
				if err := r.syncRoomNotJoinedTups(ctx, options, membershipTup, res); err != nil {
					panic(err)
				}
			}(membershipTup, res)
			continue
		}

		wg.Add(1)
		go func(membershipTup types.MembershipTup, res *roomSyncResult) {
			defer wg.Done()
			// Mutates: res.eventTups + res.eventStateTups + res.limited, fetch event ID tups in range
			if err := r.syncRoomEventTups(ctx, options, membershipTup, res, paginateRoomEvents, paginateRoomStateEvents); err != nil {
				panic(fmt.Errorf("failed to sync room events: %s: %w", membershipTup.RoomID, err))
			}
		}(membershipTup, res)

		if roomConfig.includeReceipts {
			wg.Add(1)
			go func(membershipTup types.MembershipTup, res *roomSyncResult) {
				defer wg.Done()
				// Mutates: res.receipts
				if err := r.syncRoomReceipts(ctx, options, membershipTup, res, paginateRoomReceipts); err != nil {
					panic(fmt.Errorf("failed to sync room receipts: %s: %w", membershipTup.RoomID, err))
				}
			}(membershipTup, res)
		}
	}

	// Wait for all the EventTups+Receipts to be fetched
	wg.Wait()

	// If streaming: combine them all together, grab the first <limit> and drop the rest. Then we
	// update the latest position to that of the latest event in the selected list. This means we're
	// over-paginating event ID tups when the since token is far behind now. Edge-case-y enough not
	// to be a big concern.
	if options.Mode == types.SyncModeStreaming && !isInitSync {
		latestVersion = r.filterStreamingSyncTups(options, roomResults, latestVersion)
	}

	// Now that we have our final set of rooms, fetch the events!
	rooms := make(map[types.MembershipTup]*types.SyncRoom, len(roomResults))

	_, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		idToVersion := make(map[id.EventID]tuple.Versionstamp, 10)

		// First pass: start fetching all the timeline events
		for _, room := range roomResults {
			roomEvIDs := make(map[id.EventID]struct{}, len(room.eventTups))
			for _, tup := range room.eventTups {
				eventsProvider.WillGet(tup.EventID)
				idToVersion[tup.EventID] = tup.Version
				roomEvIDs[tup.EventID] = struct{}{}
			}

			// Now get any state events not in timeline, update the state event IDs to only those
			newEventStateTups := make([]types.EventStateTupWithVersion, 0, len(room.eventStateTups))
			for _, tup := range room.eventStateTups {
				if _, found := roomEvIDs[tup.EventID]; !found {
					eventsProvider.WillGet(tup.EventID)
					idToVersion[tup.EventID] = tup.Version
					newEventStateTups = append(newEventStateTups, tup)
				}
			}
			room.eventStateTups = newEventStateTups
		}

		// Second pass: apply the changes
		for membershipTup, result := range roomResults {
			timeline := make([]*types.Event, len(result.eventTups))
			for i, tup := range result.eventTups {
				timeline[i] = eventsProvider.MustGet(tup.EventID)
				timeline[i].SetUnsigned("hs.order", types.MustVersionstampToString(idToVersion[tup.EventID]))
			}
			state := make([]*types.Event, len(result.eventStateTups))
			for i, tup := range result.eventStateTups {
				state[i] = eventsProvider.MustGet(tup.EventID)
				state[i].SetUnsigned("hs.order", types.MustVersionstampToString(idToVersion[tup.EventID]))
			}

			rooms[membershipTup] = &types.SyncRoom{
				TimelineEvents: types.Timeline{
					EventList: types.EventList{Events: timeline},
					Limited:   result.limited,
				},
				StateEvents: types.EventList{Events: state},
				Receipts:    result.receipts,
			}
		}

		return nil, nil
	})

	return latestVersion, rooms, err
}

func (r *RoomsDatabase) syncRoomNotJoinedTups(
	ctx context.Context,
	options types.SyncOptions,
	membershipTup types.MembershipTup,
	res *roomSyncResult,
) error {
	// Super simple: just include the membership event itself as a state event
	res.eventStateTups = []types.EventStateTupWithVersion{{
		EventStateTup: types.EventStateTup{
			EventID: membershipTup.EventID,
			StateTup: types.StateTup{
				Type:     event.StateMember,
				StateKey: options.UserID.String(),
			},
		},
	}}
	return nil
}

func (r *RoomsDatabase) syncRoomEventTups(
	ctx context.Context,
	options types.SyncOptions,
	membershipTup types.MembershipTup,
	res *roomSyncResult,
	paginateRoomEvents func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions, *events.TxnEventsProvider) []types.EventTupWithVersion,
	paginateRoomStateEvents func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions, *events.TxnEventsProvider) []types.EventStateTupWithVersion,
) error {
	reverse := true
	limit := options.GetTimelineLimit()

	if !res.isInitial {
		if options.Mode == types.SyncModeStreaming {
			// For streaming incremental sync fetch oldest -> newest events (no gaps).
			reverse = false
		} else {
			// For sliding/v2 incremental sync over-paginate events by 1 to indicate limited timline
			limit += 1
		}
	}

	zerolog.Ctx(ctx).Debug().
		Str("room_id", membershipTup.RoomID.String()).
		Any("version_from", res.from).
		Any("version_to", res.to).
		Bool("initial", res.isInitial).
		Bool("revesre", reverse).
		Int("limit", limit).
		Msg("Paginating room events for sync")

	// First grab the event IDs, most recent first unless streaming incremental
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		tups := paginateRoomEvents(txn, membershipTup.RoomID, types.PaginationOptions{
			From:    res.from,
			To:      res.to,
			Reverse: reverse,
			Limit:   limit,
		}, nil)
		if reverse {
			// Switch the timeline back to old -> new order
			slices.Reverse(tups)
		}
		res.eventTups = tups
		return nil, nil
	})
	if err != nil {
		return err
	}

	if res.isInitial {
		// Handle newly seen rooms (either init sync or newly joined room): fetch all current state
		// event IDs. Technically incorrect since this will fetch current, at time of transaction,
		// state, rather than at the time of latestVersion. In reality this means there's a small
		// chance state events might be delivered twice (once in this sync, once in the next) for
		// this room. Since clients need to handle double-delivery of events anyway, who cares.
		var stateEventID id.EventID
		_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
			stateMap := r.events.TxnLookupCurrentRoomStateMap(txn, membershipTup.RoomID, nil)
			// TODO: lazy loading members
			memberMap := r.events.TxnLookupCurrentRoomMemberStateMap(txn, membershipTup.RoomID, nil)
			stateTups := make([]types.EventStateTupWithVersion, 0, len(stateMap)+len(memberMap))
			for stateTup, evID := range stateMap {
				if stateTup.Type == event.StateCreate {
					stateEventID = evID
				}
				stateTups = append(stateTups, types.EventStateTupWithVersion{
					Version: res.to,
					EventStateTup: types.EventStateTup{
						EventID:  evID,
						StateTup: stateTup,
					},
				})
			}
			res.eventStateTups = stateTups

			// Initial syncs are always limited unless we have the create event in the timeline
			if stateEventID != "" {
				var hasCreateEvent bool
				for _, tup := range res.eventTups {
					if tup.EventID == stateEventID {
						hasCreateEvent = true
						break
					}
				}
				if !hasCreateEvent {
					res.limited = true
				}
			}
			return nil, nil
		})
		return err
	}

	if options.Mode == types.SyncModeStreaming {
		// Streaming mode is simple - there's no state gap ever
		return nil
	}

	if len(res.eventTups) == 0 {
		// If we have no timeline we don't need to fetch state either, this might happen if a rooms
		// version increases due to a receipt and thus there's no new events.
		return nil
	} else if len(res.eventTups) == limit {
		// We overfetched the timeline to indicate a gappy/limited sync
		res.limited = true
		res.eventTups = res.eventTups[1:]
	}

	// Fetch state tups:
	// - if sliding sync OR MSC4222 enabled all state changes from -> end of timeline
	// - if legacy sync without MSC4222 all state changes from -> start of timeline
	_, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		to := res.eventTups[len(res.eventTups)-1].Version
		if options.Mode == types.SyncModeLegacy && !options.EnableLegacyStateAfter {
			to = res.eventTups[0].Version
		}
		stateEvTups := paginateRoomStateEvents(txn, membershipTup.RoomID, types.PaginationOptions{
			From: res.from,
			To:   to,
			Mode: fdb.StreamingModeWantAll,
		}, nil)
		res.eventStateTups = stateEvTups
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) syncRoomReceipts(
	ctx context.Context,
	_ types.SyncOptions,
	membershipTup types.MembershipTup,
	res *roomSyncResult,
	paginateRoomReceipts func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions) ([]*types.ReceiptWithVersion, error),
) error {
	zerolog.Ctx(ctx).Debug().
		Str("room_id", membershipTup.RoomID.String()).
		Any("version_from", res.from).
		Any("version_to", res.to).
		Bool("initial", res.isInitial).
		Msg("Paginating room receipts for sync")

	if res.isInitial {
		// Get all receipts for the room
		_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
			// Receipts are stored in a per-room sparse stream, so we can just paginate the stream
			// TODO: recency limit
			rcs, err := paginateRoomReceipts(txn, membershipTup.RoomID, types.PaginationOptions{
				From: types.ZeroVersionstamp,
				To:   types.ZeroVersionstamp,
				Mode: fdb.StreamingModeWantAll,
			})
			if err != nil {
				return nil, err
			}
			res.receipts = rcs
			return nil, nil
		})
		return err
	}

	// Fetch receipts
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		rcs, err := paginateRoomReceipts(txn, membershipTup.RoomID, types.PaginationOptions{
			From: res.from,
			To:   res.to,
			Mode: fdb.StreamingModeWantAll,
		})
		if err != nil {
			return nil, err
		}
		res.receipts = rcs
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) filterStreamingSyncTups(
	options types.SyncOptions,
	roomResults map[types.MembershipTup]*roomSyncResult,
	latestVersion tuple.Versionstamp,
) tuple.Versionstamp {
	timelineLimit := options.GetTimelineLimit()
	allEventTups := make([]types.EventTupWithVersion, 0, len(roomResults)*timelineLimit)
	for _, res := range roomResults {
		allEventTups = append(allEventTups, res.eventTups...)
	}
	if len(allEventTups) > timelineLimit {
		// Now, overwrite the latest version with the latest selected event tup
		types.SortVersioners(allEventTups)
		allEventTups = allEventTups[:min(len(allEventTups), timelineLimit)]
		latestVersion = allEventTups[len(allEventTups)-1].Version
	}

	receiptsLimit := options.GetReceiptsLimit()
	allReceipts := make([]*types.ReceiptWithVersion, 0, len(roomResults)*receiptsLimit)
	for _, res := range roomResults {
		allReceipts = append(allReceipts, res.receipts...)
	}
	if len(allReceipts) > receiptsLimit {
		// Limit also applies to receipts, overwrite latest version *if* older than the one
		// already set. If newer we'll be trimming anything ahead already.
		types.SortVersioners(allReceipts)
		allReceipts = allReceipts[:min(len(allReceipts), receiptsLimit)]
		latestReceiptVersion := allReceipts[len(allReceipts)-1].Version
		if types.VersionIsBefore(latestReceiptVersion, latestVersion) {
			latestVersion = latestReceiptVersion
		}
	}

	// Now go through each room result and remove events + receipts ahead of the version
	for _, res := range roomResults {
		filteredTups := make([]types.EventTupWithVersion, 0, len(res.eventTups))
		for _, tup := range res.eventTups {
			if types.VersionIsAtOrBefore(tup.Version, latestVersion) {
				filteredTups = append(filteredTups, tup)
			}
		}
		res.eventTups = filteredTups

		filteredReceipts := make([]*types.ReceiptWithVersion, 0, len(res.receipts))
		for _, receipt := range res.receipts {
			if types.VersionIsAtOrBefore(receipt.Version, latestVersion) {
				filteredReceipts = append(filteredReceipts, receipt)
			}
		}
		res.receipts = filteredReceipts
	}

	return latestVersion
}
