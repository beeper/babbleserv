package types

import (
	"maunium.net/go/mautrix/id"
)

type PublicRoomInfo struct {
	RoomID           id.RoomID    `json:"room_id"`
	AvatarURL        string       `json:"avatar_url,omitempty"`
	CanonicalAlias   id.RoomAlias `json:"canonical_alias,omitempty"`
	GuestCanJoin     bool         `json:"guest_can_join"`
	JoinRule         string       `json:"join_rule,omitempty"`
	Name             string       `json:"name,omitempty"`
	NumJoinedMembers int          `json:"num_joined_members"`
	RoomType         string       `json:"room_type,omitempty"`
	Topic            string       `json:"topic,omitempty"`
	WorldReadable    bool         `json:"world_readable"`
}

type PublicRoomsResponse struct {
	Chunk                  []*PublicRoomInfo `json:"chunk"`
	NextBatch              string            `json:"next_batch,omitempty"`
	PrevBatch              string            `json:"prev_batch,omitempty"`
	TotalRoomCountEstimate *int              `json:"total_room_count_estimate,omitempty"`
}

type PublicRoomsFilter struct {
	GenericSearchTerm string
	RoomTypes         map[string]struct{}
	FilterRoomTypes   bool
}

type UserDirectoryCandidate struct {
	UserID      id.UserID `json:"user_id"`
	DisplayName string    `json:"display_name,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
}

type UserDirectoryResponse struct {
	Limited bool                      `json:"limited"`
	Results []*UserDirectoryCandidate `json:"results"`
}
