package rooms

import (
	"maps"
	"slices"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func syncTestVersion(transaction byte, user uint16) tuple.Versionstamp {
	return tuple.Versionstamp{TransactionVersion: [10]byte{9: transaction}, UserVersion: user}
}

func syncTestEvent(eventID id.EventID, evType event.Type, version tuple.Versionstamp) types.EventTupWithVersion {
	return types.EventTupWithVersion{EventTup: types.EventTup{EventID: eventID, Type: evType}, Version: version}
}

func streamingTestOptions(limit int) types.SyncOptions {
	return types.SyncOptions{
		Mode: types.SyncModeStreaming,
		Filter: &mautrix.Filter{Room: &mautrix.RoomFilter{
			Timeline:  &mautrix.FilterPart{Limit: limit},
			Ephemeral: &mautrix.FilterPart{},
		}},
	}
}

func TestTakeTimelinePageTellsAFullPageBeforeFiltering(t *testing.T) {
	page := func() []types.EventTupWithVersion {
		return []types.EventTupWithVersion{
			syncTestEvent("$one", event.EventMessage, syncTestVersion(1, 0)),
			syncTestEvent("$filtered", event.NewEventType("please_filter_me"), syncTestVersion(2, 0)),
			syncTestEvent("$three", event.EventMessage, syncTestVersion(3, 0)),
		}
	}
	filter := &mautrix.Filter{Room: &mautrix.RoomFilter{Timeline: &mautrix.FilterPart{
		NotTypes: []event.Type{event.NewEventType("please_filter_me")},
	}}}

	streaming := &roomSyncResult{roomSyncConfig: &roomSyncConfig{}}
	takeTimelinePage(types.SyncOptions{Mode: types.SyncModeStreaming, Filter: filter}, streaming, page(), 3)
	assert.Equal(t, syncTestVersion(3, 0), streaming.pageEnd)
	assert.Equal(t, []types.EventTupWithVersion{page()[0], page()[2]}, streaming.eventTups)

	legacy := &roomSyncResult{roomSyncConfig: &roomSyncConfig{}}
	takeTimelinePage(types.SyncOptions{Mode: types.SyncModeLegacy, Filter: filter}, legacy, page(), 3)
	assert.True(t, legacy.limited)
	assert.Equal(t, []types.EventTupWithVersion{page()[2]}, legacy.eventTups, "the event read to tell a limited timeline is dropped")

	short := &roomSyncResult{roomSyncConfig: &roomSyncConfig{}}
	takeTimelinePage(types.SyncOptions{Mode: types.SyncModeStreaming}, short, page(), 4)
	assert.Equal(t, types.ZeroVersionstamp, short.pageEnd)
	assert.Equal(t, page(), short.eventTups)

	initial := &roomSyncResult{roomSyncConfig: &roomSyncConfig{isInitial: true}}
	takeTimelinePage(types.SyncOptions{Mode: types.SyncModeLegacy}, initial, page(), 3)
	assert.False(t, initial.limited)
	assert.Equal(t, types.ZeroVersionstamp, initial.pageEnd)
	assert.Equal(t, page(), initial.eventTups)
}

func TestFilterStreamingSyncTupsEndsRoomsAtTheTrimmedVersion(t *testing.T) {
	from := syncTestVersion(0, types.MaxVersionstampUserVersion)
	latest := syncTestVersion(9, types.MaxVersionstampUserVersion)
	joined := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{from: from, to: latest},
		eventTups: []types.EventTupWithVersion{
			{Version: syncTestVersion(1, 0)}, {Version: syncTestVersion(3, 0)}, {Version: syncTestVersion(5, 0)},
		},
	}
	newlyJoined := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{to: latest, isInitial: true},
		eventTups:      []types.EventTupWithVersion{{Version: syncTestVersion(2, 1)}},
	}
	left := &roomSyncResult{roomSyncConfig: &roomSyncConfig{to: syncTestVersion(1, 2)}}
	options := streamingTestOptions(2)

	trimmed := (&RoomsDatabase{}).filterStreamingSyncTups(options, map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!joined:example.com"}:      joined,
		{RoomID: "!newlyjoined:example.com"}: newlyJoined,
		{RoomID: "!left:example.com"}:        left,
	}, from, latest)

	assert.Equal(t, syncTestVersion(2, 1), trimmed)
	assert.Equal(t, []types.EventTupWithVersion{{Version: syncTestVersion(1, 0)}}, joined.eventTups)
	assert.Equal(t, trimmed, joined.to)
	assert.Equal(t, trimmed, newlyJoined.to)
	assert.Equal(t, syncTestVersion(1, 2), left.to, "a room ending before the trimmed version keeps its end")
}

func TestFilterStreamingSyncTupsEndsAtAFullPage(t *testing.T) {
	from := syncTestVersion(0, types.MaxVersionstampUserVersion)
	latest := syncTestVersion(9, types.MaxVersionstampUserVersion)
	full := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{from: from, to: latest},
		eventTups:      []types.EventTupWithVersion{{Version: syncTestVersion(1, 0)}, {Version: syncTestVersion(3, 0)}},
		pageEnd:        syncTestVersion(4, 0),
	}

	trimmed := (&RoomsDatabase{}).filterStreamingSyncTups(streamingTestOptions(3), map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!full:example.com"}: full,
	}, from, latest)

	assert.Equal(t, syncTestVersion(4, 0), trimmed, "a full page within the limit still ends the sync")
	assert.Equal(t, trimmed, full.to)
	assert.Len(t, full.eventTups, 2)
}

func TestFilterStreamingSyncTupsNeverEndsAtOrBeforeSince(t *testing.T) {
	from := syncTestVersion(5, types.MaxVersionstampUserVersion)
	latest := syncTestVersion(9, types.MaxVersionstampUserVersion)
	receipts := make([]*types.ReceiptWithVersion, 3)
	for i := range receipts {
		receipts[i] = &types.ReceiptWithVersion{Version: syncTestVersion(byte(1+i), 0)}
	}
	newlyJoined := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{to: latest, isInitial: true},
		eventTups:      []types.EventTupWithVersion{{Version: syncTestVersion(6, 0)}},
		receipts:       receipts,
	}
	options := streamingTestOptions(5)
	options.Filter.Room.Ephemeral.Limit = 2

	trimmed := (&RoomsDatabase{}).filterStreamingSyncTups(options, map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!newlyjoined:example.com"}: newlyJoined,
	}, from, latest)

	assert.Equal(t, latest, trimmed, "a newly joined room's receipts from before since do not end the sync")
	assert.Len(t, newlyJoined.receipts, 3)
}

func TestFilterStreamingSyncTupsLeavesLaterMembershipChangesToTheNextSync(t *testing.T) {
	from := syncTestVersion(0, types.MaxVersionstampUserVersion)
	latest := syncTestVersion(9, types.MaxVersionstampUserVersion)
	joined := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{from: from, to: latest},
		eventTups:      []types.EventTupWithVersion{{Version: syncTestVersion(1, 0)}, {Version: syncTestVersion(3, 0)}, {Version: syncTestVersion(5, 0)}},
	}
	invitedBefore := &roomSyncResult{roomSyncConfig: &roomSyncConfig{changedAt: syncTestVersion(2, 0)}}
	invitedAfter := &roomSyncResult{roomSyncConfig: &roomSyncConfig{changedAt: syncTestVersion(4, 0)}}
	leftAfter := &roomSyncResult{roomSyncConfig: &roomSyncConfig{from: from, to: syncTestVersion(6, 0), changedAt: syncTestVersion(6, 0)}}
	joinedAfter := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{to: latest, isInitial: true, changedAt: syncTestVersion(7, 0)},
		eventTups:      []types.EventTupWithVersion{{Version: syncTestVersion(7, 0)}},
	}
	results := map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!joined:example.com", Membership: event.MembershipJoin}:          joined,
		{RoomID: "!invitedbefore:example.com", Membership: event.MembershipInvite}: invitedBefore,
		{RoomID: "!invitedafter:example.com", Membership: event.MembershipInvite}:  invitedAfter,
		{RoomID: "!leftafter:example.com", Membership: event.MembershipLeave}:      leftAfter,
		{RoomID: "!joinedafter:example.com", Membership: event.MembershipJoin}:     joinedAfter,
	}

	trimmed := (&RoomsDatabase{}).filterStreamingSyncTups(streamingTestOptions(2), results, from, latest)

	assert.Equal(t, syncTestVersion(3, 0), trimmed)
	assert.ElementsMatch(t, []*roomSyncResult{joined, invitedBefore}, slices.Collect(maps.Values(results)),
		"a room changed after the trimmed end, its join included, is left to the next sync")
}

func TestFilterClientSyncRoomsSelectsMembershipAtResponseEnd(t *testing.T) {
	from, latest := syncTestVersion(1, 0), syncTestVersion(9, 0)
	changes := types.MembershipChanges{
		syncTestChange(syncTestVersion(2, 0), event.MembershipJoin),
		syncTestChange(syncTestVersion(4, 0), event.MembershipLeave),
		syncTestChange(syncTestVersion(6, 0), event.MembershipJoin),
	}
	changes[0].EventID, changes[1].EventID, changes[2].EventID = "$join1", "$leave", "$join2"
	ends := joinEntryEnds(changes, latest)
	for _, tc := range []struct {
		name string
		mode types.SyncMode
		end  byte
		want int
	}{
		{"legacy rejoin", types.SyncModeLegacy, 9, 2},
		{"streaming rejoin", types.SyncModeStreaming, 9, 2},
		{"before leave", types.SyncModeStreaming, 3, 0},
		{"at leave", types.SyncModeStreaming, 4, 1},
		{"before rejoin", types.SyncModeStreaming, 5, 1},
		{"at rejoin", types.SyncModeStreaming, 6, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := make(map[types.MembershipTup]*roomSyncResult)
			for i, change := range changes {
				res := &roomSyncResult{roomSyncConfig: &roomSyncConfig{
					from: from, to: change.Version, changedAt: change.Version,
				}}
				if change.Membership == event.MembershipJoin {
					res.from, _ = types.VersionstampBefore(change.Version)
					res.to, res.isInitial = ends[i], true
					res.eventTups = []types.EventTupWithVersion{syncTestEvent(change.EventID, event.StateMember, change.Version)}
				}
				results[change.MembershipTup] = res
			}
			wantMembership := changes[tc.want].MembershipTup
			wantResult := results[wantMembership]
			otherMembership := types.MembershipTup{RoomID: "!other:example.com", Membership: event.MembershipJoin}
			other := &roomSyncResult{
				roomSyncConfig: &roomSyncConfig{from: from, to: latest},
				pageEnd:        syncTestVersion(tc.end, 0),
			}
			results[otherMembership] = other

			end := latest
			if tc.mode == types.SyncModeStreaming {
				end = (&RoomsDatabase{}).filterStreamingSyncTups(streamingTestOptions(10), results, from, latest)
			}
			filterClientSyncRooms(results)

			assert.Equal(t, syncTestVersion(tc.end, 0), end)
			require.Len(t, results, 2, "only the selected membership and the unchanged room remain")
			require.Same(t, wantResult, results[wantMembership])
			assert.Same(t, other, results[otherMembership])
			if wantMembership.Membership == event.MembershipJoin {
				assert.Equal(t, end, wantResult.to, "state is read at the response end")
				assert.Equal(t, changes[tc.want].EventID, wantResult.eventTups[0].EventID, "the selected join keeps its timeline")
			}

			// Exercise response assembly too: the room must appear in exactly one membership section.
			rooms := make(map[types.MembershipTup]*types.SyncRoom)
			for membership := range results {
				rooms[membership] = &types.SyncRoom{}
			}
			response := types.NewSync(rooms, nil, nil, nil)
			if wantMembership.Membership == event.MembershipJoin {
				assert.Same(t, rooms[wantMembership], response.Rooms.Join[wantMembership.RoomID])
				assert.NotContains(t, response.Rooms.Leave, wantMembership.RoomID)
			} else {
				assert.Same(t, rooms[wantMembership], response.Rooms.Leave[wantMembership.RoomID])
				assert.NotContains(t, response.Rooms.Join, wantMembership.RoomID)
			}
		})
	}
}

func syncTestChange(version tuple.Versionstamp, membership event.Membership) types.MembershipTupWithVersion {
	return types.MembershipTupWithVersion{MembershipTup: types.MembershipTup{RoomID: "!room:example.com", Membership: membership}, Version: version}
}

func TestServerSyncRangeIsWhileBothServersWereInTheRoom(t *testing.T) {
	since, to := syncTestVersion(5, 0), syncTestVersion(9, 0)
	v := func(transaction byte) tuple.Versionstamp { return syncTestVersion(transaction, 0) }
	for _, tc := range []struct {
		name             string
		sync             serverRoomSync
		wantFrom, wantTo tuple.Versionstamp
		wantSynced       bool
	}{
		{
			name:     "both joined since before since",
			sync:     serverRoomSync{since: since, to: to, serverJoinedNow: true, localJoinedNow: true},
			wantFrom: since, wantTo: to, wantSynced: true,
		},
		{
			name:     "the server joined within the sync",
			sync:     serverRoomSync{since: since, to: to, serverJoined: v(6), serverJoinedNow: true, localJoinedNow: true},
			wantFrom: v(6), wantTo: to, wantSynced: true,
		},
		{
			name: "this server joined within the sync, the server's join unrecorded",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true, localJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(v(7), event.MembershipJoin)},
			},
			wantFrom: v(7), wantTo: to, wantSynced: true,
		},
		{
			name: "this server left within the sync",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(v(6), event.MembershipJoin), syncTestChange(v(7), event.MembershipLeave)},
			},
			wantFrom: v(6), wantTo: v(7), wantSynced: true,
		},
		{
			name: "this server left and rejoined with events still pending before the leave",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true, localJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(v(7), event.MembershipLeave), syncTestChange(v(8), event.MembershipJoin)},
			},
			wantFrom: since, wantTo: v(7), wantSynced: true,
		},
		{
			name: "resume at the leave and read the next joined interval",
			sync: serverRoomSync{
				since: v(7), to: to, serverJoinedNow: true, localJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(v(8), event.MembershipJoin)},
			},
			wantFrom: v(8), wantTo: to, wantSynced: true,
		},
		{
			name: "first join and leave win over a later rejoin",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true, localJoinedNow: true,
				localChanges: types.MembershipChanges{
					syncTestChange(v(6), event.MembershipJoin), syncTestChange(v(7), event.MembershipLeave), syncTestChange(v(8), event.MembershipJoin),
				},
			},
			wantFrom: v(6), wantTo: v(7), wantSynced: true,
		},
		{
			name: "this server left, rejoined while the server joined and left again",
			sync: serverRoomSync{
				since: since, to: to, serverJoined: v(7), serverJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(v(6), event.MembershipLeave), syncTestChange(v(7), event.MembershipJoin), syncTestChange(v(8), event.MembershipLeave)},
			},
			wantFrom: v(7), wantTo: v(8), wantSynced: true,
		},
		{
			name: "this server left after the sync's end",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(syncTestVersion(10, 0), event.MembershipLeave)},
			},
			wantFrom: since, wantTo: to, wantSynced: true,
		},
		{
			name: "out of the room for the whole sync",
			sync: serverRoomSync{since: since, to: to, serverJoinedNow: true},
		},
		{
			name: "this server left before since",
			sync: serverRoomSync{since: since, to: to, serverJoinedNow: true, localChanges: types.MembershipChanges{}},
		},
		{
			name: "the server left after the range's end",
			sync: serverRoomSync{
				since: since, to: to, localJoinedNow: true,
				serverChanges: types.MembershipChanges{syncTestChange(syncTestVersion(10, 0), event.MembershipLeave)},
			},
			wantFrom: since, wantTo: to, wantSynced: true,
		},
		{
			name: "the server is not in the room",
			sync: serverRoomSync{since: since, to: to, localJoinedNow: true},
		},
		{
			name: "the server joined after the range's end",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true, localJoinedNow: true,
				serverChanges: types.MembershipChanges{syncTestChange(syncTestVersion(10, 0), event.MembershipJoin)},
			},
		},
		{
			name: "this server joins exactly at the end with nothing after it",
			sync: serverRoomSync{
				since: since, to: to, serverJoinedNow: true, localJoinedNow: true,
				localChanges: types.MembershipChanges{syncTestChange(to, event.MembershipJoin)},
			},
		},
	} {
		from, to, synced := serverSyncRange(tc.sync)
		assert.Equal(t, tc.wantSynced, synced, tc.name)
		if synced {
			assert.Equal(t, tc.wantFrom, from, tc.name)
			assert.Equal(t, tc.wantTo, to, tc.name)
			assert.False(t, types.VersionIsBefore(from, tc.sync.since), "%s: never before since", tc.name)
		}
	}
}

func TestServerSyncResumesJoinedIntervalsWithoutSkippingEventsOrReceipts(t *testing.T) {
	v := func(transaction byte) tuple.Versionstamp { return syncTestVersion(transaction, 0) }
	latest := v(10)
	localChanges := types.MembershipChanges{
		syncTestChange(v(7), event.MembershipLeave), syncTestChange(v(8), event.MembershipJoin),
	}
	allEvents := []types.EventTupWithVersion{
		syncTestEvent("$pending", event.EventMessage, v(6)),
		syncTestEvent("$leave", event.StateMember, v(7)),
		syncTestEvent("$gap", event.EventMessage, syncTestVersion(7, 1)),
		syncTestEvent("$rejoin", event.StateMember, v(8)),
		syncTestEvent("$after", event.EventMessage, v(9)),
	}
	allReceipts := []*types.ReceiptWithVersion{
		{Version: syncTestVersion(6, 1)},
		{Version: syncTestVersion(6, 2)},
		{Version: syncTestVersion(7, 2)},
		{Version: syncTestVersion(8, 1)},
	}
	for _, tc := range []struct {
		name         string
		eventLimit   int
		receiptLimit int
		firstEnd     tuple.Versionstamp
	}{
		{"short page", 10, 10, v(7)},
		{"full page", 2, 10, v(7)},
		{"page stops before leave", 1, 10, v(6)},
		{"receipt limit stops before leave", 10, 1, syncTestVersion(6, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := streamingTestOptions(tc.eventLimit)
			options.IsServerToServer = true
			options.Filter.Room.Ephemeral.Limit = tc.receiptLimit
			room := types.MembershipTup{RoomID: "!room:example.com", Membership: event.MembershipJoin}
			var events []id.EventID
			var receipts []tuple.Versionstamp
			cursor := v(5)
			for attempt := 0; cursor != latest; attempt++ {
				require.Less(t, attempt, 10, "catch-up must finish")
				res := &roomSyncResult{roomSyncConfig: &roomSyncConfig{
					from: cursor, to: latest, includeReceipts: true,
					server: serverRoomSync{
						serverJoinedNow: true, localJoinedNow: true,
						localChanges: slices.DeleteFunc(slices.Clone(localChanges), func(change types.MembershipTupWithVersion) bool {
							return types.VersionIsAtOrBefore(change.Version, cursor)
						}),
					},
				}}
				results := map[types.MembershipTup]*roomSyncResult{room: res}
				if applyServerSyncRange(res) {
					var page []types.EventTupWithVersion
					for _, ev := range allEvents {
						if types.VersionIsAfter(ev.Version, res.from) && types.VersionIsAtOrBefore(ev.Version, res.to) {
							page = append(page, ev)
							if len(page) == tc.eventLimit {
								break
							}
						}
					}
					takeTimelinePage(options, res, page, tc.eventLimit)
					for _, receipt := range allReceipts {
						if types.VersionIsAfter(receipt.Version, res.from) && types.VersionIsAtOrBefore(receipt.Version, res.to) {
							res.receipts = append(res.receipts, receipt)
						}
					}
				} else {
					delete(results, room)
				}
				next := (&RoomsDatabase{}).filterStreamingSyncTups(options, results, cursor, latest)
				if attempt == 0 {
					assert.Equal(t, tc.firstEnd, next)
				}
				require.True(t, types.VersionIsAfter(next, cursor), "the delivery cursor must advance")
				for _, ev := range res.eventTups {
					events = append(events, ev.EventID)
				}
				for _, receipt := range res.receipts {
					receipts = append(receipts, receipt.Version)
				}
				cursor = next
			}
			assert.Equal(t, []id.EventID{"$pending", "$leave", "$after"}, events,
				"deliver both joined intervals once, excluding the gap and the join already exchanged")
			assert.Equal(t, []tuple.Versionstamp{syncTestVersion(6, 1), syncTestVersion(6, 2), syncTestVersion(8, 1)}, receipts)
		})
	}
}

func TestServerSyncEmptyIntervalStillBoundsOtherRooms(t *testing.T) {
	from, latest := syncTestVersion(5, 0), syncTestVersion(10, 0)
	leave := syncTestVersion(7, 0)
	res := &roomSyncResult{roomSyncConfig: &roomSyncConfig{
		from: from, to: latest,
		server: serverRoomSync{
			serverJoinedNow: true, localJoinedNow: true,
			localChanges: types.MembershipChanges{
				syncTestChange(leave, event.MembershipLeave), syncTestChange(syncTestVersion(8, 0), event.MembershipJoin),
			},
		},
	}}
	require.True(t, applyServerSyncRange(res))
	options := streamingTestOptions(10)
	options.IsServerToServer = true
	takeTimelinePage(options, res, nil, 10)
	other := &roomSyncResult{
		roomSyncConfig: &roomSyncConfig{from: from, to: latest},
		eventTups: []types.EventTupWithVersion{
			syncTestEvent("$before", event.EventMessage, syncTestVersion(6, 0)),
			syncTestEvent("$later", event.EventMessage, syncTestVersion(9, 0)),
		},
		receipts: []*types.ReceiptWithVersion{{Version: syncTestVersion(9, 1)}},
	}
	results := map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!room:example.com", Membership: event.MembershipJoin}:  res,
		{RoomID: "!other:example.com", Membership: event.MembershipJoin}: other,
	}
	next := (&RoomsDatabase{}).filterStreamingSyncTups(options, results, from, latest)
	assert.Equal(t, leave, next, "an empty local-event page must not skip the next joined interval")
	assert.Equal(t, leave, other.to)
	require.Len(t, other.eventTups, 1)
	assert.Equal(t, id.EventID("$before"), other.eventTups[0].EventID)
	assert.Empty(t, other.receipts)
}

func TestServerSyncLastIntervalDoesNotHoldBackTheCursor(t *testing.T) {
	from, latest := syncTestVersion(5, 0), syncTestVersion(10, 0)
	leave := syncTestVersion(7, 0)
	res := &roomSyncResult{roomSyncConfig: &roomSyncConfig{
		from: from, to: latest,
		server: serverRoomSync{
			serverJoinedNow: true,
			localChanges:    types.MembershipChanges{syncTestChange(leave, event.MembershipLeave)},
		},
	}}
	require.True(t, applyServerSyncRange(res))
	assert.Equal(t, leave, res.to)
	assert.Equal(t, types.ZeroVersionstamp, res.intervalEnd)
	options := streamingTestOptions(10)
	options.IsServerToServer = true
	takeTimelinePage(options, res, []types.EventTupWithVersion{syncTestEvent("$leave", event.StateMember, leave)}, 10)
	next := (&RoomsDatabase{}).filterStreamingSyncTups(options, map[types.MembershipTup]*roomSyncResult{
		{RoomID: "!room:example.com", Membership: event.MembershipJoin}: res,
	}, from, latest)
	assert.Equal(t, latest, next, "no later joined interval remains to be read")
}

func TestServerSyncRangeOfAServerTheRejoinRemoved(t *testing.T) {
	since, latest := syncTestVersion(1, 0), syncTestVersion(9, 0)
	vJ, vL1, vJ2, vJ3, vE := syncTestVersion(2, 0), syncTestVersion(3, 0), syncTestVersion(4, 0), syncTestVersion(5, 0), syncTestVersion(6, 0)
	// The server joins, this server's last user leaves, the server's users leave meanwhile, this
	// server rejoins and its join's publish removes the server, which rejoins through it and leaves
	serverChanges := types.MembershipChanges{
		syncTestChange(vJ, event.MembershipJoin), syncTestChange(vJ2, event.MembershipLeave), syncTestChange(vJ3, event.MembershipJoin), syncTestChange(vE, event.MembershipLeave),
	}
	localChanges := types.MembershipChanges{syncTestChange(vL1, event.MembershipLeave), syncTestChange(vJ2, event.MembershipJoin)}
	ends := joinEntryEnds(serverChanges, latest)

	var ranges [][2]tuple.Versionstamp
	for i, change := range serverChanges {
		if change.Membership != event.MembershipJoin {
			continue
		}
		from, to, synced := serverSyncRange(serverRoomSync{
			since: since, to: ends[i], serverJoined: change.Version,
			serverChanges: serverChanges, localChanges: localChanges, localJoinedNow: true,
		})
		require.True(t, synced, change.Version)
		ranges = append(ranges, [2]tuple.Versionstamp{from, to})
	}
	beforeLeave, _ := types.VersionstampBefore(vE)
	assert.Equal(t, [][2]tuple.Versionstamp{{vJ, vL1}, {vJ3, beforeLeave}}, ranges, "never this server's events from when the server was out of the room")
}

func TestJoinEntriesEndBeforeTheRoomsNextOtherChange(t *testing.T) {
	end := syncTestVersion(9, types.MaxVersionstampUserVersion)
	change := func(roomID id.RoomID, transaction byte, membership event.Membership) types.MembershipTupWithVersion {
		return types.MembershipTupWithVersion{
			MembershipTup: types.MembershipTup{RoomID: roomID, Membership: membership},
			Version:       syncTestVersion(transaction, 0),
		}
	}
	changes := types.MembershipChanges{
		change("!a:example.com", 1, event.MembershipJoin),
		change("!b:example.com", 2, event.MembershipJoin),
		change("!a:example.com", 3, event.MembershipJoin),
		change("!a:example.com", 4, event.MembershipLeave),
		change("!a:example.com", 5, event.MembershipJoin),
		change("!b:example.com", 6, event.MembershipBan),
	}
	before := func(version tuple.Versionstamp) tuple.Versionstamp {
		previous, _ := types.VersionstampBefore(version)
		return previous
	}
	assert.Equal(t, []tuple.Versionstamp{
		before(syncTestVersion(4, 0)),
		before(syncTestVersion(6, 0)),
		before(syncTestVersion(4, 0)),
		end,
		end,
		end,
	}, joinEntryEnds(changes, end), "a join's entry ends before the leave, another join of the room not ending it")
}
