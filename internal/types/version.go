package types

import (
	"bytes"
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
)

const MaxVersionstampUserVersion = math.MaxUint16 - 1

var (
	ZeroVersionstamp  = tuple.Versionstamp{}
	incompleteVersion = [10]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	ErrInvalidVersion = errors.New("invalid versionstamp tuple")
)

func IsIncompleteVersionstamp(v tuple.Versionstamp) bool {
	return v.TransactionVersion == incompleteVersion
}

func DecodeRawVersionstamp(b []byte) tuple.Versionstamp {
	var transactionVersion [10]byte
	var userVersion uint16

	copy(transactionVersion[:], b[:10])

	return tuple.Versionstamp{
		TransactionVersion: transactionVersion,
		UserVersion:        userVersion,
	}
}

func VersionstampToString(version tuple.Versionstamp) (string, error) {
	vstamp, err := VersionstampToBytes(version)
	if err != nil {
		return "", err
	}
	return base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(vstamp), nil
}

func MustVersionstampToString(version tuple.Versionstamp) string {
	if vstamp, err := VersionstampToString(version); err != nil {
		panic(err)
	} else {
		return vstamp
	}
}

func StringToVersionstamp(s string) (tuple.Versionstamp, error) {
	b, err := base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return ZeroVersionstamp, err
	}
	return BytesToVersionstamp(b)
}

func VersionstampToBytes(version tuple.Versionstamp) ([]byte, error) {
	if IsIncompleteVersionstamp(version) {
		// Note that this seems to result in bytes that unpack to 4 tuple values (v, nil, nil, nil)
		// Not managed to figure out why.
		val, err := tuple.Tuple{version}.PackWithVersionstamp(nil)
		if err != nil {
			return nil, err
		}
		return val, nil
	} else {
		return tuple.Tuple{version}.Pack(), nil
	}
}

func MustVersionstampToBytes(version tuple.Versionstamp) []byte {
	if b, err := VersionstampToBytes(version); err != nil {
		panic(err)
	} else {
		return b
	}
}

func BytesToVersionstamp(value []byte) (tuple.Versionstamp, error) {
	tup, err := tuple.Unpack(value)
	if err != nil {
		return ZeroVersionstamp, fmt.Errorf("%w: %w", ErrInvalidVersion, err)
	} else if len(tup) == 0 {
		// As above only check for 0 here since these might have 3 nil elements appended(?)
		return ZeroVersionstamp, ErrInvalidVersion
	} else if v, ok := tup[0].(tuple.Versionstamp); !ok {
		return ZeroVersionstamp, ErrInvalidVersion
	} else {
		return v, nil
	}
}

func MustBytesToVersionstamp(value []byte) tuple.Versionstamp {
	if v, err := BytesToVersionstamp(value); err != nil {
		panic(err)
	} else {
		return v
	}
}

func MustPackVersionKey(sub subspace.Subspace, tup tuple.Tuple) fdb.Key {
	hasIncomplete, err := tup.HasIncompleteVersionstamp()
	if err != nil {
		panic(err)
	}
	if !hasIncomplete {
		return sub.Pack(tup)
	}
	if key, err := sub.PackWithVersionstamp(tup); err != nil {
		panic(err)
	} else {
		return key
	}
}

// Get a fdb.ExactRange of a subspace or tuple within. Flips FDBs range exclusivity defaults from
// [inclusive,exclusive) to (exclusive,inclusive], which matches the sync API.
func GetVersionRange(
	sub subspace.Subspace,
	fromVersion, toVersion tuple.Versionstamp,
	args ...tuple.TupleElement,
) fdb.ExactRange {
	prefix := sub.Pack(args)
	var begin, end fdb.KeyConvertible
	if fromVersion == ZeroVersionstamp {
		begin = fdb.Key(append(append([]byte(nil), prefix...), byte(0x00)))
	} else {
		// FDB range starts are inclusive by default, this switches that
		if after, ok := VersionstampAfter(fromVersion); ok {
			begin = sub.Pack(append(args, after))
		} else {
			begin = fdb.Key(append(append([]byte(nil), prefix...), byte(0xff)))
		}
	}
	if toVersion == ZeroVersionstamp {
		end = fdb.Key(append(append([]byte(nil), prefix...), byte(0xff)))
	} else {
		// FDB range ends are exclusive by default, this switches that
		if after, ok := VersionstampAfter(toVersion); ok {
			end = sub.Pack(append(args, after))
		} else {
			end = fdb.Key(append(append([]byte(nil), prefix...), byte(0xff)))
		}
	}
	return fdb.KeyRange{Begin: begin, End: end}
}

type VersionKey string

var (
	// Each maps to a database - not sure where else to put them!
	RoomsVersionKey     VersionKey = "r"
	AccountsVersionKey  VersionKey = "a"
	TransientVersionKey VersionKey = "t"
)

type VersionMap map[VersionKey]tuple.Versionstamp

// Custom marshal/unmarshal to use tuple encoding for version map values
func (vm VersionMap) MarshalMsgpack() ([]byte, error) {
	rawMap := make(map[string][]byte, len(vm))
	for k, v := range vm {
		version, err := VersionstampToBytes(v)
		if err != nil {
			return nil, err
		}
		rawMap[string(k)] = version
	}
	return msgpack.Marshal(rawMap)
}

func (vm *VersionMap) UnmarshalMsgpack(b []byte) error {
	// Decode into string -> []byte map
	rawMap := make(map[string][]byte)
	if err := msgpack.Unmarshal(b, &rawMap); err != nil {
		return err
	}
	vMap := *vm
	if vMap == nil {
		vMap = make(VersionMap, len(vMap))
	}
	for k, v := range rawMap {
		version, err := BytesToVersionstamp(v)
		if err != nil {
			return err
		}
		vMap[VersionKey(k)] = version
	}
	*vm = vMap
	return nil
}

type Versioner interface {
	GetVersion() tuple.Versionstamp
}

func SortVersioners[T Versioner](versions []T) {
	slices.SortFunc(versions, func(a, b T) int {
		return bytes.Compare(a.GetVersion().Bytes(), b.GetVersion().Bytes())
	})
}

func VersionIsBefore(version, beforeVersion tuple.Versionstamp) bool {
	return bytes.Compare(version.Bytes(), beforeVersion.Bytes()) == -1
}

func VersionIsAtOrBefore(version, beforeVersion tuple.Versionstamp) bool {
	return bytes.Compare(version.Bytes(), beforeVersion.Bytes()) <= 0
}

func VersionIsAfter(version, beforeVersion tuple.Versionstamp) bool {
	return bytes.Compare(version.Bytes(), beforeVersion.Bytes()) == 1
}

// VersionstampAfter returns the smallest versionstamp greater than version.
// Event batches reserve the maximum user version, but carrying into the
// transaction version keeps historical lookups correct for arbitrary tokens.
func VersionstampAfter(version tuple.Versionstamp) (tuple.Versionstamp, bool) {
	if IsIncompleteVersionstamp(version) {
		return ZeroVersionstamp, false
	}
	if version.UserVersion < math.MaxUint16 {
		version.UserVersion++
		return version, true
	}
	version.UserVersion = 0
	for i := len(version.TransactionVersion) - 1; i >= 0; i-- {
		if version.TransactionVersion[i] != math.MaxUint8 {
			version.TransactionVersion[i]++
			if version.TransactionVersion == incompleteVersion {
				return ZeroVersionstamp, false
			}
			return version, true
		}
		version.TransactionVersion[i] = 0
	}
	return ZeroVersionstamp, false
}

// VersionstampBefore returns the largest versionstamp less than version, borrowing from the
// transaction version so that VersionstampAfter maps the result back to version. Returns
// ZeroVersionstamp, an open range start, when version has no predecessor.
func VersionstampBefore(version tuple.Versionstamp) tuple.Versionstamp {
	if version.UserVersion > 0 {
		version.UserVersion--
		return version
	}
	version.UserVersion = math.MaxUint16
	for i := len(version.TransactionVersion) - 1; i >= 0; i-- {
		if version.TransactionVersion[i] != 0 {
			version.TransactionVersion[i]--
			return version
		}
		version.TransactionVersion[i] = math.MaxUint8
	}
	return ZeroVersionstamp
}
