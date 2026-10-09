package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestHasNonlocalUser(t *testing.T) {
	const serverName = "local.example"

	assert.False(t, hasNonlocalUser(serverName, mautrix.DeviceKeysRequest{}))
	assert.False(t, hasNonlocalUser(serverName, mautrix.DeviceKeysRequest{
		"@alice:local.example": nil,
		"@bob:local.example":   {"DEVICE"},
	}))
	assert.True(t, hasNonlocalUser(serverName, mautrix.DeviceKeysRequest{
		"@alice:local.example":  nil,
		"@carol:remote.example": nil,
	}))
	assert.True(t, hasNonlocalUser(serverName, mautrix.DeviceKeysRequest{"@nohomeserver": nil}))
	assert.True(t, hasNonlocalUser(serverName, mautrix.DeviceKeysRequest{"@alice:local.example.evil": nil}))

	assert.False(t, hasNonlocalUser(serverName, mautrix.OneTimeKeysRequest{
		"@alice:local.example": {"DEVICE": id.KeyAlgorithmSignedCurve25519},
	}))
	assert.True(t, hasNonlocalUser(serverName, mautrix.OneTimeKeysRequest{
		"@carol:remote.example": {"DEVICE": id.KeyAlgorithmSignedCurve25519},
	}))
}
