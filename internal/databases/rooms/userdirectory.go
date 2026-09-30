package rooms

import (
	"context"
	"unicode/utf8"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const maxRemoteMembershipGenerationScan = 256

func (r *RoomsDatabase) FilterUserDirectoryCandidates(
	ctx context.Context,
	requester id.UserID,
	candidates []*types.UserDirectoryCandidate,
	maxResults int,
	maxMembershipRows int,
	maxMembershipRowsPerUser int,
) ([]*types.UserDirectoryCandidate, bool, error) {
	type result struct {
		candidates []*types.UserDirectoryCandidate
		limited    bool
	}
	res, err := util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (result, error) {
		visible := make([]*types.UserDirectoryCandidate, 0, min(maxResults+1, len(candidates)))
		remaining := maxMembershipRows
		limited := false
		for _, candidate := range candidates {
			if candidate.UserID == requester {
				continue
			}
			if remaining <= 1 {
				limited = true
				break
			}
			// Reserve one row for the membership scan's lookahead.
			memberships, more := r.users.TxnLookupUserMembershipRows(txn, candidate.UserID, min(remaining-1, maxMembershipRowsPerUser))
			remaining -= len(memberships)
			if more {
				remaining--
			}
			rooms := make([]fdb.FutureByteSlice, len(memberships))
			for i, membership := range memberships {
				if membership.Membership == event.MembershipJoin {
					rooms[i] = txn.Get(r.KeyForRoom(membership.RoomID))
				}
			}
			found := false
			var visibleMembership types.MembershipTup
			for i, membership := range memberships {
				if membership.Membership != event.MembershipJoin {
					continue
				}
				b := rooms[i].MustGet()
				if b == nil {
					continue
				}
				room := types.MustNewRoomFromBytes(b, membership.RoomID)
				if room.JoinRule == string(event.JoinRulePublic) || room.HistoryVisibility == string(event.HistoryVisibilityWorldReadable) ||
					r.users.TxnIsUserJoinedRoom(txn, requester, membership.RoomID) {
					found = true
					visibleMembership = membership
					break
				}
			}
			if found {
				// Keep room-specific fallbacks out of the global index and of inputs
				// reused if this read transaction retries.
				result := *candidate
				if candidate.UserID.Homeserver() != r.config.ServerName &&
					(candidate.DisplayName == "" || candidate.AvatarURL == "") {
					if ev := r.events.TxnGetEvent(txn, visibleMembership.EventID); ev != nil {
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
				}
				visible = append(visible, &result)
				if len(visible) > maxResults {
					limited = true
					break
				}
			} else if more {
				limited = true
			}
		}
		if len(visible) > maxResults {
			visible = visible[:maxResults]
		}
		return result{visible, limited}, nil
	})
	return res.candidates, res.limited, err
}

// DiscoverRemoteDirectoryUsersForEvents returns remote users whose event in
// this batch is still the accepted current membership event. This
// validation prevents stale iterator work from scheduling profiles for users
// who have since left or whose membership generation has been replaced.
func (r *RoomsDatabase) DiscoverRemoteDirectoryUsersForEvents(
	ctx context.Context,
	eventTups []types.EventTupWithVersion,
) ([]types.RemoteUserDirectorySource, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) ([]types.RemoteUserDirectorySource, error) {
		sources := make([]types.RemoteUserDirectorySource, 0, len(eventTups))
		for _, eventTup := range eventTups {
			if eventTup.Type.String() != event.StateMember.Type {
				continue
			}
			ev := r.events.TxnGetEvent(txn, eventTup.EventID)
			if ev == nil || ev.Rejected || ev.SoftFailed || ev.Outlier || ev.StateKey == nil ||
				ev.RoomID != eventTup.RoomID || ev.Type.String() != event.StateMember.Type {
				continue
			}
			userID := id.UserID(*ev.StateKey)
			if userID.Homeserver() == r.config.ServerName {
				continue
			}
			if _, _, err := userID.ParseAndValidateRelaxed(); err != nil {
				continue
			}
			membershipBytes, err := txn.Get(r.events.KeyForCurrentRoomMember(ev.RoomID, userID)).Get()
			if err != nil {
				return nil, err
			} else if membershipBytes == nil {
				continue
			}
			membership := types.BytesToMembershipTup(membershipBytes)
			if membership.EventID != ev.ID {
				continue
			}
			sourceEventID, sourceRoomID := ev.ID, ev.RoomID
			joined := membership.Membership == event.MembershipJoin
			if !joined {
				// A user may leave one room while remaining joined elsewhere.
				// In that case retain a live join as the account-side profile
				// generation. If the bounded scan is inconclusive, preserve the
				// existing generation; the search visibility scan is independently
				// conservative under the same condition.
				currentMemberships, more := r.users.TxnLookupUserMembershipRows(
					txn, userID, maxRemoteMembershipGenerationScan,
				)
				for _, current := range currentMemberships {
					if current.Membership == event.MembershipJoin {
						joined = true
						sourceEventID, sourceRoomID = current.EventID, current.RoomID
						break
					}
				}
				if !joined && more {
					continue
				}
			}
			source := types.RemoteUserDirectorySource{
				UserID: userID, SourceEventID: sourceEventID, Joined: joined,
			}
			if joined {
				source.Profile = r.txnGetPublicRoomMemberProfile(txn, sourceRoomID, sourceEventID)
			}
			sources = append(sources, source)
		}
		return sources, nil
	})
}

// As in Synapse, a member event in a public or world-readable room is taken as
// the user's global profile rather than making a federation profile request.
func (r *RoomsDatabase) txnGetPublicRoomMemberProfile(
	txn fdb.ReadTransaction,
	roomID id.RoomID,
	eventID id.EventID,
) *types.UserProfile {
	b := txn.Get(r.KeyForRoom(roomID)).MustGet()
	if b == nil {
		return nil
	}
	room := types.MustNewRoomFromBytes(b, roomID)
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
