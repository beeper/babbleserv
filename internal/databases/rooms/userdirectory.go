package rooms

import (
	"context"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// Candidates of a directory search decided together, their lookups in a room made in one batch
const directoryCandidateChunk = 50

// FilterUserDirectoryCandidates returns, in order, up to maxResults of the candidates the requester
// may see and whether the search was limited.
func (r *RoomsDatabase) FilterUserDirectoryCandidates(
	ctx context.Context,
	requester id.UserID,
	candidates []*types.UserDirectoryCandidate,
	maxResults int,
	maxLookups int,
	maxLookupsPerUser int,
) ([]*types.UserDirectoryCandidate, bool, error) {
	type result struct {
		candidates []*types.UserDirectoryCandidate
		limited    bool
	}
	res, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (result, error) {
		requesterRows, limited := r.users.TxnLookupUserMembershipRows(txn, requester, maxLookups)
		var joinedIDs []id.RoomID
		for _, membership := range requesterRows {
			if membership.Membership == event.MembershipJoin {
				joinedIDs = append(joinedIDs, membership.RoomID)
			}
		}
		joined, err := r.txnGetRooms(txn, joinedIDs)
		if err != nil {
			return result{}, err
		}
		f := &directoryFilter{
			isLocal: r.isLocalUser,
			shared:  slices.DeleteFunc(joined, func(room *types.Room) bool { return room == nil }),
			budget:  directoryBudget{left: maxLookups, perUser: maxLookupsPerUser, used: make(map[id.UserID]int)},
			rows: func(userID id.UserID, limit int) ([]types.MembershipTup, bool) {
				return r.users.TxnLookupUserMembershipRows(txn, userID, limit)
			},
			rooms: func(roomIDs []id.RoomID) ([]*types.Room, error) {
				return r.txnGetRooms(txn, roomIDs)
			},
			members: func(room *types.Room, userIDs []id.UserID) (types.StateEntries, error) {
				return r.txnRoomMembers(txn, room, userIDs)
			},
		}

		visible := make([]*types.UserDirectoryCandidate, 0, min(maxResults+1, len(candidates)))
		for chunk := range slices.Chunk(candidates, directoryCandidateChunk) {
			userIDs := make([]id.UserID, 0, len(chunk))
			for _, candidate := range chunk {
				if candidate.UserID != requester {
					userIDs = append(userIDs, candidate.UserID)
				}
			}
			found, undecided, err := f.visible(userIDs)
			if err != nil {
				return result{}, err
			}
			for _, candidate := range chunk {
				eventID, ok := found[candidate.UserID]
				if !ok {
					limited = limited || undecided[candidate.UserID]
					continue
				}
				visible = append(visible, r.txnWithMemberProfile(txn, candidate, eventID))
				if len(visible) > maxResults {
					return result{visible[:maxResults], true}, nil
				}
			}
		}
		return result{visible, limited}, nil
	})
	return res.candidates, res.limited, err
}

// txnGetRooms reads the records of the rooms together, nil for a room without one
func (r *RoomsDatabase) txnGetRooms(txn fdb.ReadTransaction, roomIDs []id.RoomID) ([]*types.Room, error) {
	futures := make([]fdb.FutureByteSlice, len(roomIDs))
	for i, roomID := range roomIDs {
		futures[i] = txn.Get(r.KeyForRoom(roomID))
	}
	rooms := make([]*types.Room, len(roomIDs))
	for i, future := range futures {
		roomBytes, err := future.Get()
		if err != nil {
			return nil, err
		} else if roomBytes == nil {
			continue
		} else if rooms[i], err = types.NewRoomFromBytes(roomBytes, roomIDs[i]); err != nil {
			return nil, err
		}
	}
	return rooms, nil
}

// txnWithMemberProfile returns a copy of a remote candidate missing a display name or avatar with
// them taken from the member event that made them visible, keeping room-specific fallbacks out of the
// global index and of inputs reused if the read transaction retries.
func (r *RoomsDatabase) txnWithMemberProfile(txn fdb.ReadTransaction, candidate *types.UserDirectoryCandidate, eventID id.EventID) *types.UserDirectoryCandidate {
	result := *candidate
	if r.isLocalUser(candidate.UserID) || (candidate.DisplayName != "" && candidate.AvatarURL != "") {
		return &result
	}
	if ev := r.events.TxnGetEvent(txn, eventID); ev != nil {
		name := gjson.GetBytes(ev.Content, "displayname")
		if result.DisplayName == "" && name.Type == gjson.String && utf8.ValidString(name.Str) &&
			utf8.RuneCountInString(name.Str) <= 255 && len(name.Str) <= 1024 {
			result.DisplayName = name.Str
		}
		avatar := gjson.GetBytes(ev.Content, "avatar_url")
		if result.AvatarURL == "" && avatar.Type == gjson.String && len(avatar.Str) <= 4096 {
			result.AvatarURL = avatar.Str
		}
	}
	return &result
}

// directoryBudget bounds the membership lookups of one directory search: left in all, and perUser
// for one candidate
type directoryBudget struct {
	left, perUser int
	used          map[id.UserID]int
}

// allows returns how many more lookups the candidate may make
func (b *directoryBudget) allows(userID id.UserID) int {
	return max(min(b.left, b.perUser-b.used[userID]), 0)
}

func (b *directoryBudget) take(userID id.UserID, n int) {
	b.left -= n
	b.used[userID] += n
}

// directoryFilter decides which candidates of a directory search the requester sees: those joined
// in a room the requester is joined to, and local users whose rows hold them joined to a room whose
// join rule is public or history visibility world-readable. Local users' rows are exact, so they
// decide both for a local candidate. A remote candidate has no rows, so is looked up in the current
// state of each of the requester's rooms, and one in public rooms the requester shares none of is
// not found.
type directoryFilter struct {
	isLocal func(id.UserID) bool
	// The rooms the requester is joined to
	shared  []*types.Room
	budget  directoryBudget
	rows    func(userID id.UserID, limit int) ([]types.MembershipTup, bool)
	rooms   func(roomIDs []id.RoomID) ([]*types.Room, error)
	members func(room *types.Room, userIDs []id.UserID) (types.StateEntries, error)
}

// visible returns the member event making each visible candidate so, and the candidates the budget
// left undecided.
func (f *directoryFilter) visible(userIDs []id.UserID) (map[id.UserID]id.EventID, map[id.UserID]bool, error) {
	found := make(map[id.UserID]id.EventID)
	undecided := make(map[id.UserID]bool)
	sharedIDs := make(map[id.RoomID]bool, len(f.shared))
	for _, room := range f.shared {
		sharedIDs[room.ID] = true
	}
	var remote []id.UserID
	for _, userID := range userIDs {
		if !f.isLocal(userID) {
			remote = append(remote, userID)
			continue
		}
		eventID, decided, err := f.localVisible(userID, sharedIDs)
		if err != nil {
			return nil, nil, err
		} else if eventID != "" {
			found[userID] = eventID
		} else if !decided {
			undecided[userID] = true
		}
	}

	looked := make(map[id.UserID]int, len(remote))
	for _, room := range f.shared {
		var lookup []id.UserID
		for _, userID := range remote {
			if _, ok := found[userID]; !ok && f.budget.allows(userID) > 0 {
				f.budget.take(userID, 1)
				lookup = append(lookup, userID)
				looked[userID]++
			}
		}
		if len(lookup) == 0 {
			break
		}
		members, err := f.members(room, lookup)
		if err != nil {
			return nil, nil, err
		}
		for _, userID := range lookup {
			if member := members[types.MemberStateTup(userID)]; member.Membership == event.MembershipJoin {
				found[userID] = member.EventID
			}
		}
	}
	for _, userID := range remote {
		if _, ok := found[userID]; !ok && looked[userID] < len(f.shared) {
			undecided[userID] = true
		}
	}
	return found, undecided, nil
}

// localVisible returns the event of a local candidate's join row making them visible, and whether
// their rows decided it, as they did unless the budget cut them short. One row of the budget is kept
// for the read telling whether there are more.
func (f *directoryFilter) localVisible(userID id.UserID, sharedIDs map[id.RoomID]bool) (id.EventID, bool, error) {
	limit := f.budget.allows(userID) - 1
	if limit <= 0 {
		return "", false, nil
	}
	memberships, more := f.rows(userID, limit)
	f.budget.take(userID, len(memberships))
	if more {
		f.budget.take(userID, 1)
	}
	var joined []types.MembershipTup
	for _, membership := range memberships {
		if membership.Membership != event.MembershipJoin {
			continue
		} else if sharedIDs[membership.RoomID] {
			return membership.EventID, true, nil
		}
		joined = append(joined, membership)
	}
	roomIDs := make([]id.RoomID, len(joined))
	for i, membership := range joined {
		roomIDs[i] = membership.RoomID
	}
	rooms, err := f.rooms(roomIDs)
	if err != nil {
		return "", false, err
	}
	for i, room := range rooms {
		if room != nil && (room.JoinRule == string(event.JoinRulePublic) || room.HistoryVisibility == string(event.HistoryVisibilityWorldReadable)) {
			return joined[i].EventID, true, nil
		}
	}
	return "", !more, nil
}

// DiscoverRemoteDirectoryUsersForEvents returns the remote users whose join in this batch is still
// their accepted member event in current state, so stale iterator work schedules no profile fetch.
//
// It also returns the rooms of the batch's remote joins of local users that
// this server is joined to, once each. A remote join's response events are
// staged without entering the index of all events, so the iterator never sees
// them: every remote member of those rooms is indexed instead, see
// RoomRemoteDirectorySources, whatever the joiner's membership is by now.
func (r *RoomsDatabase) DiscoverRemoteDirectoryUsersForEvents(
	ctx context.Context,
	eventTups []types.EventTupWithVersion,
) ([]types.RemoteUserDirectorySource, []id.RoomID, error) {
	type discovered struct {
		sources     []types.RemoteUserDirectorySource
		joinedRooms []id.RoomID
	}
	res, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (discovered, error) {
		var joinEvs []*types.Event
		remoteJoinRooms := make(map[id.RoomID]struct{})
		roomUsers := make(map[id.RoomID][]id.UserID)
		for _, eventTup := range eventTups {
			if eventTup.Type.String() != event.StateMember.Type {
				continue
			}
			ev := r.events.TxnGetEvent(txn, eventTup.EventID)
			if ev == nil || ev.Rejected || ev.SoftFailed || ev.Outlier || ev.StateKey == nil ||
				ev.RoomID != eventTup.RoomID || !isJoinEvent(ev) {
				continue
			}
			userID := id.UserID(*ev.StateKey)
			if userID.Homeserver() == r.config.ServerName {
				// The resident server sends a remote join, so it is not local
				if !ev.Local {
					remoteJoinRooms[ev.RoomID] = struct{}{}
				}
				continue
			}
			if _, _, err := userID.ParseAndValidateRelaxed(); err != nil {
				continue
			}
			joinEvs = append(joinEvs, ev)
			roomUsers[ev.RoomID] = append(roomUsers[ev.RoomID], userID)
		}
		currentMembers := make(map[id.RoomID]types.StateEntries, len(roomUsers))
		rooms := make(map[id.RoomID]*types.Room, len(roomUsers))
		for roomID, userIDs := range roomUsers {
			room, err := roomOrNil(r.txnGetRoom(txn, roomID))
			if err != nil {
				return discovered{}, err
			} else if currentMembers[roomID], err = r.txnRoomMembers(txn, room, userIDs); err != nil {
				return discovered{}, err
			}
			rooms[roomID] = room
		}

		var res discovered
		for _, roomID := range slices.Sorted(maps.Keys(remoteJoinRooms)) {
			if room, err := roomOrNil(r.txnGetRoom(txn, roomID)); err != nil {
				return discovered{}, err
			} else if isServerJoined(room) {
				res.joinedRooms = append(res.joinedRooms, roomID)
			}
		}
		for _, ev := range joinEvs {
			userID := id.UserID(*ev.StateKey)
			if currentMembers[ev.RoomID][types.MemberStateTup(userID)].EventID == ev.ID {
				res.sources = append(res.sources, types.RemoteUserDirectorySource{
					UserID: userID, SourceEventID: ev.ID,
					Profile: r.txnGetPublicRoomMemberProfile(txn, rooms[ev.RoomID], ev.ID),
				})
			}
		}
		return res, nil
	})
	return res.sources, res.joinedRooms, err
}

// RoomRemoteDirectorySources returns the remote users joined in the room's current state as the
// record gives it, among a page of its members from bucket from, see state.Batch.TxnMembersPage, and
// the bucket the next page starts at, false once there is none.
func (r *RoomsDatabase) RoomRemoteDirectorySources(
	ctx context.Context,
	room *types.Room,
	from uint64,
	limit int,
) ([]types.RemoteUserDirectorySource, uint64, bool, error) {
	type page struct {
		sources []types.RemoteUserDirectorySource
		next    uint64
		more    bool
	}
	res, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (page, error) {
		members, next, more, err := r.state.NewBatch(room.ID).TxnMembersPage(txn, roomCurrentState(room), from, limit)
		if err != nil {
			return page{}, err
		}
		res := page{next: next, more: more}
		for _, userID := range slices.Sorted(maps.Keys(members)) {
			member := members[userID]
			if member.Membership != event.MembershipJoin || r.isLocalUser(userID) {
				continue
			} else if _, _, err := userID.ParseAndValidateRelaxed(); err != nil {
				continue
			}
			res.sources = append(res.sources, types.RemoteUserDirectorySource{
				UserID: userID, SourceEventID: member.EventID,
				Profile: r.txnGetPublicRoomMemberProfile(txn, room, member.EventID),
			})
		}
		return res, nil
	})
	return res.sources, res.next, res.more, err
}

// As in Synapse, a member event in a public or world-readable room is taken as
// the user's global profile rather than making a federation profile request.
func (r *RoomsDatabase) txnGetPublicRoomMemberProfile(
	txn fdb.ReadTransaction,
	room *types.Room,
	eventID id.EventID,
) *types.UserProfile {
	if room == nil {
		return nil
	}
	if room.JoinRule != string(event.JoinRulePublic) && room.HistoryVisibility != string(event.HistoryVisibilityWorldReadable) {
		return nil
	}
	ev := r.events.TxnGetEvent(txn, eventID)
	if ev == nil {
		return nil
	}
	profile := &types.UserProfile{}
	if name := gjson.GetBytes(ev.Content, "displayname"); name.Type == gjson.String {
		profile.DisplayName = name.Str
	}
	if avatar := gjson.GetBytes(ev.Content, "avatar_url"); avatar.Type == gjson.String {
		profile.AvatarURL = avatar.Str
	}
	return profile
}
