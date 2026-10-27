package config

import (
	"testing"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
)

func TestStateDAGRoomVersionOnlyOnceEnabled(t *testing.T) {
	var cfg BabbleConfig
	versions := cfg.RoomVersions()
	assert.NotContains(t, versions, string(gomatrixserverlib.RoomVersionStateDAGs))
	assert.True(t, versions["12"])

	cfg.Rooms.UnstableStateDAGs = true
	stable, found := cfg.RoomVersions()[string(gomatrixserverlib.RoomVersionStateDAGs)]
	assert.True(t, found)
	assert.False(t, stable)
}

func TestJoinedRoomVersionsAreThoseHashed(t *testing.T) {
	var cfg BabbleConfig
	assert.True(t, cfg.JoinsRoomVersion("12"))
	assert.True(t, cfg.JoinsRoomVersion(string(gomatrixserverlib.RoomVersionHydra)))
	assert.False(t, cfg.JoinsRoomVersion("1"))
	assert.False(t, cfg.JoinsRoomVersion("unknown"))
	assert.False(t, cfg.JoinsRoomVersion(string(gomatrixserverlib.RoomVersionStateDAGs)))
	cfg.Rooms.UnstableStateDAGs = true
	assert.True(t, cfg.JoinsRoomVersion(string(gomatrixserverlib.RoomVersionStateDAGs)))
}
