package types

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"
)

// Sync request

const DefaultTimelineLimit = 10
const DefaultReceiptsLimit = 100

type SyncMode string

const (
	SyncModeLegacy    SyncMode = "SyncModeLegacy"
	SyncModeStreaming SyncMode = "SyncModeStreaming"
	SyncModeSliding   SyncMode = "SyncModeSliding"
)

type SyncOptions struct {
	Mode   SyncMode
	Filter *mautrix.Filter
	UserID id.UserID

	// Enables MSC4222: state_after
	EnableLegacyStateAfter bool

	// Flag indicating whether this sync is for S2S federation
	IsServerToServer bool
}

func (o *SyncOptions) GetTimelineLimit() int {
	if o == nil || o.Filter == nil || o.Filter.Room == nil || o.Filter.Room.Timeline == nil {
		return DefaultTimelineLimit
	}
	if o.Filter.Room.Timeline.Limit > 0 {
		return o.Filter.Room.Timeline.Limit
	}
	return DefaultTimelineLimit
}

func (o *SyncOptions) GetReceiptsLimit() int {
	if o == nil || o.Filter == nil {
		return DefaultReceiptsLimit
	}
	if o.Filter.Room.Ephemeral.Limit > 0 {
		return o.Filter.Room.Ephemeral.Limit
	}
	return DefaultReceiptsLimit
}

func (o *SyncOptions) UseRoomThreadedNotifications() bool {
	if o == nil || o.Filter == nil || o.Filter.Room == nil || o.Filter.Room.Timeline == nil {
		return false
	}
	return o.Filter.Room.Timeline.UnreadThreadNotifications
}

func (o *SyncOptions) GetRoomFilter() *mautrix.RoomFilter {
	if o == nil || o.Filter == nil {
		return nil
	}
	return o.Filter.Room
}

func (o *SyncOptions) GetTimelineFilter() *mautrix.FilterPart {
	if o == nil || o.Filter == nil {
		return nil
	}
	return o.Filter.Room.Timeline
}

// Represents a user device native/sliding sync connection
type UserSyncConn struct {
	StartPositions VersionMap `msgpack:"sp"`
	PrevPositions  VersionMap `msgpack:"pp"`
	PrevAtMS       int64      `msgpack:"ms"`
}

// Sync response
//

type syncRooms struct {
	Join   map[id.RoomID]*SyncRoom       `json:"join,omitempty"`
	Leave  map[id.RoomID]*SyncRoom       `json:"leave,omitempty"`
	Invite map[id.RoomID]*syncRoomInvite `json:"invite,omitempty"`
	Knock  map[id.RoomID]*syncRoomKnock  `json:"knock,omitempty"`
}

type syncDeviceLists struct {
	Changed []id.UserID `json:"changed,omitempty"`
	Left    []id.UserID `json:"left,omitempty"`
}

type Sync struct {
	NextBatch   string           `json:"next_batch"`
	Rooms       syncRooms        `json:"rooms,omitzero"`
	DeviceLists syncDeviceLists  `json:"device_lists,omitzero"`
	AccountData EventList        `json:"account_data,omitzero"`
	ToDevice    PartialEventList `json:"to_device,omitzero"`
	Presence    PartialEventList `json:"presence,omitzero"`
}

func NewSync(
	rooms map[MembershipTup]*SyncRoom,
	accounts map[AccountDataTup]map[string]any,
	toDevice []*ToDeviceWithVersion,
	pushRules *pushrules.PushRuleset,
) *Sync {
	sync := &Sync{}

	// If push rules changed, add them as m.push_rules global account data
	if pushRules != nil {
		accounts[AccountDataTup{Type: event.AccountDataPushRules}] = map[string]any{
			"global": pushRules,
		}
	}

	if len(toDevice) > 0 {
		// Convert internal device list to-device events into presence and device lists
		toDeviceEvents := make([]*PartialEvent, 0, len(toDevice))
		presenceEventsByUser := make(map[id.UserID]*PartialEvent, len(toDevice)) // latest per user
		changedUserIDs := make(map[id.UserID]struct{})
		leftUserIDs := make(map[id.UserID]struct{})

		for _, td := range toDevice {
			switch td.Type {
			case BabbleservLocalDeviceChange:
				changedUserIDs[td.Sender] = struct{}{}
			case BabbleservLocalDeviceLeft:
				leftUserIDs[td.Sender] = struct{}{}
			case BabbleservLocalPresenceChange:
				partialEv := td.ToPartialEvent()
				partialEv.Type = event.EphemeralEventPresence
				presenceEventsByUser[td.Sender] = partialEv
			default:
				toDeviceEvents = append(toDeviceEvents, td.ToPartialEvent())
			}
		}

		if len(toDeviceEvents) > 0 {
			sync.ToDevice = PartialEventList{
				Events: toDeviceEvents,
			}
		}

		if len(presenceEventsByUser) > 0 {
			sync.Presence = PartialEventList{
				Events: slices.Collect(maps.Values(presenceEventsByUser)),
			}
		}

		for userID := range changedUserIDs {
			sync.DeviceLists.Changed = append(sync.DeviceLists.Changed, userID)
		}
		for userID := range leftUserIDs {
			sync.DeviceLists.Left = append(sync.DeviceLists.Left, userID)
		}
	}

	allRooms := make(map[id.RoomID]*SyncRoom, len(rooms))
	for membershipTup, room := range rooms {
		allRooms[membershipTup.RoomID] = room
	}

	globalAccountData := make([]*Event, 0, 1)
	for accountDataTup, content := range accounts {
		ev := NewEventFromPartialEvent(NewPartialEvent(
			"",
			accountDataTup.Type,
			nil,
			"",
			content,
		))
		if accountDataTup.RoomID == "" {
			globalAccountData = append(globalAccountData, ev)
			continue
		}
		room, found := allRooms[accountDataTup.RoomID]
		if !found {
			room = &SyncRoom{}
			allRooms[accountDataTup.RoomID] = room
			rooms[MembershipTup{
				RoomID:     accountDataTup.RoomID,
				Membership: event.MembershipJoin,
			}] = room
		}
		if room.AccountData.Events == nil {
			room.AccountData.Events = make([]*Event, 0, 1)
		}
		room.AccountData.Events = append(room.AccountData.Events, ev)
	}
	if len(globalAccountData) > 0 {
		sync.AccountData = EventList{Events: globalAccountData}
	}

	sync.Rooms = syncRooms{
		Join:   make(map[id.RoomID]*SyncRoom, len(rooms)),
		Leave:  make(map[id.RoomID]*SyncRoom, 5),
		Invite: make(map[id.RoomID]*syncRoomInvite, 5),
		Knock:  make(map[id.RoomID]*syncRoomKnock, 5),
	}
	for membershipTup, room := range rooms {
		allRooms[membershipTup.RoomID] = room

		switch membershipTup.Membership {
		case event.MembershipJoin:
			sync.Rooms.Join[membershipTup.RoomID] = room
		case event.MembershipLeave, event.MembershipBan:
			sync.Rooms.Leave[membershipTup.RoomID] = room
		case event.MembershipInvite:
			sync.Rooms.Invite[membershipTup.RoomID] = room.toInviteRoom()
		case event.MembershipKnock:
			sync.Rooms.Knock[membershipTup.RoomID] = room.toKnockRoom()
		}
	}

	return sync
}

func (s *Sync) IsEmpty() bool {
	return len(s.AccountData.Events) == 0 &&
		len(s.ToDevice.Events) == 0 &&
		len(s.DeviceLists.Changed) == 0 &&
		len(s.DeviceLists.Left) == 0 &&
		len(s.Rooms.Join) == 0 &&
		len(s.Rooms.Leave) == 0 &&
		len(s.Rooms.Invite) == 0 &&
		len(s.Rooms.Knock) == 0 &&
		len(s.Presence.Events) == 0
}

type marshalSync Sync

func (s *Sync) MarshalJSON() ([]byte, error) {
	s.prepareForJSON()
	return json.Marshal((*marshalSync)(s))
}

func (s *Sync) prepareForJSON() {
	allRooms := make(map[id.RoomID]*SyncRoom, len(s.Rooms.Join))
	maps.Copy(allRooms, s.Rooms.Join)
	maps.Copy(allRooms, s.Rooms.Leave)

	var hasRooms bool
	for _, room := range allRooms {
		room.prepareForJSON()
		hasRooms = true
	}
	for _, room := range s.Rooms.Invite {
		room.prepareForJSON()
		hasRooms = true
	}
	for _, room := range s.Rooms.Knock {
		room.prepareForJSON()
		hasRooms = true
	}
	if !hasRooms {
		s.Rooms.Join = nil
		s.Rooms.Leave = nil
		s.Rooms.Invite = nil
		s.Rooms.Knock = nil
	}
}

type PartialEventList struct {
	Events []*PartialEvent `json:"events"`
}

type EventList struct {
	Events []*Event `json:"events"`
}

type Timeline struct {
	EventList `json:",inline"`
	Limited   bool `json:"limited"`
}

type syncRoomInvite struct {
	*SyncRoom   `json:"-"`
	InviteState EventList `json:"invite_state"`
}

type syncRoomKnock struct {
	*SyncRoom  `json:"-"`
	KnockState EventList `json:"knock_state"`
}

// UnreadNotificationCounts represents the unread notification counts for a room.
type UnreadNotificationCounts struct {
	NotificationCount int `json:"notification_count"`
	HighlightCount    int `json:"highlight_count"`
}

type SyncRoom struct {
	// Rooms database
	TimelineEvents      Timeline                  `json:"timeline"`
	StateEvents         EventList                 `json:"state"`
	Ephemeral           EventList                 `json:"ephemeral"`
	AccountData         EventList                 `json:"account_data"`
	UnreadNotifications *UnreadNotificationCounts `json:"unread_notifications"`

	// Per-thread notification counts (MSC3773), keyed by thread root event ID
	UnreadThreadNotifications map[string]*UnreadNotificationCounts `json:"unread_thread_notifications,omitzero"`

	Receipts          []*ReceiptWithVersion `json:"-"`
	DeviceListChanges []id.UserID           `json:"-"`
	Typing            []id.UserID           `json:"-"`
	PresenceEvents    []id.UserID           `json:"-"`
}

func (s *SyncRoom) prepareForJSON() {
	// Flag events for client use/marshal format
	for _, ev := range s.TimelineEvents.Events {
		ev.IsForClientAPI = true
	}
	for _, ev := range s.StateEvents.Events {
		ev.IsForClientAPI = true
	}

	if s.Ephemeral.Events == nil {
		s.Ephemeral.Events = []*Event{}
	}

	// Turn receipts -> ephemeral event
	if len(s.Receipts) > 0 {
		content := make(event.ReceiptEventContent, 10)

		for _, r := range s.Receipts {
			content.Set(r.EventID, r.Type, r.UserID, event.ReadReceipt{
				ThreadID:  id.EventID(r.ThreadID),
				Timestamp: time.UnixMilli(r.Timestamp),
			})
		}

		rawContent := make(map[string]any, len(content))
		for evID, v := range content {
			rawContent[evID.String()] = v
		}

		rev := NewEventFromPartialEvent(NewPartialEvent(
			s.Receipts[0].RoomID,
			event.EphemeralEventReceipt,
			nil,
			"",
			rawContent,
		))
		rev.IsForClientAPI = true
		s.Ephemeral.Events = append(s.Ephemeral.Events, rev)
	}

	// TODO: typing, etc
}

func (s *SyncRoom) toInviteRoom() *syncRoomInvite {
	if len(s.StateEvents.Events) != 1 {
		panic("invalid invite room")
	}
	return &syncRoomInvite{SyncRoom: s, InviteState: s.StateEvents}
}

func (s *SyncRoom) toKnockRoom() *syncRoomKnock {
	if len(s.StateEvents.Events) != 1 {
		panic("invalid knock room")
	}
	return &syncRoomKnock{SyncRoom: s, KnockState: s.StateEvents}
}
