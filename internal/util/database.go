package util

import (
	"context"
	"math"
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

func TxnIterAllRange(txn fdb.ReadTransaction, rng fdb.Range, f func(fdb.KeyValue) error) error {
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return err
		}
		f(kv)
	}
	return nil
}

func TxnGetLatestWriteVersion(ctx context.Context, txn fdb.ReadTransaction) tuple.Versionstamp {
	kvs := txn.GetRange(
		tuple.Tuple{timeToVersionPrefix},
		fdb.RangeOptions{
			Reverse: true,
			Limit:   1,
		},
	).GetSliceOrPanic()
	return types.MustValueToVersionstamp(kvs[0].Value)
}

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
		// Use a snapshot for the transaction since we're read-only, this means changes to the keys
		// we read won't conflict (we still read a consistent view of the DB).
		res, err := fn(txn.Snapshot())
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

		// Write global nanos -> version key - this allows us to, for any database, map from time
		// to FDB version. We use nanos here since FDB itself uses micros (ie 1M txn/s), this
		// should avoid any conflicts.
		txn.SetVersionstampedValue(
			tuple.Tuple{timeToVersionPrefix, time.Now().UnixNano()},
			// Use max uint16 -1 (65534) for user version so we're after anything persisted inside
			// the transaction. Means we have a hard insert limit batch size of 65533 per txn. We
			// minus one so we can use this value as a "from" which should not include itself, we
			// add one to the UserVersion at query location to achieve this.
			types.VersionstampToValue(tuple.IncompleteVersionstamp(math.MaxUint16-1)),
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
