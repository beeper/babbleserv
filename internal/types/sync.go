package types

import (
	"encoding/json"
	"maps"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
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
	if o == nil || o.Filter == nil {
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

func (o *SyncOptions) GetRoomFilter() *mautrix.RoomFilter {
	if o == nil || o.Filter == nil {
		return nil
	}
	return &o.Filter.Room
}

func (o *SyncOptions) GetTimelineFilter() *mautrix.FilterPart {
	if o == nil || o.Filter == nil {
		return nil
	}
	return &o.Filter.Room.Timeline
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
	NextBatch   string            `json:"next_batch"`
	Rooms       *syncRooms        `json:"rooms,omitempty"`
	DeviceLists *syncDeviceLists  `json:"device_lists,omitempty"`
	AccountData *PartialEventList `json:"account_data,omitempty"`
	ToDevice    *PartialEventList `json:"to_device,omitempty"`
}

func NewSync(rooms map[MembershipTup]*SyncRoom, accounts map[AccountDataTup]map[string]any, toDevice []*ToDevice) *Sync {
	sync := &Sync{}

	if len(toDevice) > 0 {
		toDeviceEvents := make([]*PartialEvent, len(toDevice))
		for i, td := range toDevice {
			toDeviceEvents[i] = td.ToEvent()
		}
		sync.ToDevice = &PartialEventList{
			Events: toDeviceEvents,
		}
	}

	allRooms := make(map[id.RoomID]*SyncRoom, len(rooms))
	for membershipTup, room := range rooms {
		allRooms[membershipTup.RoomID] = room
	}

	globalAccountData := make([]*PartialEvent, 0, 1)
	for accountDataTup, content := range accounts {
		partialEv := NewPartialEvent(
			"",
			accountDataTup.Type,
			nil,
			"",
			content,
		)
		if accountDataTup.RoomID == "" {
			globalAccountData = append(globalAccountData, partialEv)
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
			room.AccountData.Events = make([]*PartialEvent, 0, 1)
		}
		room.AccountData.Events = append(room.AccountData.Events, partialEv)
	}
	if len(globalAccountData) > 0 {
		sync.AccountData = &PartialEventList{Events: globalAccountData}
	}

	sync.Rooms = &syncRooms{
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
	return (s.AccountData == nil || len(s.AccountData.Events) == 0) &&
		(s.ToDevice == nil || len(s.ToDevice.Events) == 0) &&
		(s.DeviceLists == nil || (len(s.DeviceLists.Changed) == 0 &&
			len(s.DeviceLists.Left) == 0)) &&
		(s.Rooms == nil || (len(s.Rooms.Join) == 0 &&
			len(s.Rooms.Leave) == 0 &&
			len(s.Rooms.Invite) == 0 &&
			len(s.Rooms.Knock) == 0))
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
		s.Rooms = nil
	}

	// TODO
	// Combine all room device list updates
	// Combine all room presence updates
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

type SyncRoom struct {
	// Rooms database
	TimelineEvents Timeline         `json:"timeline"`
	StateEvents    EventList        `json:"state"`
	Ephemeral      PartialEventList `json:"ephemeral"`
	AccountData    PartialEventList `json:"account_data"`

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
		s.Ephemeral.Events = []*PartialEvent{}
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

		rev := NewPartialEvent(s.Receipts[0].RoomID, event.EphemeralEventReceipt, nil, "", rawContent)
		s.Ephemeral.Events = append(s.Ephemeral.Events, rev)
	}

	// TODO: typing, etc
}

func (s *SyncRoom) toInviteRoom() *syncRoomInvite {
	return &syncRoomInvite{SyncRoom: s, InviteState: s.StateEvents}
}

func (s *SyncRoom) toKnockRoom() *syncRoomKnock {
	return &syncRoomKnock{SyncRoom: s, KnockState: s.StateEvents}
}
