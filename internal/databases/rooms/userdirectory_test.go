package rooms

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	localTestServer           = "local.example"
	localBob        id.UserID = "@bob:local.example"
	remoteCarol     id.UserID = "@carol:remote.example"
	remoteDave      id.UserID = "@dave:remote.example"
)

// directoryTestRooms are the rooms of a directory filter test: their records, each user's rows, and
// each room's current state members
type directoryTestRooms struct {
	records map[id.RoomID]*types.Room
	rows    map[id.UserID][]types.MembershipTup
	members map[id.RoomID]types.StateEntries
	lookups int
}

func (d *directoryTestRooms) filter(shared []id.RoomID, lookups, perUser int) *directoryFilter {
	f := &directoryFilter{
		isLocal: func(userID id.UserID) bool { return userID.Homeserver() == localTestServer },
		budget:  directoryBudget{left: lookups, perUser: perUser, used: make(map[id.UserID]int)},
		rows: func(userID id.UserID, limit int) ([]types.MembershipTup, bool) {
			rows := d.rows[userID]
			if len(rows) > limit {
				return rows[:limit], true
			}
			return rows, false
		},
		rooms: func(roomIDs []id.RoomID) ([]*types.Room, error) {
			rooms := make([]*types.Room, len(roomIDs))
			for i, roomID := range roomIDs {
				rooms[i] = d.records[roomID]
			}
			return rooms, nil
		},
		members: func(room *types.Room, userIDs []id.UserID) (types.StateEntries, error) {
			d.lookups += len(userIDs)
			found := make(types.StateEntries)
			for _, userID := range userIDs {
				if entry, ok := d.members[room.ID][types.MemberStateTup(userID)]; ok {
					found[types.MemberStateTup(userID)] = entry
				}
			}
			return found, nil
		},
	}
	for _, roomID := range shared {
		f.shared = append(f.shared, d.records[roomID])
	}
	return f
}

func newDirectoryTestRooms() *directoryTestRooms {
	const private, other, public, readable = "!private:local.example", "!other:local.example", "!public:local.example", "!readable:local.example"
	row := func(eventID id.EventID, roomID id.RoomID, membership event.Membership) types.MembershipTup {
		return types.MembershipTup{EventID: eventID, RoomID: roomID, Membership: membership}
	}
	member := func(eventID id.EventID, membership event.Membership) types.StateEntry {
		return types.StateEntry{EventID: eventID, Membership: membership}
	}
	return &directoryTestRooms{
		records: map[id.RoomID]*types.Room{
			private:  {ID: private},
			other:    {ID: other},
			public:   {ID: public, JoinRule: string(event.JoinRulePublic)},
			readable: {ID: readable, HistoryVisibility: string(event.HistoryVisibilityWorldReadable)},
		},
		rows: map[id.UserID][]types.MembershipTup{
			localBob: {row("$bobPublic", public, event.MembershipJoin)},
			"@erin:" + localTestServer: {
				row("$erinLeft", private, event.MembershipLeave), row("$erinOther", other, event.MembershipJoin), row("$erinReadable", readable, event.MembershipJoin),
			},
			"@finn:" + localTestServer: {row("$finnOther", other, event.MembershipJoin), row("$finnPrivate", private, event.MembershipJoin)},
		},
		members: map[id.RoomID]types.StateEntries{
			private: {
				types.MemberStateTup(remoteCarol): member("$carolPrivate", event.MembershipJoin),
				types.MemberStateTup(remoteDave):  member("$daveLeft", event.MembershipLeave),
			},
			other: {
				types.MemberStateTup(remoteDave): member("$daveOther", event.MembershipJoin),
			},
			public: {
				types.MemberStateTup("@gus:remote.example"): member("$gusPublic", event.MembershipJoin),
			},
		},
	}
}

func TestDirectoryShowsUsersSharingARoomAndLocalUsersOfPublicRooms(t *testing.T) {
	d := newDirectoryTestRooms()
	f := d.filter([]id.RoomID{"!private:local.example"}, 1000, 100)
	found, undecided, err := f.visible([]id.UserID{
		localBob, "@erin:" + localTestServer, "@finn:" + localTestServer, remoteCarol, remoteDave, "@gus:remote.example",
	})
	require.NoError(t, err)
	assert.Equal(t, map[id.UserID]id.EventID{
		localBob:                   "$bobPublic",
		"@erin:" + localTestServer: "$erinReadable",
		"@finn:" + localTestServer: "$finnPrivate",
		remoteCarol:                "$carolPrivate",
	}, found, "a remote user who left the shared room, or is only in a public room, is not found")
	assert.Empty(t, undecided)
}

func TestDirectoryLooksRemoteUsersUpInEachSharedRoomOnceFound(t *testing.T) {
	d := newDirectoryTestRooms()
	f := d.filter([]id.RoomID{"!private:local.example", "!other:local.example"}, 1000, 100)
	found, undecided, err := f.visible([]id.UserID{remoteCarol, remoteDave})
	require.NoError(t, err)
	assert.Equal(t, map[id.UserID]id.EventID{remoteCarol: "$carolPrivate", remoteDave: "$daveOther"}, found)
	assert.Empty(t, undecided)
	assert.Equal(t, 3, d.lookups, "Carol is not looked up again once found")
}

func TestDirectoryBudgetLeavesCandidatesUndecided(t *testing.T) {
	d := newDirectoryTestRooms()
	f := d.filter([]id.RoomID{"!private:local.example", "!other:local.example"}, 1000, 1)
	found, undecided, err := f.visible([]id.UserID{remoteDave, "@erin:" + localTestServer})
	require.NoError(t, err)
	assert.Empty(t, found, "one lookup per user reaches neither Dave's second room nor any of Erin's rows")
	assert.Equal(t, map[id.UserID]bool{remoteDave: true, "@erin:" + localTestServer: true}, undecided)

	d = newDirectoryTestRooms()
	f = d.filter([]id.RoomID{"!private:local.example"}, 3, 100)
	found, undecided, err = f.visible([]id.UserID{"@erin:" + localTestServer, remoteDave})
	require.NoError(t, err)
	assert.Empty(t, found, "Erin's rows are cut short and take every lookup with the one telling there are more, leaving Dave none")
	assert.Equal(t, map[id.UserID]bool{remoteDave: true, "@erin:" + localTestServer: true}, undecided)
	assert.Equal(t, 0, f.budget.left)
}
