package types

import (
	"time"

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

// RemoteUserDirectorySource identifies the current remote join event
// which made a user known to this homeserver. SourceEventID acts as a stable
// generation for pending profile refresh jobs. Profile is set when the source
// membership is in a public or world-readable room, whose member event profile
// is used directly instead of a federation profile lookup.
type RemoteUserDirectorySource struct {
	UserID        id.UserID
	SourceEventID id.EventID
	Profile       *UserProfile
}

type RemoteUserDirectoryProfileJob struct {
	UserID        id.UserID
	SourceEventID id.EventID
	DueAt         time.Time
	Attempts      int
}
