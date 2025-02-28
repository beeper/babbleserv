package types_test

import (
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func init() {
	fdb.MustAPIVersion(710)
}

func TestVersionstamp(t *testing.T) {
	incompleteVersion := tuple.IncompleteVersionstamp(1)

	// Check we can do version -> bytes -> same version
	b := types.VersionstampToValue(incompleteVersion)
	versionFromBytes, err := types.ValueToVersionstamp(b)
	require.NoError(t, err, string(b))
	assert.Equal(t, incompleteVersion, versionFromBytes)

	// Now check we can put it through base64 and still get the same result
	b, err = util.Base64Decode(util.Base64Encode(b))
	require.NoError(t, err)
	versionFromBytes, err = types.ValueToVersionstamp(b)
	require.NoError(t, err)
	assert.Equal(t, incompleteVersion, versionFromBytes)
}

func TestVersionMap(t *testing.T) {
	incompleteVersionstamp := tuple.IncompleteVersionstamp(1)
	otherVersionstamp := tuple.Versionstamp{
		TransactionVersion: [10]uint8{0xff, 0x00, 0xf1, 0x23, 0x33, 0xff, 0xff, 0xff, 0xff, 0xff},
		UserVersion:        16,
	}
	versions := types.VersionMap{
		types.RoomsVersionKey:    incompleteVersionstamp,
		types.AccountsVersionKey: otherVersionstamp,
	}

	// Check we can marshal it
	b, err := msgpack.Marshal(versions)
	require.NoError(t, err)

	// Check that when we unmarshal it back we get the same versions
	var newVersions types.VersionMap
	err = msgpack.Unmarshal(b, &newVersions)
	require.NoError(t, err)
	assert.Equal(t, incompleteVersionstamp, newVersions[types.RoomsVersionKey])
	assert.Equal(t, otherVersionstamp, newVersions[types.AccountsVersionKey])

	// Check that our custom encoding using bytes (vs. reflection on vstamp struct fields)
	rawMap := map[string][]byte{
		string(types.RoomsVersionKey):    types.VersionstampToValue(incompleteVersionstamp),
		string(types.AccountsVersionKey): types.VersionstampToValue(otherVersionstamp),
	}
	rawB, err := msgpack.Marshal(rawMap)
	require.NoError(t, err)
	assert.Equal(t, b, rawB)

	// Check that we can unmarshal into a partially filled map without clobbering it
	partialVersions := types.VersionMap{
		"someOtherKey": incompleteVersionstamp,
	}
	err = msgpack.Unmarshal(rawB, &partialVersions)
	require.NoError(t, err)
	assert.Equal(t, incompleteVersionstamp, partialVersions[types.RoomsVersionKey])
	assert.Equal(t, otherVersionstamp, partialVersions[types.AccountsVersionKey])
	assert.Equal(t, incompleteVersionstamp, partialVersions["someOtherKey"])
}
