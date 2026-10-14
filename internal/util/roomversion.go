package util

import "github.com/matrix-org/gomatrixserverlib"

func RoomVersionHas(roomVersion string, capability func(gomatrixserverlib.IRoomVersion) bool) bool {
	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(roomVersion))
	return err == nil && capability(impl)
}
