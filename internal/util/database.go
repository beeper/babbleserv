package util

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/types"
)

const timeToVersionPrefix = "ttv"

func DoReadTransaction[T any](
	ctx context.Context,
	db fdb.Database,
	fn func(txn fdb.ReadTransaction) (T, error),
) (T, error) {
	if ctx.Err() != nil {
		var res T
		return res, ctx.Err()
	}

	_, file, no, _ := runtime.Caller(1)
	src := filepath.Base(file) + ":" + strconv.Itoa(no)
	log := zerolog.Ctx(ctx).With().
		Str("src", src).
		Logger()

	if res, err := db.ReadTransact(func(txn fdb.ReadTransaction) (any, error) {
		log.Trace().Msg("Start read transaction")
		start := time.Now()
		res, err := fn(txn)
		log.Trace().
			Err(err).
			Str("duration", time.Since(start).String()).
			Msg("End read transaction")
		return res, err
	}); err != nil {
		var res T // return empty T
		return res, err
	} else {
		return res.(T), nil
	}
}

func DoWriteTransaction[T any](
	ctx context.Context,
	db fdb.Database,
	fn func(txn fdb.Transaction) (T, error),
) (T, error) {
	if ctx.Err() != nil {
		var res T
		return res, ctx.Err()
	}

	_, file, no, _ := runtime.Caller(1)
	src := filepath.Base(file) + ":" + strconv.Itoa(no)
	log := zerolog.Ctx(ctx).With().
		Str("src", src).
		Logger()

	if res, err := db.Transact(func(txn fdb.Transaction) (any, error) {
		log.Trace().Msg("Start write transaction")
		start := time.Now()
		res, err := fn(txn)

		// Write global nanos -> version key - this allows us to, for any database,
		// map from time to FDB version. We use nanos here since FDB itself uses
		// micros (ie 1M txn/s), this should avoid any conflicts.
		txn.SetVersionstampedValue(
			tuple.Tuple{timeToVersionPrefix, time.Now().UnixNano()},
			types.ValueForVersionstamp(tuple.IncompleteVersionstamp(0)),
		)

		log.Trace().
			Err(err).
			Str("duration", time.Since(start).String()).
			Int64("size", txn.GetApproximateSize().MustGet()).
			Msg("End write transaction")
		return res, err
	}); err != nil {
		var res T // return empty T
		return res, err
	} else {
		return res.(T), nil
	}
}
