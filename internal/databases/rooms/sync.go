package rooms

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
	"github.com/beeper/babbleserv/internal/databases/rooms/state"
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
			return r.events.TxnPaginateRoomEventTups(txn, roomID, options, eventsProvider)
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
			return r.events.TxnPaginateLocalRoomEventTups(txn, roomID, options, eventsProvider)
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
	paginateRoomReceipts func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions) ([]*types.ReceiptWithVersion, error),
) (tuple.Versionstamp, map[types.MembershipTup]*types.SyncRoom, error) {
	// First stage - select the rooms, data and version range of each, we're going to pull for this
	// request this changes depending on which sync mode we're using.
	// - if sliding: most recently updated X rooms, from last sent pos -> latest
	// - if legacy or streaming: all rooms, from since -> latest
	// Care must be taken to fetch room version data in a single transaction. We must also account
	// for membership changes between since -> latest and appropriately trim the version ranges of
	// those rooms as we join/unjoin the room.

	isInitSync := fromVersion == types.ZeroVersionstamp
	isServerSync := options.IsServerToServer

	// Get the latest rooms database version, the current memberships and the latest version of each
	// room in a single txn so they are all consistent with each other.
	var memberships, localMemberships types.Memberships
	var roomVersions map[id.RoomID]tuple.Versionstamp
	latestVersion, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (tuple.Versionstamp, error) {
		latestVersion := util.TxnGetLatestWriteVersion(txn)
		var err error
		memberships, err = getCurrentMembershipsFunc(txn)
		if err != nil {
			return latestVersion, err
		}
		if isServerSync {
			localMemberships, err = r.servers.TxnLookupServerMemberships(txn, r.config.ServerName)
			if err != nil {
				return latestVersion, err
			}
		}
		roomVersions, err = r.txnGetVersionsForMemberships(txn, memberships)
		return latestVersion, err
	})
	if err != nil {
		return types.ZeroVersionstamp, nil, err
	}

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
				} else {
					zerolog.Ctx(ctx).Trace().Any("membership_tup", membershipTup).Any("room_version", roomVersion).Any("from_version", fromVersion).Any("latest_version", latestVersion).Msg("Including room with changes")
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
		var localChanges types.MembershipChanges
		membershipChanges, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.MembershipChanges, error) {
			options := types.PaginationOptions{From: fromVersion, To: latestVersion}
			changes, err := getMembershipChanges(txn, options)
			if err != nil {
				return nil, err
			}
			if isServerSync {
				localChanges, err = r.servers.TxnLookupServerMembershipChanges(txn, r.config.ServerName, options)
				if err != nil {
					return nil, err
				}
			}
			return changes, nil
		})
		if err != nil {
			return types.ZeroVersionstamp, nil, err
		}

		joinEnds := joinEntryEnds(membershipChanges, latestVersion)
		for i, membershipChange := range membershipChanges {
			if _, found := roomsToSync[membershipChange.MembershipTup]; !found {
				// This means we're not joined to the room currently, so place an empty sync config
				// as we have to at least pass the membership event down.
				roomsToSync[membershipChange.MembershipTup] = &roomSyncConfig{}
			}
			roomConfig := roomsToSync[membershipChange.MembershipTup]
			roomConfig.changedAt = membershipChange.Version

			switch membershipChange.Membership {
			case event.MembershipJoin:
				// Joined after since. A client gets the room as initial from the join on. A server gets
				// the events after the join it was recorded with, which it has: its own, or our remote
				// join, which the resident server sends to the room.
				roomConfig.to = joinEnds[i]
				if isServerSync {
					roomConfig.from, roomConfig.server.serverJoined = fromVersion, membershipChange.Version
				} else {
					// Only the zero versionstamp has no predecessor, and a zero from is the room's start
					roomConfig.from, _ = types.VersionstampBefore(membershipChange.Version)
					roomConfig.isInitial = true
				}
			case event.MembershipLeave, event.MembershipBan:
				// This means we left at this point, after the since token, so fetch events until
				// the leave event.
				// TODO: unused currently; non-joined rooms only include the membership event.
				roomConfig.from = fromVersion
				roomConfig.to = membershipChange.Version
			}
		}

		if isServerSync {
			serverChangesByRoom, localChangesByRoom := changesByRoom(membershipChanges), changesByRoom(localChanges)
			for membershipTup, roomConfig := range roomsToSync {
				roomID := membershipTup.RoomID
				roomConfig.server.serverChanges = serverChangesByRoom[roomID]
				roomConfig.server.serverJoinedNow = memberships[roomID].Membership == event.MembershipJoin
				roomConfig.server.localChanges = localChangesByRoom[roomID]
				roomConfig.server.localJoinedNow = localMemberships[roomID].Membership == event.MembershipJoin
			}
		}
	}

	// Second stage - find the event IDs and receipts to send down for each room. Depends on the
	// input room range and sync mode.
	// - if streaming: fetch oldest -> newest per room, up to <limit>
	// - if sliding or legacy: fetch most recent <timeline_limit>+1 per our version range, the +1
	//   telling a limited timeline
	// For receipts in either case we just get everything in the range.

	roomResults := make(map[types.MembershipTup]*roomSyncResult, len(roomsToSync))

	var wg sync.WaitGroup
	for membershipTup, roomConfig := range roomsToSync {
		res := &roomSyncResult{
			roomSyncConfig: roomConfig,
		}
		roomResults[membershipTup] = res

		if membershipTup.Membership != event.MembershipJoin {
			res.eventStateTups = []types.EventStateTupWithVersion{{
				EventStateTup: types.EventStateTup{
					EventID:  membershipTup.EventID,
					StateTup: types.MemberStateTup(options.UserID),
				},
			}}
			continue
		}

		wg.Go(func() {
			// Mutates: res.eventTups + res.limited, fetch event ID tups in range, after a server's
			// range of the room is applied, so receipts follow
			if err := r.syncRoomEventTups(ctx, options, membershipTup, res, paginateRoomEvents); err != nil {
				panic(fmt.Errorf("failed to sync room events: %s: %w", membershipTup.RoomID, err))
			} else if !roomConfig.includeReceipts || res.denied {
				return
			}
			// Mutates: res.receipts
			if err := r.syncRoomReceipts(ctx, options, membershipTup, res, paginateRoomReceipts); err != nil {
				panic(fmt.Errorf("failed to sync room receipts: %s: %w", membershipTup.RoomID, err))
			}
		})
	}

	// Wait for all the EventTups+Receipts to be fetched
	wg.Wait()
	for membershipTup, res := range roomResults {
		if res.denied {
			delete(roomResults, membershipTup)
		}
	}

	// If streaming: combine them all together, grab the first <limit> and drop the rest. Then we
	// update the latest position to that of the latest event in the selected list. This means we're
	// over-paginating event ID tups when the since token is far behind now. Edge-case-y enough not
	// to be a big concern.
	if options.Mode == types.SyncModeStreaming && !isInitSync {
		latestVersion = r.filterStreamingSyncTups(options, roomResults, fromVersion, latestVersion)
	}

	if !options.IsServerToServer {
		filterClientSyncRooms(roomResults)
		// Mutates: res.eventStateTups
		if err := r.syncRoomsStateTups(ctx, options, roomResults); err != nil {
			return types.ZeroVersionstamp, nil, err
		}
	}

	// Now that we have our final set of rooms, fetch the events!
	rooms := make(map[types.MembershipTup]*types.SyncRoom, len(roomResults))

	_, err = util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*struct{}, error) {
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		transactionIDs := make(map[id.EventID]fdb.FutureByteSlice)
		device := types.UserDevice{UserID: options.UserID, DeviceID: options.DeviceID}

		// First pass: start fetching all the timeline and state events
		for _, room := range roomResults {
			for _, tup := range room.eventTups {
				eventsProvider.WillGet(tup.EventID)
				if !options.IsServerToServer && options.DeviceID != "" && tup.Sender == options.UserID {
					transactionIDs[tup.EventID] = txn.Get(r.keyForEventTransactionID(tup.EventID, device))
				}
			}
			for _, tup := range room.eventStateTups {
				eventsProvider.WillGet(tup.EventID)
			}
		}

		// Second pass: apply the changes
		for membershipTup, result := range roomResults {
			timeline := make([]*types.Event, len(result.eventTups))
			for i, tup := range result.eventTups {
				timeline[i] = eventsProvider.MustGet(tup.EventID)
				if future, ok := transactionIDs[tup.EventID]; ok {
					timeline[i].ClientTransactionID = string(future.MustGet())
				}
				timeline[i].SetUnsigned("hs.order", types.MustVersionstampToString(tup.Version))
			}
			state := make([]*types.Event, len(result.eventStateTups))
			for i, tup := range result.eventStateTups {
				state[i] = eventsProvider.MustGet(tup.EventID)
				state[i].SetUnsigned("hs.order", types.MustVersionstampToString(tup.Version))
			}

			syncRoom := &types.SyncRoom{
				TimelineEvents: types.Timeline{
					EventList: types.EventList{Events: timeline},
					Limited:   result.limited,
				},
				StateEvents: types.EventList{Events: state},
				Receipts:    result.receipts,
			}

			// Add notification counts for joined rooms, pinned to latestVersion to avoid
			// over-counting if parallel events come in during sync
			if membershipTup.Membership == event.MembershipJoin && !options.IsServerToServer {
				if options.UseRoomThreadedNotifications() {
					// Thread-aware: separate main room and per-thread counts
					mainNotif, mainHighlight, threadCounts := r.users.TxnSumNotificationsByThread(
						txn, options.UserID, membershipTup.RoomID, latestVersion,
					)
					syncRoom.UnreadNotifications = &types.UnreadNotificationCounts{
						NotificationCount: mainNotif,
						HighlightCount:    mainHighlight,
					}
					if len(threadCounts) > 0 {
						syncRoom.UnreadThreadNotifications = threadCounts
					}
				} else {
					// Legacy: sum all notifications together regardless of thread
					notifCount, highlightCount := r.users.TxnSumNotifications(txn, options.UserID, membershipTup.RoomID, latestVersion)
					syncRoom.UnreadNotifications = &types.UnreadNotificationCounts{
						NotificationCount: notifCount,
						HighlightCount:    highlightCount,
					}
				}
			}

			rooms[membershipTup] = syncRoom
		}

		return nil, nil
	})

	return latestVersion, rooms, err
}

func (r *RoomsDatabase) syncRoomEventTups(
	ctx context.Context,
	options types.SyncOptions,
	membershipTup types.MembershipTup,
	res *roomSyncResult,
	paginateRoomEvents func(fdb.ReadTransaction, id.RoomID, types.PaginationOptions, *events.TxnEventsProvider) []types.EventTupWithVersion,
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

	// Grab the event IDs, most recent first unless streaming incremental
	var page []types.EventTupWithVersion
	if options.IsServerToServer {
		if res.denied = !applyServerSyncRange(res); res.denied {
			return nil
		}
	}
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		page = paginateRoomEvents(txn, membershipTup.RoomID, types.PaginationOptions{
			From:    res.from,
			To:      res.to,
			Reverse: reverse,
			Limit:   limit,
		}, nil)
		if reverse {
			// Switch the timeline back to old -> new order
			slices.Reverse(page)
		}
		return nil, nil
	})
	if err == nil {
		takeTimelinePage(options, res, page, limit)
	}
	return err
}

// applyServerSyncRange replaces a room's range in a server's sync by the one serverSyncRange gives,
// reporting false when it gives none and the room is left out.
func applyServerSyncRange(res *roomSyncResult) bool {
	room := res.server
	room.since, room.to = res.from, res.to
	from, to, ok := serverSyncRange(room)
	if ok {
		res.from, res.to = from, to
		if types.VersionIsBefore(to, room.to) {
			// Stop here if a later joined interval still needs reading, even with a short page.
			for _, change := range room.localChanges {
				if !types.VersionIsBefore(change.Version, room.to) {
					break
				}
				if change.Membership == event.MembershipJoin && types.VersionIsAfter(change.Version, to) {
					res.intervalEnd = to
					break
				}
			}
		}
	}
	return ok
}

func changesByRoom(changes types.MembershipChanges) map[id.RoomID]types.MembershipChanges {
	byRoom := make(map[id.RoomID]types.MembershipChanges)
	for _, change := range changes {
		byRoom[change.RoomID] = append(byRoom[change.RoomID], change)
	}
	return byRoom
}

// joinEntryEnds returns where the range of each of a sync's membership changes ends if it is a join:
// before the room's next change in the sync that is not a join, or at the sync's end, so no events
// after a leave are sent.
func joinEntryEnds(changes types.MembershipChanges, end tuple.Versionstamp) []tuple.Versionstamp {
	ends := make([]tuple.Versionstamp, len(changes))
	next := make(map[id.RoomID]tuple.Versionstamp)
	for i := len(changes) - 1; i >= 0; i-- {
		ends[i] = end
		if leave, found := next[changes[i].RoomID]; found {
			ends[i], _ = types.VersionstampBefore(leave)
		}
		if changes[i].Membership != event.MembershipJoin {
			next[changes[i].RoomID] = changes[i].Version
		}
	}
	return ends
}

// joinedAt reports whether a server was joined to a room at a version, from its first membership
// change of the room after it, or whether it is joined now without one
func joinedAt(version tuple.Versionstamp, changes types.MembershipChanges, joinedNow bool) bool {
	for _, change := range changes {
		if types.VersionIsAfter(change.Version, version) {
			return change.Membership != event.MembershipJoin
		}
	}
	return joinedNow
}

// Return the first (from, to] interval where both servers were joined, including our leave.
// Later rejoins are handled by subsequent syncs so we don't skip pending events before a leave.
func serverSyncRange(s serverRoomSync) (tuple.Versionstamp, tuple.Versionstamp, bool) {
	if !joinedAt(s.to, s.serverChanges, s.serverJoinedNow) {
		return s.since, s.to, false
	}
	from := s.since
	if types.VersionIsAfter(s.serverJoined, from) {
		from = s.serverJoined
	}
	localJoined := joinedAt(from, s.localChanges, s.localJoinedNow)
	for _, change := range s.localChanges {
		if types.VersionIsAfter(change.Version, s.to) {
			break
		} else if types.VersionIsAtOrBefore(change.Version, from) {
			continue
		}
		if change.Membership == event.MembershipJoin {
			if !localJoined {
				from = change.Version
			}
			localJoined = true
		} else if localJoined {
			return from, change.Version, true
		}
	}
	if localJoined && types.VersionIsBefore(from, s.to) {
		return from, s.to, true
	}
	return s.since, s.to, false
}

// takeTimelinePage makes a page of a room's events read up to limit, oldest first before applying
// any timeline filtering (so limited is accurate before filtering).
func takeTimelinePage(options types.SyncOptions, res *roomSyncResult, page []types.EventTupWithVersion, limit int) {
	if len(page) == limit && !res.isInitial {
		if options.Mode == types.SyncModeStreaming {
			res.pageEnd = page[len(page)-1].Version
		} else {
			res.limited = true
			page = page[1:]
		}
	}
	filter := options.GetTimelineFilter()
	res.eventTups = slices.DeleteFunc(page, func(tup types.EventTupWithVersion) bool {
		return filter != nil && (len(filter.Types) > 0 && !slices.Contains(filter.Types, tup.Type) ||
			len(filter.Senders) > 0 && !slices.Contains(filter.Senders, tup.Sender) ||
			slices.Contains(filter.NotTypes, tup.Type) ||
			slices.Contains(filter.NotSenders, tup.Sender))
	})
}

// Keep one result per room using the latest membership entry - ie if the user leaves/rejoins inside
// the gap we just keep the join onwards.
func filterClientSyncRooms(roomResults map[types.MembershipTup]*roomSyncResult) {
	latest := make(map[id.RoomID]types.MembershipTup, len(roomResults))
	for membershipTup, res := range roomResults {
		previous, found := latest[membershipTup.RoomID]
		if !found || types.VersionIsAfter(res.changedAt, roomResults[previous].changedAt) {
			latest[membershipTup.RoomID] = membershipTup
		}
	}
	for membershipTup := range roomResults {
		if membershipTup != latest[membershipTup.RoomID] {
			delete(roomResults, membershipTup)
		}
	}
}

func (r *RoomsDatabase) syncRoomsStateTups(
	ctx context.Context,
	options types.SyncOptions,
	roomResults map[types.MembershipTup]*roomSyncResult,
) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for membershipTup, res := range roomResults {
		if membershipTup.Membership != event.MembershipJoin {
			continue
		}
		wg.Go(func() {
			if err := r.syncRoomState(ctx, options, membershipTup.RoomID, res); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("failed to sync room state: %s: %w", membershipTup.RoomID, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *RoomsDatabase) syncRoomState(
	ctx context.Context,
	options types.SyncOptions,
	roomID id.RoomID,
	res *roomSyncResult,
) error {
	timeline := make(map[id.EventID]struct{}, len(res.eventTups))
	for _, tup := range res.eventTups {
		timeline[tup.EventID] = struct{}{}
	}
	_, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (types.Nil, error) {
		stateBatch := r.state.NewBatch(roomID)
		var stateTups []types.EventStateTup
		if res.isInitial {
			// TODO: lazy loading members
			atEnd, err := r.txnRoomStateAt(txn, roomID, res.to)
			if err != nil {
				return nil, err
			}
			stateMap, err := stateBatch.TxnIterateState(txn, atEnd)
			if err != nil {
				return nil, err
			}
			for stateTup, eventID := range stateMap {
				if _, found := timeline[eventID]; !found {
					stateTups = append(stateTups, types.EventStateTup{StateTup: stateTup, EventID: eventID})
				}
			}
			if createID := stateMap[types.StateTup{Type: event.StateCreate}]; createID != "" {
				_, inTimeline := timeline[createID]
				res.limited = !inTimeline
			}
		} else {
			atFrom, err := r.txnRoomStateAt(txn, roomID, res.from)
			if err != nil {
				return nil, err
			}
			versions := []tuple.Versionstamp{res.to}
			if len(res.eventTups) > 0 && options.Mode == types.SyncModeLegacy {
				// Legacy needs state at the timeline's start, plus changes from resolving forks
				// that aren't in the timeline. The end context supplies those missing changes.
				versions = []tuple.Versionstamp{res.eventTups[0].Version, res.to}
			}
			tos := make([]types.StateHash, len(versions))
			for i, version := range versions {
				if tos[i], err = r.txnRoomStateAt(txn, roomID, version); err != nil {
					return nil, err
				}
			}
			if stateTups, err = stateBatch.TxnStateSince(txn, atFrom, tos, timeline); err != nil {
				return nil, err
			}
		}
		var err error
		res.eventStateTups, err = r.txnWithEventVersions(txn, stateTups)
		return nil, err
	})
	return err
}

func (r *RoomsDatabase) txnRoomStateAt(txn fdb.ReadTransaction, roomID id.RoomID, version tuple.Versionstamp) (types.StateHash, error) {
	stateCtx, found, err := r.events.TxnLookupRoomStateAt(txn, roomID, version)
	if err != nil || found {
		return stateCtx, err
	}
	return state.EmptyContext, nil
}

func (r *RoomsDatabase) txnWithEventVersions(txn fdb.ReadTransaction, stateTups []types.EventStateTup) ([]types.EventStateTupWithVersion, error) {
	futures := make([]fdb.FutureByteSlice, len(stateTups))
	for i, tup := range stateTups {
		futures[i] = txn.Get(r.events.KeyForIDToVersion(tup.EventID))
	}
	withVersions := make([]types.EventStateTupWithVersion, len(stateTups))
	for i, tup := range stateTups {
		withVersions[i].EventStateTup = tup
		if b, err := futures[i].Get(); err != nil {
			return nil, err
		} else if b != nil {
			if withVersions[i].Version, err = types.BytesToVersionstamp(b); err != nil {
				return nil, err
			}
		}
	}
	return withVersions, nil
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
	from, latestVersion tuple.Versionstamp,
) tuple.Versionstamp {
	endAt := func(version tuple.Versionstamp) {
		if types.VersionIsAfter(version, from) && types.VersionIsBefore(version, latestVersion) {
			latestVersion = version
		}
	}

	timelineLimit := options.GetTimelineLimit()
	allEventTups := make([]types.EventTupWithVersion, 0, len(roomResults)*timelineLimit)
	for _, res := range roomResults {
		allEventTups = append(allEventTups, res.eventTups...)
		endAt(res.pageEnd)
		endAt(res.intervalEnd)
	}
	if len(allEventTups) > timelineLimit {
		types.SortVersioners(allEventTups)
		endAt(allEventTups[timelineLimit-1].Version)
	}

	receiptsLimit := options.GetReceiptsLimit()
	allReceipts := make([]*types.ReceiptWithVersion, 0, len(roomResults)*receiptsLimit)
	for _, res := range roomResults {
		allReceipts = append(allReceipts, res.receipts...)
	}
	if len(allReceipts) > receiptsLimit {
		types.SortVersioners(allReceipts)
		endAt(allReceipts[receiptsLimit-1].Version)
	}

	// Now go through each room result and remove events + receipts ahead of the version
	for membershipTup, res := range roomResults {
		if types.VersionIsAfter(res.changedAt, latestVersion) {
			delete(roomResults, membershipTup)
			continue
		}
		if types.VersionIsAfter(res.to, latestVersion) {
			res.to = latestVersion
		}
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
