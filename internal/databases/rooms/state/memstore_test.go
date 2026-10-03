package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

type memStore struct {
	pages       map[string][]byte
	contexts    map[string][]byte
	resolutions map[string][]byte

	pageReads, pageReadCalls, contextReads, contextReadCalls int
	pageWrites, contextWrites                                int

	// What each committed staging transaction wrote, in order
	stages [][]stagedRecord
	// The staging transaction that fails, counting from 1, or 0 for none
	failingStage, stageCalls int
}

type recordKind int

const (
	pageRecord recordKind = iota
	contextRecord
)

type stagedRecord struct {
	hash types.StateHash
	kind recordKind
	raw  []byte
}

func newMemStore() *memStore {
	return &memStore{
		pages:       make(map[string][]byte),
		contexts:    make(map[string][]byte),
		resolutions: make(map[string][]byte),
	}
}

func (m *memStore) room(roomID id.RoomID) memRoom {
	return memRoom{m, roomID}
}

func (m *memStore) reader(_ fdb.ReadTransaction, roomID id.RoomID) storeReader {
	return m.room(roomID)
}

func (m *memStore) writer(_ fdb.Transaction, roomID id.RoomID) storeWriter {
	return m.room(roomID)
}

func (m *memStore) stage(_ context.Context, roomID id.RoomID, fn func(storeWriter) error) error {
	m.stageCalls++
	if m.stageCalls == m.failingStage {
		return errStageFailed
	}
	w := &stagingWriter{memRoom: m.room(roomID)}
	if err := fn(w); err != nil {
		return err
	}
	m.stages = append(m.stages, w.records)
	return nil
}

func (m *memStore) keySize(roomID id.RoomID) int {
	return len(m.room(roomID).key(types.StateHash{}))
}

func (m *memStore) resetCounts() {
	m.pageReads, m.pageReadCalls, m.contextReads, m.contextReadCalls = 0, 0, 0, 0
}

type memRoom struct {
	*memStore
	roomID id.RoomID
}

func (m memRoom) key(h types.StateHash) string {
	return m.roomID.String() + "/" + string(h[:])
}

func (m memRoom) cacheable() bool {
	return true
}

func (m memRoom) readPages(hashes []types.StateHash) ([][]byte, error) {
	m.pageReads += len(hashes)
	m.pageReadCalls++
	return m.getAll(m.pages, hashes), nil
}

func (m memRoom) readContexts(ctxs []types.StateHash) ([][]byte, error) {
	m.contextReads += len(ctxs)
	m.contextReadCalls++
	return m.getAll(m.contexts, ctxs), nil
}

func (m memRoom) getAll(records map[string][]byte, hashes []types.StateHash) [][]byte {
	raws := make([][]byte, len(hashes))
	for i, h := range hashes {
		raws[i] = records[m.key(h)]
	}
	return raws
}

func (m memRoom) readResolution(key types.StateHash) func() ([]byte, error) {
	raw := m.resolutions[m.key(key)]
	return func() ([]byte, error) { return raw, nil }
}

func (m memRoom) writeResolution(key types.StateHash, raw []byte) {
	put(m.resolutions, m.key(key), raw)
}

func (m memRoom) writePage(h types.StateHash, raw []byte) {
	m.pageWrites++
	put(m.pages, m.key(h), raw)
}

func (m memRoom) writeContext(ctx types.StateHash, raw []byte) {
	m.contextWrites++
	put(m.contexts, m.key(ctx), raw)
}

var errStageFailed = errors.New("staging transaction failed")

type stagingWriter struct {
	memRoom
	records []stagedRecord
}

func (w *stagingWriter) writePage(h types.StateHash, raw []byte) {
	w.records = append(w.records, stagedRecord{hash: h, kind: pageRecord, raw: raw})
	w.memRoom.writePage(h, raw)
}

func (w *stagingWriter) writeContext(ctx types.StateHash, raw []byte) {
	w.records = append(w.records, stagedRecord{hash: ctx, kind: contextRecord, raw: raw})
	w.memRoom.writeContext(ctx, raw)
}

func put(records map[string][]byte, key string, raw []byte) {
	if existing, ok := records[key]; ok && !bytes.Equal(existing, raw) {
		panic(fmt.Sprintf("immutable record %x rewritten with different bytes", key))
	}
	records[key] = bytes.Clone(raw)
}

// uncommitted reads from the store but drops every write, like a transaction that never commits.
type uncommitted struct {
	*memStore
}

func (uncommitted) writer(fdb.Transaction, id.RoomID) storeWriter {
	return droppedWrites{}
}

type droppedWrites struct{}

func (droppedWrites) writePage(types.StateHash, []byte) {}

func (droppedWrites) writeContext(types.StateHash, []byte) {}

func (droppedWrites) writeResolution(types.StateHash, []byte) {}

// openTxn is a transaction that has not committed over the store: it reads back its own writes, as
// a FoundationDB transaction does, and is not cacheable once it wrote.
type openTxn struct {
	*memStore
	writes *memStore
	wrote  bool
}

func newOpenTxn(store *memStore) *openTxn {
	return &openTxn{memStore: store, writes: newMemStore()}
}

func (t *openTxn) reader(_ fdb.ReadTransaction, roomID id.RoomID) storeReader {
	return openTxnReader{memRoom: t.room(roomID), writes: t.writes.room(roomID), committed: !t.wrote}
}

func (t *openTxn) writer(_ fdb.Transaction, roomID id.RoomID) storeWriter {
	t.wrote = true
	return t.writes.room(roomID)
}

type openTxnReader struct {
	memRoom
	writes    memRoom
	committed bool
}

func (r openTxnReader) cacheable() bool {
	return r.committed
}

func (r openTxnReader) readPages(hashes []types.StateHash) ([][]byte, error) {
	return r.overlay(r.memRoom.readPages, r.writes.readPages, hashes)
}

func (r openTxnReader) readContexts(ctxs []types.StateHash) ([][]byte, error) {
	return r.overlay(r.memRoom.readContexts, r.writes.readContexts, ctxs)
}

func (r openTxnReader) overlay(stored, written func([]types.StateHash) ([][]byte, error), hashes []types.StateHash) ([][]byte, error) {
	raws, err := stored(hashes)
	if err != nil {
		return nil, err
	}
	own, err := written(hashes)
	for i, raw := range own {
		if raw != nil {
			raws[i] = raw
		}
	}
	return raws, err
}
