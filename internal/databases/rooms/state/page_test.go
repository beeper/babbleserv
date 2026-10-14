package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func TestLeafEncodingRoundTrip(t *testing.T) {
	entries := []entry{
		{[]byte("a"), []byte("$one")},
		{[]byte("b\x00c"), []byte("$two")},
	}
	raw := encodeLeaf(entries)
	assert.Equal(t, tuple.Tuple{0, []byte("a"), []byte("$one"), []byte("b\x00c"), []byte("$two")}.Pack(), raw)
	assert.Equal(t, len(raw), 1+entries[0].encodedSize()+entries[1].encodedSize())

	p, err := decodePage(raw)
	require.NoError(t, err)
	assert.True(t, p.leaf)
	assert.Equal(t, entries, p.entries)
	assert.Equal(t, 2, p.count)
	assert.Equal(t, len(raw)-1, p.bytes)
	assert.Equal(t, []byte("$two"), p.find([]byte("b\x00c")))
	assert.Nil(t, p.find([]byte("b")))
}

func TestBranchEncodingRoundTrip(t *testing.T) {
	var children [fanout]types.StateHash
	children[0] = hashOf([]byte("zero"))
	children[5] = hashOf([]byte("five"))
	raw := encodeBranch(&children, 300, 24000)

	expected := tuple.Tuple{1, 300, 24000}
	for i := range fanout {
		switch i {
		case 0, 5:
			expected = append(expected, children[i][:])
		default:
			expected = append(expected, []byte{})
		}
	}
	assert.Equal(t, expected.Pack(), raw)

	p, err := decodePage(raw)
	require.NoError(t, err)
	assert.False(t, p.leaf)
	assert.Equal(t, children, p.children)
	assert.Equal(t, 300, p.count)
	assert.Equal(t, 24000, p.bytes)
}

func TestHashesAreDeterministic(t *testing.T) {
	raw := encodeLeaf([]entry{{[]byte("key"), []byte("value")}})
	sum := sha256.Sum256(raw)
	assert.Equal(t, types.StateHash(sum[:16]), hashOf(raw))
	assert.Equal(t, hashOf(raw), hashOf(encodeLeaf([]entry{{[]byte("key"), []byte("value")}})))
	assert.NotEqual(t, hashOf(raw), hashOf(encodeLeaf([]entry{{[]byte("key"), []byte("other")}})))

	bucketSum := sha256.Sum256([]byte("key"))
	assert.Equal(t, int(bucketSum[0]>>4), slot(bucketOf([]byte("key")), 0))
	assert.Equal(t, int(bucketSum[0]&0xf), slot(bucketOf([]byte("key")), 1))
	assert.Equal(t, int(bucketSum[7]&0xf), slot(bucketOf([]byte("key")), maxDepth-1))
}

func TestContextEncoding(t *testing.T) {
	c := stateContext{stateRoot: hashOf([]byte("state"))}
	raw := c.encode()
	assert.Equal(t, tuple.Tuple{c.stateRoot[:], []byte{}}.Pack(), raw)

	decoded, err := decodeContext(raw)
	require.NoError(t, err)
	assert.Equal(t, c, decoded)
	assert.Equal(t, hashOf(raw), c.ID())

	assert.False(t, EmptyContext.IsZero())
	assert.Equal(t, hashOf(tuple.Tuple{[]byte{}, []byte{}}.Pack()), EmptyContext)

	_, err = decodeContext(tuple.Tuple{c.stateRoot[:], []byte{}, []byte{}}.Pack())
	assert.ErrorIs(t, err, ErrInvalidContext, "a context row holds two roots")
}

// The leaf limits, hash functions, bucket order, value and page encodings are the format: every
// stored root and context ID depends on them, so these must never change.
func TestGoldenRoots(t *testing.T) {
	stateMap := types.StateMap{createTup: "$create", powerTup: "$power", rulesTup: "$rules", topicTup: "$topic"}
	for i := range 300 {
		stateMap[types.MemberStateTup(userID(i))] = id.EventID(fmt.Sprintf("$%s%d", testMemberships[i%len(testMemberships)], i))
	}
	b := newBatch(newMemStore(), newCache(0), testRoomID, defaultLeafLimits)
	ctx, err := b.TxnApply(nil, EmptyContext, withMemberships(stateMap))
	require.NoError(t, err)
	c := b.contexts[ctx]
	assert.Equal(t, "2ede2f124a889bb3779e090af0c7a0de", hex.EncodeToString(c.stateRoot[:]))
	assert.Equal(t, "75bc96d192f2aa4d3aacba70da057f7d", hex.EncodeToString(c.memberRoot[:]))
	assert.Equal(t, "9830cba165ecd52e7378e2d0aa6f4537", hex.EncodeToString(ctx[:]))
	assert.False(t, b.tree.pending[c.memberRoot].page.leaf, "300 members make a branch")
}

func TestDecodeRejectsMalformedPages(t *testing.T) {
	emptySlots := make(tuple.Tuple, fanout)
	for i := range emptySlots {
		emptySlots[i] = []byte{}
	}
	for name, raw := range map[string][]byte{
		"not a tuple":           {0xff},
		"empty":                 tuple.Tuple{}.Pack(),
		"unknown tag":           tuple.Tuple{2}.Pack(),
		"string tag":            tuple.Tuple{"x", []byte("key"), []byte("value")}.Pack(),
		"bytes tag":             tuple.Tuple{[]byte{}, []byte("key"), []byte("value")}.Pack(),
		"nil tag":               tuple.Tuple{nil, []byte("key"), []byte("value")}.Pack(),
		"empty leaf":            tuple.Tuple{0}.Pack(),
		"odd leaf":              tuple.Tuple{0, []byte("key")}.Pack(),
		"string leaf key":       tuple.Tuple{0, "key", []byte("value")}.Pack(),
		"short branch":          tuple.Tuple{1, 300, 24000, []byte{}}.Pack(),
		"branch without totals": append(tuple.Tuple{1}, emptySlots...).Pack(),
		"zero branch count":     append(tuple.Tuple{1, 0, 24000}, emptySlots...).Pack(),
		"bad child length":      append(tuple.Tuple{1, 300, 24000, []byte("short")}, emptySlots[1:]...).Pack(),
	} {
		_, err := decodePage(raw)
		assert.ErrorIs(t, err, ErrInvalidPage, name)
	}

	_, err := decodeContext(tuple.Tuple{[]byte("short"), []byte{}}.Pack())
	assert.ErrorIs(t, err, ErrInvalidContext)
}
