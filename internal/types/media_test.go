package types_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestMediaSerialization(t *testing.T) {
	media := types.NewMedia("serverName", "mediaID", "storeKey", id.UserID("sender"))

	b, err := msgpack.Marshal(media)
	require.NoError(t, err)

	mediaFromBytes, err := types.NewMediaFromBytes(b, "serverName", "mediaID")
	require.NoError(t, err)

	assert.Equal(t, media.ServerName, mediaFromBytes.ServerName)
}
