package queue

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
)

// A queue based on FoundationDB:
// key queue items by insert version
// - subspace for key -> value where key is the version, generic queue item bytes
// - subspace for versions -> tup(activeAt: time)
//
// To get the next item we paginate versions, looking for the first with eother:
// - zero active time
// - active time > hardcoded 5? minute timeout
// Then set the active time, pass the refresh function to the queue handler.
type Queue[V any] struct {
	db fdb.Database

	handlerFn func(context.Context, V, func()) error

	versionToItem subspace.Subspace
	queueTups     subspace.Subspace
}

func NewQueue[V any](
	db fdb.Database,
	subspace subspace.Subspace,
) *Queue[V] {
	return &Queue[V]{
		db:            db,
		versionToItem: subspace.Sub("vt"),
		queueTups:     subspace.Sub("qt"),
	}
}
