package lock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

var errIsLocked = errors.New("lock is locked")

type LockingDatabase interface {
	GetLockPrimitives() (fdb.Database, subspace.Subspace)
}

type LockOptions struct {
	RetryInterval time.Duration
	Timeout       time.Duration
}

type LockTxnRefreshFunc func(fdb.Transaction)

type Lock struct {
	TxnRefresh LockTxnRefreshFunc
	Refresh    func()
	Release    func()
}

func WithLockIfAvailable(
	ctx context.Context,
	database LockingDatabase,
	name string,
	options LockOptions,
	handler func(Lock),
) (bool, error) {
	token, err := acquireLockOnce(ctx, database, name, options)
	if err == errIsLocked {
		return false, nil
	} else if err != nil {
		return false, err
	}
	handler(makeLock(ctx, database, name, options, token))
	return true, nil
}

func WithLock(
	ctx context.Context,
	database LockingDatabase,
	name string,
	options LockOptions,
	handler func(Lock),
) {
	token := acquireLock(ctx, database, name, options)
	if token == nil {
		if ctx.Err() == context.Canceled {
			return
		}
		panic("acquired lock with no token")
	}
	handler(makeLock(ctx, database, name, options, token))
}

func GetLock(
	ctx context.Context,
	database LockingDatabase,
	name string,
	options LockOptions,
) *Lock {
	token := acquireLock(ctx, database, name, options)
	if token == nil {
		if ctx.Err() == context.Canceled {
			return nil
		}
		panic("acquired lock with no token")
	}
	lock := makeLock(ctx, database, name, options, token)
	return &lock
}

func acquireLock(ctx context.Context, database LockingDatabase, name string, options LockOptions) []byte {
	log := zerolog.Ctx(ctx).With().Str("name", name).Logger()
	log.Debug().Msg("Acquiring lock...")

	for {
		if token, err := acquireLockOnce(ctx, database, name, options); err == errIsLocked {
			log.Trace().
				Dur("retry", options.RetryInterval/2).
				Msg("Lock is currently locked, retrying in")
		} else if err != nil {
			log.Err(err).Msg("Error trying to acquire lock, retrying in 1s")
		} else {
			log.Debug().Bytes("token", token).Msg("Acquired lock")
			return token
		}
		select {
		case <-time.After(options.RetryInterval / 2):
		case <-ctx.Done():
			return nil
		}
	}
}

func acquireLockOnce(ctx context.Context, database LockingDatabase, name string, options LockOptions) ([]byte, error) {
	zerolog.Ctx(ctx).Trace().Str("name", name).Msg("Attempting to acquire lock...")

	db, prefix := database.GetLockPrimitives()

	if fut, err := util.DoWriteTransaction(ctx, db, func(txn fdb.Transaction) (any, error) {
		if txnIsLocked(ctx, txn, prefix, name) {
			return nil, errIsLocked
		}
		return txnCreateLock(txn, prefix, name, options.Timeout), nil
	}); err != nil {
		return nil, err
	} else {
		return fut.(fdb.FutureKey).MustGet(), nil
	}
}

func makeLock(
	ctx context.Context,
	database LockingDatabase,
	name string,
	options LockOptions,
	token []byte,
) Lock {
	log := zerolog.Ctx(ctx)
	db, prefix := database.GetLockPrimitives()

	refreshLockTxn := func(txn fdb.Transaction) {
		lockBytes := txn.Get(keyForLock(prefix, name)).MustGet()
		if lockBytes == nil {
			panic("lock has vanished")
		}

		// Bytes -> tuple -> versionstamp -> transaction version (the token)
		tup, _ := tuple.Unpack(lockBytes)
		vstamp := tup[0].(tuple.Versionstamp)
		vtoken := vstamp.TransactionVersion[:]

		if !bytes.Equal(vtoken, token) {
			panic(fmt.Sprintf("lock fencing token changed (got %b, expected %b)", vtoken, token))
		}

		txnUpdateLock(txn, prefix, name, options.Timeout)
	}

	// Both refreshLock & releaseLock use db.Transact directly not the util.DoWriteTransaction
	// wrapper used elsewhere. We don't want the ctx check as our ctx is likely canceled during
	// lock release. And we also don't want such verbose logging.

	refreshLock := func() {
		// Note: use of db.Transact not util.DoWriteTransaction
		_, err := db.Transact(func(txn fdb.Transaction) (any, error) {
			refreshLockTxn(txn)
			return nil, nil
		})
		if err != nil {
			panic(err)
		}
		log.Trace().
			Str("name", name).
			Bytes("token", token).
			Msg("Refreshed lock")
	}

	releaseLock := func() {
		// Note: use of db.Transact not util.DoWriteTransaction
		_, err := db.Transact(func(txn fdb.Transaction) (any, error) {
			txn.Clear(keyForLock(prefix, name))
			txn.Clear(keyForLockExpires(prefix, name))
			txn.Clear(keyForLockHostname(prefix, name))
			return nil, nil
		})
		if err != nil {
			panic(err)
		}
		log.Debug().
			Str("name", name).
			Bytes("token", token).
			Msg("Released lock")
	}

	return Lock{
		TxnRefresh: refreshLockTxn,
		Refresh:    refreshLock,
		Release:    releaseLock,
	}
}

func keyForLock(prefix subspace.Subspace, name string) fdb.Key {
	return prefix.Sub("lck").Pack(tuple.Tuple{name})
}

func keyForLockExpires(prefix subspace.Subspace, name string) fdb.Key {
	return prefix.Sub("exp").Pack(tuple.Tuple{name})
}

func keyForLockHostname(prefix subspace.Subspace, name string) fdb.Key {
	return prefix.Sub("hst").Pack(tuple.Tuple{name})
}

func txnIsLocked(ctx context.Context, txn fdb.Transaction, prefix subspace.Subspace, name string) bool {
	lockBytes := txn.Get(keyForLock(prefix, name)).MustGet()
	if lockBytes == nil {
		return false
	}

	expiresBytes := txn.Get(keyForLockExpires(prefix, name)).MustGet()
	tup, _ := tuple.Unpack(expiresBytes)

	expires := tup[0].(int64)
	if expires < time.Now().UTC().UnixMilli() {
		hostnameBytes := txn.Get(keyForLockHostname(prefix, name)).MustGet()
		hostnameTup, _ := tuple.Unpack(hostnameBytes)
		zerolog.Ctx(ctx).Warn().
			Str("name", name).
			Str("timed_out_hostname", hostnameTup[0].(string)).
			Msg("Found lock timed out")
		return false
	}

	return true
}

func txnCreateLock(txn fdb.Transaction, prefix subspace.Subspace, name string, timeout time.Duration) fdb.FutureKey {
	// Create the lock with the versionstamp as the fencing token
	txn.SetVersionstampedValue(
		keyForLock(prefix, name),
		types.MustVersionstampToBytes(tuple.IncompleteVersionstamp(0)),
	)

	// Set the hostname (purely for informational display)
	hostname, _ := os.Hostname()
	txn.Set(keyForLockHostname(prefix, name), tuple.Tuple{hostname}.Pack())

	// Update the log to set the timeout
	txnUpdateLock(txn, prefix, name, timeout)

	// Return a future to get the token after the transaction commits
	return txn.GetVersionstamp()
}

func txnUpdateLock(txn fdb.Transaction, prefix subspace.Subspace, name string, timeout time.Duration) {
	expireTime := time.Now().UTC().Add(timeout).UnixMilli()
	txn.Set(keyForLockExpires(prefix, name), tuple.Tuple{expireTime}.Pack())
}
