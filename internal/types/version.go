package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
)

var (
	ZeroVersionstamp  = tuple.Versionstamp{}
	incompleteVersion = [10]uint8{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

func ValueForVersionstamp(version tuple.Versionstamp) []byte {
	if version.TransactionVersion == incompleteVersion {
		val, err := tuple.Tuple{version}.PackWithVersionstamp(nil)
		if err != nil {
			panic(err)
		}
		return val
	} else {
		return tuple.Tuple{version}.Pack()
	}
}

func ValueToVersionstamp(value []byte) (tuple.Versionstamp, error) {
	tup, err := tuple.Unpack(value)
	if err != nil {
		return ZeroVersionstamp, err
	}
	return tup[0].(tuple.Versionstamp), nil
}

func GetVersionRange(
	sub subspace.Subspace,
	fromVersion, toVersion tuple.Versionstamp,
	args ...tuple.TupleElement,
) fdb.Range {
	var begin, end fdb.KeyConvertible
	if fromVersion == ZeroVersionstamp {
		begin = fdb.Key(append(sub.Pack(args), byte(0x00)))
	} else {
		begin = sub.Pack(append(args, fromVersion))
	}
	if toVersion == ZeroVersionstamp {
		end = fdb.Key(append(sub.Pack(args), byte(0xff)))
	} else {
		end = sub.Pack(append(args, toVersion))
	}
	return fdb.KeyRange{Begin: begin, End: end}
}

type VersionKey string

var (
	// Each maps to a database - not sure where else to put them!
	RoomsVersionKey    VersionKey = "r"
	AccountsVersionKey VersionKey = "a"
	DevicesVersionKey  VersionKey = "d"
)

type VersionMap map[VersionKey]tuple.Versionstamp

// Custom marshal/unmarshal to use tuple encoding for version map values
func (vm VersionMap) MarshalMsgpack() ([]byte, error) {
	rawMap := make(map[string][]byte, len(vm))
	for k, v := range vm {
		rawMap[string(k)] = ValueForVersionstamp(v)
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
		version, err := ValueToVersionstamp(v)
		if err != nil {
			return err
		}
		vMap[VersionKey(k)] = version
	}
	*vm = vMap
	return nil
}
