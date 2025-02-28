package types

import (
	"encoding/json"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type syncRooms struct {
	Join   map[id.RoomID]*SyncRoom `json:"join,omitempty"`
	Leave  map[id.RoomID]*SyncRoom `json:"leave,omitempty"`
	Invite map[id.RoomID]*SyncRoom `json:"invite,omitempty"`
	Knock  map[id.RoomID]*SyncRoom `json:"knock,omitempty"`
}

type syncDeviceLists struct {
	Changed []id.UserID `json:"changed,omitempty"`
	Left    []id.UserID `json:"left,omitempty"`
}

type Sync struct {
	NextBatch   string           `json:"next_batch"`
	Rooms       *syncRooms       `json:"rooms,omitempty"`
	DeviceLists *syncDeviceLists `json:"device_lists,omitempty"`
	AccountData []*Event         `json:"account_data,omitempty"`
}

func NewSyncFromRooms(rooms map[MembershipTup]*SyncRoom) *Sync {
	sync := Sync{
		Rooms: &syncRooms{
			Join:   make(map[id.RoomID]*SyncRoom, len(rooms)),
			Leave:  make(map[id.RoomID]*SyncRoom, 5),
			Invite: make(map[id.RoomID]*SyncRoom, 5),
			Knock:  make(map[id.RoomID]*SyncRoom, 5),
		},
	}

	for membershipTup, room := range rooms {
		switch membershipTup.Membership {
		case event.MembershipJoin:
			sync.Rooms.Join[membershipTup.RoomID] = room
		case event.MembershipLeave:
			sync.Rooms.Leave[membershipTup.RoomID] = room
		case event.MembershipInvite:
			sync.Rooms.Invite[membershipTup.RoomID] = room
		case event.MembershipKnock:
			sync.Rooms.Knock[membershipTup.RoomID] = room
		}
	}

	return &sync
}

func (s *Sync) IsEmpty() bool {
	return len(s.AccountData) == 0 &&
		len(s.Rooms.Join) == 0 &&
		len(s.Rooms.Leave) == 0 &&
		len(s.Rooms.Invite) == 0 &&
		len(s.Rooms.Knock) == 0 &&
		len(s.DeviceLists.Changed) == 0 &&
		len(s.DeviceLists.Left) == 0
}

type marshalSync Sync

func (s *Sync) MarshalJSON() ([]byte, error) {
	s.prepareForJSON()
	return json.Marshal((*marshalSync)(s))
}

func (s *Sync) prepareForJSON() {
	// Merge all the room maps
	allRooms := s.Rooms.Join
	for k, v := range s.Rooms.Leave {
		allRooms[k] = v
	}
	for k, v := range s.Rooms.Invite {
		allRooms[k] = v
	}
	for k, v := range s.Rooms.Knock {
		allRooms[k] = v
	}

	if len(allRooms) == 0 {
		s.Rooms = nil
	} else {
		for _, room := range allRooms {
			room.prepareForJSON()
		}
	}

	// TODO
	// Combine all room device list updates
	// Combine all room presence updates
}

type SyncRoom struct {
	// Rooms database
	StateEvents    []*Event        `json:"state,omitempty"`
	TimelineEvents []*Event        `json:"timeline,omitempty"`
	Ephemeral      []*PartialEvent `json:"ephemeral,omitempty"`

	Receipts          []*Receipt  `json:"-"`
	DeviceListChanges []id.UserID `json:"-"`
	Typing            []id.UserID `json:"-"`
	PresenceEvents    []id.UserID `json:"-"`
	AccountData       []*Event    `json:"-"`
}

func (s *SyncRoom) prepareForJSON() {
	// Turn receipts -> ephemeral event
	// Turn typing -> ephemeral event
	if len(s.Receipts) > 0 {
		content := make(event.ReceiptEventContent, 0)

		for _, r := range s.Receipts {
			var data map[string]any
			if len(r.Data) > 0 {
				err := json.Unmarshal(r.Data, &data)
				if err != nil {
					panic(err)
				}
			}

			content.Set(r.EventID, r.Type, r.UserID, event.ReadReceipt{
				ThreadID: id.EventID(r.ThreadID),
				Extra:    data,
			})
		}

		rawContent := make(map[string]any, len(content))
		for evID, v := range content {
			rawContent[evID.String()] = v
		}

		rev := NewPartialEvent(s.Receipts[0].RoomID, event.EphemeralEventReceipt, nil, "", rawContent)

		s.Ephemeral = append(s.Ephemeral, rev)
	}
}
