package types

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

type PaginationOptions struct {
	// Ranges in FDB are from-inclusive, to-exclusive by default
	From, To tuple.Versionstamp

	Limit   int
	Reverse bool
	Mode    fdb.StreamingMode
}

func (o PaginationOptions) RangeOptions() fdb.RangeOptions {
	return fdb.RangeOptions{
		Limit:   o.Limit,
		Mode:    o.Mode,
		Reverse: o.Reverse,
	}
}
