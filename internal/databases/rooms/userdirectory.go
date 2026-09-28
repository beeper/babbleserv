package rooms

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

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
					break
				}
			}
			if found {
				visible = append(visible, candidate)
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
