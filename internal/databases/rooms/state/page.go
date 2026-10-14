package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/types"
)

// A subtree is a single leaf page when it has at most leafCapacity entries and encodes to at most
// leafMaxBytes, otherwise it is a branch. The byte bound keeps pages under the FoundationDB value
// limit when keys approach the 255 byte type and state key limits.
const (
	leafCapacity = 64
	leafMaxBytes = 64 << 10
)

const (
	fanout = 16
	// One nibble of the 8 byte bucket hash per level, pages this deep are always leaves.
	maxDepth = 16

	leafTag   = 0
	branchTag = 1
)

var ErrInvalidPage = errors.New("invalid state page")

type entry struct {
	key, value []byte
}

// encodedSize is the entry's share of a leaf encoding: both byte strings with their type code,
// terminator and escaped zero bytes.
func (e entry) encodedSize() int {
	return 4 + len(e.key) + len(e.value) + bytes.Count(e.key, []byte{0}) + bytes.Count(e.value, []byte{0})
}

// page is a decoded leaf or branch. count and bytes total the entries of the whole subtree, bytes
// being their encoded size, so a leaf encodes to bytes plus its one byte tag.
type page struct {
	leaf     bool
	entries  []entry
	children [fanout]types.StateHash
	count    int
	bytes    int
}

func (p *page) find(key []byte) []byte {
	i, found := slices.BinarySearchFunc(p.entries, key, func(e entry, key []byte) int {
		return bytes.Compare(e.key, key)
	})
	if !found {
		return nil
	}
	return p.entries[i].value
}

func hashOf(raw []byte) types.StateHash {
	sum := sha256.Sum256(raw)
	return types.StateHash(sum[:16])
}

func bucketOf(key []byte) uint64 {
	sum := sha256.Sum256(key)
	return binary.BigEndian.Uint64(sum[:8])
}

func slot(bucket uint64, depth int) int {
	return int(bucket>>(60-4*depth)) & (fanout - 1)
}

func hashToBytes(h types.StateHash) []byte {
	if h.IsZero() {
		return []byte{}
	}
	return h[:]
}

func hashFromElement(el tuple.TupleElement) (types.StateHash, error) {
	b, ok := el.([]byte)
	switch {
	case !ok:
		return types.StateHash{}, fmt.Errorf("state hash is %T, not bytes", el)
	case len(b) == 0:
		return types.StateHash{}, nil
	case len(b) != len(types.StateHash{}):
		return types.StateHash{}, fmt.Errorf("state hash has %d bytes", len(b))
	}
	return types.StateHash(b), nil
}

func encodeLeaf(entries []entry) []byte {
	tup := make(tuple.Tuple, 0, 1+2*len(entries))
	tup = append(tup, leafTag)
	for _, e := range entries {
		tup = append(tup, e.key, e.value)
	}
	return tup.Pack()
}

func encodeBranch(children *[fanout]types.StateHash, count, size int) []byte {
	tup := make(tuple.Tuple, 0, 3+fanout)
	tup = append(tup, branchTag, count, size)
	for _, child := range children {
		tup = append(tup, hashToBytes(child))
	}
	return tup.Pack()
}

func decodePage(raw []byte) (*page, error) {
	tup, err := tuple.Unpack(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPage, err)
	} else if len(tup) == 0 {
		return nil, fmt.Errorf("%w: empty", ErrInvalidPage)
	}
	tag, ok := tup[0].(int64)
	if !ok {
		return nil, fmt.Errorf("%w: tag is %T", ErrInvalidPage, tup[0])
	}

	switch {
	case tag == leafTag && len(tup) >= 3 && len(tup)%2 == 1:
		p := &page{leaf: true, entries: make([]entry, 0, len(tup)/2), count: len(tup) / 2, bytes: len(raw) - 1}
		for i := 1; i < len(tup); i += 2 {
			key, keyOK := tup[i].([]byte)
			value, valueOK := tup[i+1].([]byte)
			if !keyOK || !valueOK {
				return nil, fmt.Errorf("%w: leaf entry %d is not bytes", ErrInvalidPage, i/2)
			}
			p.entries = append(p.entries, entry{key, value})
		}
		return p, nil
	case tag == branchTag && len(tup) == 3+fanout:
		count, countOK := tup[1].(int64)
		size, sizeOK := tup[2].(int64)
		if !countOK || !sizeOK || count <= 0 || size <= 0 {
			return nil, fmt.Errorf("%w: branch totals %v and %v", ErrInvalidPage, tup[1], tup[2])
		}
		p := &page{count: int(count), bytes: int(size)}
		for i := range fanout {
			if p.children[i], err = hashFromElement(tup[3+i]); err != nil {
				return nil, fmt.Errorf("%w: branch slot %d: %w", ErrInvalidPage, i, err)
			}
		}
		return p, nil
	default:
		return nil, fmt.Errorf("%w: tag %d with %d elements", ErrInvalidPage, tag, len(tup))
	}
}
