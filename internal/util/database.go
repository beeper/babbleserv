package util

import (
	"context"
	"runtime"
	"strconv"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	timeToVersionPrefix   = "ttv"
	versionToTimePrefix   = "vtt"
	maxTransactionRetries = 10
)

func TxnIterAllRange(txn fdb.ReadTransaction, rng fdb.Range, f func(fdb.KeyValue) error) error {
	iter := txn.GetRange(rng, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).Iterator()
	for iter.Advance() {
		kv, err := iter.Get()
		if err != nil {
			return err
		} else if err := f(kv); err != nil {
			return err
		}
	}
	return nil
}

func TxnGetTimeForVersion(txn fdb.ReadTransaction, version tuple.Versionstamp) *time.Time {
	version.UserVersion = types.MaxVersionstampUserVersion // time is stored at this UserVersion
	b := txn.Get(tuple.Tuple{versionToTimePrefix, version}).MustGet()
	if b == nil {
		return nil
	}
	tup, _ := tuple.Unpack(b)
	nanos := tup[0].(int64)
	time := time.Unix(0, nanos)
	return &time
}

func TxnGetLatestWriteVersion(txn fdb.ReadTransaction) tuple.Versionstamp {
	kvs := txn.GetRange(
		tuple.Tuple{timeToVersionPrefix},
		fdb.RangeOptions{
			Reverse: true,
			Limit:   1,
		},
	).GetSliceOrPanic()
	if len(kvs) == 0 {
		return types.ZeroVersionstamp
	}
	return types.MustBytesToVersionstamp(kvs[0].Value)
}

func TxnGetLatestWriteVersionBefore(txn fdb.ReadTransaction, before time.Time) tuple.Versionstamp {
	kvs := txn.GetRange(
		tuple.Tuple{timeToVersionPrefix, before.UTC().UnixNano()},
		fdb.RangeOptions{
			Reverse: true,
			Limit:   1,
		},
	).GetSliceOrPanic()
	if len(kvs) == 0 {
		return types.ZeroVersionstamp
	}
	return types.MustBytesToVersionstamp(kvs[0].Value)
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

	log := zerolog.Nop()
	if zerolog.GlobalLevel() == zerolog.TraceLevel {
		_, file, no, _ := runtime.Caller(1)
		src := file + ":" + strconv.Itoa(no)
		log = zerolog.Ctx(ctx).With().
			Str("src", src).
			Logger()
	}

	res, err := db.ReadTransact(func(txn fdb.ReadTransaction) (any, error) {
		log.Trace().Msg("Start read transaction")
		start := time.Now()
		// Use a snapshot for the transaction since we're read-only, this means changes to the keys
		// we read won't conflict (we still see a consistent view of the DB).
		res, err := fn(txn.Snapshot())
		log.Trace().
			Err(err).
			Stringer("duration", time.Since(start)).
			Msg("End read transaction")
		return res, err
	})

	if err != nil || res == nil {
		var res T // return empty T
		return res, err
	}
	return res.(T), nil
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

	res, err := doWriteTransactionWithRetries(ctx, func(log zerolog.Logger) (any, error) {
		return db.Transact(func(txn fdb.Transaction) (any, error) {
			log.Trace().Msg("Start write transaction")
			start := time.Now()
			res, err := fn(txn)
			log.Trace().
				Err(err).
				Stringer("duration", time.Since(start)).
				Int64("size", txn.GetApproximateSize().MustGet()).
				Msg("End write transaction")
			return res, err
		})
	})

	if err != nil || res == nil {
		var res T // return empty T
		return res, err
	}
	return res.(T), nil
}

func DoWriteTransactionWithVersion[T any](
	ctx context.Context,
	db fdb.Database,
	fn func(txn fdb.Transaction) (T, error),
) (T, error) {
	if ctx.Err() != nil {
		var res T
		return res, ctx.Err()
	}

	res, err := doWriteTransactionWithRetries(ctx, func(log zerolog.Logger) (any, error) {
		return db.Transact(func(txn fdb.Transaction) (any, error) {
			log.Trace().Msg("Start write transaction")
			start := time.Now()
			res, err := fn(txn)

			// Write global nanos -> version key & version -> nanos- this allows us to, for any database,
			// map from time to FDB version, and backwards.

			// We use nanos since FDB itself uses micros (ie 1M txn/s), this should avoid any conflicts
			timeNano := time.Now().UTC().UnixNano()

			// Use max for user version so we're at/after anything persisted
			version := tuple.IncompleteVersionstamp(types.MaxVersionstampUserVersion)
			versionBytes := types.MustVersionstampToBytes(version)

			txn.SetVersionstampedValue(
				tuple.Tuple{timeToVersionPrefix, timeNano},
				versionBytes,
			)
			key, _ := tuple.Tuple{versionToTimePrefix, version}.PackWithVersionstamp(nil)
			txn.SetVersionstampedKey(
				fdb.Key(key),
				tuple.Tuple{timeNano}.Pack(),
			)

			log.Trace().
				Err(err).
				Stringer("duration", time.Since(start)).
				Int64("size", txn.GetApproximateSize().MustGet()).
				Msg("End write transaction")
			return res, err
		})
	})

	if err != nil || res == nil {
		var res T // return empty T
		return res, err
	}
	return res.(T), nil
}

// FDB does do retries itself (apparently) but this appears to be either broken or inconsistent
// such that there are
func doWriteTransactionWithRetries(
	ctx context.Context,
	f func(log zerolog.Logger) (any, error),
) (any, error) {
	log := zerolog.Ctx(ctx).With().Logger()
	if zerolog.GlobalLevel() == zerolog.TraceLevel {
		_, file, no, _ := runtime.Caller(2)
		src := file + ":" + strconv.Itoa(no)
		log = log.With().Str("src", src).Logger()
	}

	var retries int

	for {
		res, err := f(log)

		// We only care about FDB errors here
		if fdbErr, ok := err.(fdb.Error); ok {
			var canRetry bool
			if retries < maxTransactionRetries {
				// https://apple.github.io/foundationdb/api-error-codes.html
				switch fdbErr.Code {
				case 1020:
					// > Transaction not committed due to conflict with another transaction
					// This happens when transactions involve high traffic keys, the FDB library
					// should already be retrying these but does not appear so.
					canRetry = true
				case 1036:
					// > Read or wrote an unreadable key
					// This can happen when querying ranges and two transactions race, both writing
					// to the range.
					canRetry = true
				}
			}
			if canRetry {
				retries++
				retryIn := time.Millisecond * 100 * time.Duration(retries)
				log.Warn().Err(err).
					Dur("retry_in", retryIn).
					Int("retries", retries).
					Msg("Retrying transaction error")
				select {
				// Sleep 100ms * retries, max 1s total
				case <-time.After(retryIn):
					continue // retry
				case <-ctx.Done():
					return ctx.Err(), nil
				}
			}
			log.Err(err).Msg("Transaction error")
		}

		return res, err
	}
}
