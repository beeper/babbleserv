package events

import (
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// State used for authorization, excluding members
// https://spec.matrix.org/v1.11/server-server-api/#auth-events-selection
var authStateTups = []types.StateTup{
	{Type: event.StateCreate, StateKey: ""},
	{Type: event.StateJoinRules, StateKey: ""},
	{Type: event.StatePowerLevels, StateKey: ""},
}

func AuthStateTupsWithMembers(userIDs []id.UserID) []types.StateTup {
	return slices.Concat(authStateTups, MemberStateTups(userIDs))
}

// AuthStateTupsForEvent includes the state an auth check may read, including the
// additional membership and invite state named in member content.
func AuthStateTupsForEvent(ev *types.Event) []types.StateTup {
	userIDs := []id.UserID{ev.Sender}
	var thirdPartyInvite *gomatrixserverlib.MemberThirdPartyInvite
	if ev.Type == event.StateMember && ev.StateKey != nil {
		userIDs = append(userIDs, id.UserID(*ev.StateKey))
		if member, err := gomatrixserverlib.NewMemberContentFromEvent(ev.PDU()); err == nil {
			if member.AuthorisedVia != "" {
				userIDs = append(userIDs, id.UserID(member.AuthorisedVia))
			}
			thirdPartyInvite = member.ThirdPartyInvite
		}
	}
	keys := AuthStateTupsWithMembers(userIDs)
	if thirdPartyInvite != nil {
		keys = append(keys, types.StateTup{Type: event.StateThirdPartyInvite, StateKey: thirdPartyInvite.Signed.Token})
	}
	return uniqueStateTups(keys)
}

func uniqueStateTups(tups []types.StateTup) []types.StateTup {
	unique := tups[:0]
	for _, tup := range tups {
		if !slices.Contains(unique, tup) {
			unique = append(unique, tup)
		}
	}
	return unique
}

func MemberStateTups(userIDs []id.UserID) []types.StateTup {
	tups := make([]types.StateTup, len(userIDs))
	for i, userID := range userIDs {
		tups[i] = types.MemberStateTup(userID)
	}
	return tups
}

// Types of state event used for stripped state on invites
// https://spec.matrix.org/v1.11/client-server-api/#stripped-state
var StrippedStateTups = []types.StateTup{
	{Type: event.StateCreate, StateKey: ""},
	{Type: event.StateRoomName, StateKey: ""},
	{Type: event.StateRoomAvatar, StateKey: ""},
	{Type: event.StateTopic, StateKey: ""},
	{Type: event.StateJoinRules, StateKey: ""},
	{Type: event.StateCanonicalAlias, StateKey: ""},
	{Type: event.StateEncryption, StateKey: ""},
}

// authEventSelection is the state an event's auth_events may cite, as Synapse's
// auth_types_for_event selects it for every room version, and so narrower than
// AuthStateTupsForEvent. A domainless room ID implies the create event.
// https://spec.matrix.org/v1.16/server-server-api/#auth-events-selection
func authEventSelection(ev *types.Event) []types.StateTup {
	if ev.Type == event.StateCreate {
		return nil
	}
	selected := []types.StateTup{
		{Type: event.StatePowerLevels},
		types.MemberStateTup(ev.Sender),
	}
	if !util.RoomVersionHas(ev.RoomVersion, gomatrixserverlib.IRoomVersion.DomainlessRoomIDs) {
		selected = append(selected, types.StateTup{Type: event.StateCreate})
	}
	if ev.Type != event.StateMember || ev.StateKey == nil {
		return selected
	}
	selected = append(selected, types.MemberStateTup(id.UserID(*ev.StateKey)))
	if member, err := gomatrixserverlib.NewMemberContentFromEvent(ev.PDU()); err == nil {
		membership := event.Membership(member.Membership)
		switch membership {
		case event.MembershipJoin, event.MembershipInvite, event.MembershipKnock:
			selected = append(selected, types.StateTup{Type: event.StateJoinRules})
		}
		if membership == event.MembershipInvite && member.ThirdPartyInvite != nil {
			selected = append(selected, types.StateTup{Type: event.StateThirdPartyInvite, StateKey: member.ThirdPartyInvite.Signed.Token})
		}
		if membership == event.MembershipJoin && member.AuthorisedVia != "" {
			if roomSpec, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion()); err == nil && roomSpec.CheckRestrictedJoinsAllowed() == nil {
				selected = append(selected, types.MemberStateTup(id.UserID(member.AuthorisedVia)))
			}
		}
	}
	return uniqueStateTups(selected)
}
