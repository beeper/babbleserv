package util

import "github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

func MakeVersionstampValue(version tuple.Versionstamp) []byte {
	val, err := tuple.Tuple{version}.PackWithVersionstamp(nil)
	if err != nil {
		panic(err)
	}
	return val
}
