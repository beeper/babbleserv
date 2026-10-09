package remotedevices

import (
	"encoding/json"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	masterKeyRow      = "m"
	selfSigningKeyRow = "s"
)

type meta struct {
	// Unix milliseconds the cache was created or last written by a snapshot or an accepted delta,
	// zero when the user is not cached. A cache idle for RemoteDeviceCacheRetention is evicted;
	// key queries never write.
	WrittenAt  int64 `msgpack:"wat"`
	Generation int64 `msgpack:"gen"`
	// Present once a complete snapshot is published; a snapshot may hold zero devices
	StreamID    *int64 `msgpack:"sid,omitempty"`
	DeviceCount int    `msgpack:"dct"`
	AcceptSeq   int64  `msgpack:"asq"`
}

func (m meta) cached() bool {
	return m.WrittenAt != 0
}

func (m meta) hasSnapshot() bool {
	return m.cached() && m.StreamID != nil
}

type storedDevice struct {
	DisplayName string `msgpack:"dn"`
	Keys        []byte `msgpack:"k,omitempty"`
}

type storedSigningKeys struct {
	MasterKey      []byte
	SelfSigningKey []byte
}

type signingKeysFuture struct {
	masterKey      fdb.FutureByteSlice
	selfSigningKey fdb.FutureByteSlice
}

func (f signingKeysFuture) mustGet() storedSigningKeys {
	return storedSigningKeys{MasterKey: f.masterKey.MustGet(), SelfSigningKey: f.selfSigningKey.MustGet()}
}

func mustMarshal(v any) []byte {
	b, err := msgpack.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mustUnmarshal(b []byte, v any) {
	if err := msgpack.Unmarshal(b, v); err != nil {
		panic(err)
	}
}

func decodeMeta(b []byte) meta {
	var m meta
	if b != nil {
		mustUnmarshal(b, &m)
	}
	return m
}

func decodeDevice(b []byte) *types.RemoteDevice {
	if b == nil {
		return nil
	}
	var stored storedDevice
	mustUnmarshal(b, &stored)
	return &types.RemoteDevice{DisplayName: stored.DisplayName, Keys: json.RawMessage(stored.Keys)}
}

func (r *RemoteDevicesDirectory) txnWriteMeta(txn fdb.Transaction, userID id.UserID, m meta) {
	txn.Set(r.keyForMeta(userID), mustMarshal(m))
}

func (r *RemoteDevicesDirectory) txnStoreDevice(txn fdb.Transaction, userID id.UserID, deviceID id.DeviceID, device types.RemoteDevice) {
	txn.Set(r.keyForDevice(userID, deviceID), mustMarshal(storedDevice{DisplayName: device.DisplayName, Keys: device.Keys}))
}

func (r *RemoteDevicesDirectory) getSigningKeys(txn fdb.ReadTransaction, userID id.UserID) signingKeysFuture {
	return signingKeysFuture{
		masterKey:      txn.Get(r.keyForSigningKey(userID, masterKeyRow)),
		selfSigningKey: txn.Get(r.keyForSigningKey(userID, selfSigningKeyRow)),
	}
}

func (r *RemoteDevicesDirectory) txnStoreSigningKeys(txn fdb.Transaction, userID id.UserID, keys storedSigningKeys) {
	r.txnStoreSigningKey(txn, userID, masterKeyRow, keys.MasterKey)
	r.txnStoreSigningKey(txn, userID, selfSigningKeyRow, keys.SelfSigningKey)
}

func (r *RemoteDevicesDirectory) txnStoreSigningKey(txn fdb.Transaction, userID id.UserID, row string, key []byte) {
	if len(key) == 0 {
		txn.Clear(r.keyForSigningKey(userID, row))
	} else {
		txn.Set(r.keyForSigningKey(userID, row), key)
	}
}

// txnStoreStreamID records an accepted stream ID, ordered after every earlier acceptance
func (r *RemoteDevicesDirectory) txnStoreStreamID(txn fdb.Transaction, userID id.UserID, m *meta, streamID int64, now time.Time) acceptance {
	m.AcceptSeq++
	accepted := acceptance{at: time.UnixMilli(now.UnixMilli()).UTC(), seq: m.AcceptSeq}
	txn.Set(r.keyForStreamID(userID, streamID), tuple.Tuple{accepted.at.UnixMilli(), accepted.seq}.Pack())
	return accepted
}

func (r *RemoteDevicesDirectory) txnClearData(txn fdb.Transaction, userID id.UserID, m *meta) {
	txn.ClearRange(r.rangeForDevices(userID))
	txn.ClearRange(r.rangeForStreamIDs(userID))
	txn.ClearRange(r.rangeForSigningKeys(userID))
	m.StreamID = nil
	m.DeviceCount = 0
}
