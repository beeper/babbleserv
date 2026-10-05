package types_test

import (
	"bytes"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
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
	b := types.MustVersionstampToBytes(incompleteVersion)
	versionFromBytes, err := types.BytesToVersionstamp(b)
	require.NoError(t, err, string(b))
	assert.Equal(t, incompleteVersion, versionFromBytes)

	// Now check we can put it through base64 and still get the same result
	b, err = util.Base64Decode(util.Base64Encode(b))
	require.NoError(t, err)
	versionFromBytes, err = types.BytesToVersionstamp(b)
	require.NoError(t, err)
	assert.Equal(t, incompleteVersion, versionFromBytes)
}

func TestVersionstampBeforeAfter(t *testing.T) {
	batchStart := tuple.Versionstamp{
		TransactionVersion: [10]uint8{0, 0, 0, 0, 0x08, 0x09, 0x39, 0x55, 0x01, 0x00},
		UserVersion:        0,
	}
	before, ok := types.VersionstampBefore(batchStart)
	require.True(t, ok)
	assert.Equal(t, tuple.Versionstamp{
		TransactionVersion: [10]uint8{0, 0, 0, 0, 0x08, 0x09, 0x39, 0x55, 0x00, 0xff},
		UserVersion:        0xffff,
	}, before)
	after, ok := types.VersionstampAfter(before)
	require.True(t, ok)
	assert.Equal(t, batchStart, after)

	mid := tuple.Versionstamp{TransactionVersion: batchStart.TransactionVersion, UserVersion: 7}
	beforeMid, ok := types.VersionstampBefore(mid)
	require.True(t, ok)
	assert.Equal(t, uint16(6), beforeMid.UserVersion)

	beforeZero, ok := types.VersionstampBefore(types.ZeroVersionstamp)
	assert.False(t, ok)
	assert.Equal(t, types.ZeroVersionstamp, beforeZero)
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

	// Check that our custom msgpack encoding using bytes (vs. reflection on vstamp struct fields)
	rawMap := map[string][]byte{
		string(types.RoomsVersionKey):    types.MustVersionstampToBytes(incompleteVersionstamp),
		string(types.AccountsVersionKey): types.MustVersionstampToBytes(otherVersionstamp),
	}
	rawB, err := msgpack.Marshal(rawMap)
	require.NoError(t, err)
	var rawMapDecoded map[string][]byte
	err = msgpack.Unmarshal(rawB, &rawMapDecoded)
	require.NoError(t, err)
	assert.Equal(t, rawMap[string(types.RoomsVersionKey)], rawMapDecoded[string(types.RoomsVersionKey)])

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

func TestVersionstampBefore(t *testing.T) {
	version := func(userVersion uint16, transactionVersion ...byte) tuple.Versionstamp {
		v := tuple.Versionstamp{UserVersion: userVersion}
		copy(v.TransactionVersion[len(v.TransactionVersion)-len(transactionVersion):], transactionVersion)
		return v
	}

	for _, tc := range []struct {
		name    string
		version tuple.Versionstamp
		before  tuple.Versionstamp
	}{
		{"user version", version(5, 0x07, 0x4c), version(4, 0x07, 0x4c)},
		{"first of a transaction", version(0, 0x07, 0x4c), version(0xffff, 0x07, 0x4b)},
		{"borrow across bytes", version(0, 0x01, 0x00, 0x00), version(0xffff, 0x00, 0xff, 0xff)},
		{"first transaction", version(1), version(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, ok := types.VersionstampBefore(tc.version)
			require.True(t, ok)
			assert.Equal(t, tc.before, before)
			assert.True(t, types.VersionIsBefore(before, tc.version))

			after, ok := types.VersionstampAfter(before)
			require.True(t, ok)
			assert.Equal(t, tc.version, after)

			sub := subspace.Sub("versions")
			begin, _ := types.GetVersionRange(sub, before, types.ZeroVersionstamp).FDBRangeKeys()
			assert.LessOrEqual(t, bytes.Compare(begin.FDBKey(), sub.Pack(tuple.Tuple{tc.version})), 0)
		})
	}

	for _, tc := range []struct {
		name    string
		version tuple.Versionstamp
	}{
		{"zero", types.ZeroVersionstamp},
		{"incomplete", tuple.IncompleteVersionstamp(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, ok := types.VersionstampBefore(tc.version)
			assert.False(t, ok)
			assert.Equal(t, types.ZeroVersionstamp, before)
		})
	}
}
