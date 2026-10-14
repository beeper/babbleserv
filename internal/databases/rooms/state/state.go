package state

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/stateres"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// StateDirectory stores immutable map pages and contexts identified by their state roots.
type StateDirectory struct {
	log zerolog.Logger
	db  fdb.Database

	// Context records, the state at a point in the room DAG as the roots of its state and member
	// maps. The empty context is never stored.
	//
	// key: (RoomID, ContextID)
	// value: (StateRoot, MemberRoot)
	idToContext subspace.Subspace

	// Map pages. A leaf holds entries sorted by key. A branch holds its subtree's entry count and
	// encoded entry bytes, then the child page for each nibble of the key bucket hash with "" for
	// an empty slot.
	//
	// key: (RoomID, PageID)
	// value: (0, key, value, key, value, ...) or (1, count, bytes, child0, ..., child15)
	idToPage subspace.Subspace

	// Results of resolutions too large to run within a send's transactions, stored by the artifact
	// job that ran them once the result context is stored. A resolution key is the hash of the
	// sorted distinct input context IDs, under the version of state resolution that resolved them.
	//
	// key: (RoomID, stateres.Version, ResolutionKey)
	// value: (ContextID)
	idToResolution subspace.Subspace

	cache   *cache
	written writtenTxns
}

func NewStateDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *StateDirectory {
	stateDir, err := parentDir.CreateOrOpen(db, []string{"state"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "state").Logger()
	log.Debug().
		Bytes("prefix", stateDir.Bytes()).
		Msg("Init rooms/state directory")

	return &StateDirectory{
		log: log,
		db:  db,

		idToContext:    stateDir.Sub("cx"),
		idToPage:       stateDir.Sub("ob"),
		idToResolution: stateDir.Sub("rs"),

		cache: newCache(defaultCacheBytes),
	}
}

func (s *StateDirectory) NewBatch(roomID id.RoomID) *Batch {
	return newBatch(s, s.cache, roomID, defaultLeafLimits)
}

// NewJobBatch returns a batch for an artifact job, running each batch of reads through read,
// whatever transaction its methods are given, so its reads may span transactions.
func (s *StateDirectory) NewJobBatch(roomID id.RoomID, read func(func(fdb.ReadTransaction) error) error) *Batch {
	return newBatch(jobStorage{storage: s, read: read}, s.cache, roomID, defaultLeafLimits)
}

func (s *StateDirectory) keyForContext(roomID id.RoomID, ctx types.StateHash) fdb.Key {
	return s.idToContext.Pack(tuple.Tuple{roomID.String(), ctx[:]})
}

func (s *StateDirectory) keyForPage(roomID id.RoomID, h types.StateHash) fdb.Key {
	return s.idToPage.Pack(tuple.Tuple{roomID.String(), h[:]})
}

func (s *StateDirectory) keyForResolution(roomID id.RoomID, key types.StateHash) fdb.Key {
	return s.idToResolution.Pack(tuple.Tuple{roomID.String(), int64(stateres.Version), key[:]})
}

func (s *StateDirectory) reader(txn fdb.ReadTransaction, roomID id.RoomID) storeReader {
	return txnReader{dir: s, txn: txn.Snapshot(), roomID: roomID, committed: !s.written.has(txnID(txn))}
}

func (s *StateDirectory) writer(txn fdb.Transaction, roomID id.RoomID) storeWriter {
	return &txnWriter{dir: s, txn: txn, roomID: roomID}
}

func (s *StateDirectory) stage(ctx context.Context, roomID id.RoomID, fn func(storeWriter) error) error {
	_, err := util.DoWriteTransaction(ctx, s.db, func(txn fdb.Transaction) (types.Nil, error) {
		return nil, fn(s.writer(txn, roomID))
	})
	return err
}

func (s *StateDirectory) keySize(roomID id.RoomID) int {
	return len(s.keyForPage(roomID, hashOf(nil)))
}

// txnID identifies a transaction, from any view of it, by the address of its handle, without
// keeping it alive.
func txnID(txn fdb.ReadTransaction) uintptr {
	return reflect.ValueOf(txn.Snapshot()).Field(0).Pointer()
}

// writtenTxns are the transactions state rows were written in, whose reads are never cached as they
// read back writes not yet committed. One is forgotten at the second rotation after its write, a
// rotation period being longer than a transaction can read for, and until then a later transaction
// reusing its address does not cache either.
type writtenTxns struct {
	mu                sync.RWMutex
	rotated           time.Time
	current, previous map[uintptr]struct{}
}

const writtenTxnsRotation = 10 * time.Second

func (w *writtenTxns) add(txn uintptr) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now := time.Now(); w.current == nil || now.Sub(w.rotated) >= writtenTxnsRotation {
		w.previous, w.current, w.rotated = w.current, make(map[uintptr]struct{}), now
	}
	w.current[txn] = struct{}{}
}

func (w *writtenTxns) has(txn uintptr) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, inCurrent := w.current[txn]
	_, inPrevious := w.previous[txn]
	return inCurrent || inPrevious
}

type txnReader struct {
	dir    *StateDirectory
	txn    fdb.ReadTransaction
	roomID id.RoomID
	// No state rows were written in the transaction, so everything it reads is committed
	committed bool
}

func (r txnReader) cacheable() bool {
	return r.committed
}

func (r txnReader) readPages(hashes []types.StateHash) ([][]byte, error) {
	return r.getAll(hashes, r.dir.keyForPage)
}

func (r txnReader) readContexts(ctxs []types.StateHash) ([][]byte, error) {
	return r.getAll(ctxs, r.dir.keyForContext)
}

func (r txnReader) readResolution(key types.StateHash) func() ([]byte, error) {
	return r.txn.Get(r.dir.keyForResolution(r.roomID, key)).Get
}

func (r txnReader) getAll(hashes []types.StateHash, keyFor func(id.RoomID, types.StateHash) fdb.Key) ([][]byte, error) {
	futures := make([]fdb.FutureByteSlice, len(hashes))
	for i, h := range hashes {
		futures[i] = r.txn.Get(keyFor(r.roomID, h))
	}
	values := make([][]byte, len(hashes))
	for i, future := range futures {
		value, err := future.Get()
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, nil
}

type txnWriter struct {
	dir    *StateDirectory
	txn    fdb.Transaction
	roomID id.RoomID
	wrote  bool
}

// set records the transaction as written in before its first write.
func (w *txnWriter) set(key fdb.Key, value []byte) {
	if !w.wrote {
		w.dir.written.add(txnID(w.txn))
		w.wrote = true
	}
	w.txn.Set(key, value)
}

func (w *txnWriter) writePage(h types.StateHash, raw []byte) {
	w.set(w.dir.keyForPage(w.roomID, h), raw)
}

func (w *txnWriter) writeContext(ctx types.StateHash, raw []byte) {
	w.set(w.dir.keyForContext(w.roomID, ctx), raw)
}

func (w *txnWriter) writeResolution(key types.StateHash, raw []byte) {
	w.set(w.dir.keyForResolution(w.roomID, key), raw)
}

// Pages and contexts a job batch reads per batch of reads, so a batch finishes well within
// FoundationDB's five seconds: pages are at most 64 KiB, 8 MiB a batch, and a context row is under
// 100 bytes.
const (
	jobPageBatch    = 128
	jobContextBatch = 1000
)

type jobStorage struct {
	storage
	read func(func(fdb.ReadTransaction) error) error
}

func (s jobStorage) reader(_ fdb.ReadTransaction, roomID id.RoomID) storeReader {
	return jobReader{store: s.storage, roomID: roomID, read: s.read}
}

type jobReader struct {
	store  storage
	roomID id.RoomID
	read   func(func(fdb.ReadTransaction) error) error
}

// A job's transactions only read
func (r jobReader) cacheable() bool {
	return true
}

func (r jobReader) readPages(hashes []types.StateHash) ([][]byte, error) {
	return r.readInBatches(hashes, jobPageBatch, storeReader.readPages)
}

func (r jobReader) readContexts(ctxs []types.StateHash) ([][]byte, error) {
	return r.readInBatches(ctxs, jobContextBatch, storeReader.readContexts)
}

func (r jobReader) readInBatches(
	hashes []types.StateHash,
	size int,
	readBatch func(storeReader, []types.StateHash) ([][]byte, error),
) ([][]byte, error) {
	raws := make([][]byte, 0, len(hashes))
	for part := range slices.Chunk(hashes, size) {
		if err := r.read(func(txn fdb.ReadTransaction) error {
			partRaws, err := readBatch(r.store.reader(txn, r.roomID), part)
			if err == nil {
				raws = append(raws, partRaws...)
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	return raws, nil
}

func (r jobReader) readResolution(key types.StateHash) func() ([]byte, error) {
	var raw []byte
	err := r.read(func(txn fdb.ReadTransaction) (err error) {
		raw, err = r.store.reader(txn, r.roomID).readResolution(key)()
		return err
	})
	return func() ([]byte, error) { return raw, err }
}
