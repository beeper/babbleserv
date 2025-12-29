package todevice

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// The to-device directory holds versioned to-device events stored by user/device or server for
// pagination during sync or federation sending. We also use these to store and sync/federate device
// list changes (m.device_list_update EDU) and signing key updates (m.signing_key_update EDU).
type ToDeviceDirectory struct {
	log zerolog.Logger

	// Version to to-device details, acts as a global index so we can drop stale records, which
	// build up as devices go missing/etc. These are not cleared when removing user/server messages
	// during sync, but when dropping stale records.
	//
	// key: Versionstamp
	// value: ("server", ServerName) OR ("user", UserID, DeviceID, TransactionID)
	versionToMessage subspace.Subspace

	// To-device messages for local user devices
	//
	// key: (UserID, DeviceID, Version)
	// value: types.ToDevice
	localUserMessages subspace.Subspace

	// Used to-device transaction IDs
	//
	// key: (UserID, DeviceID, TransactionID)
	// value: []byte (always empty)
	localUserMessageTxns subspace.Subspace

	// To-device messages for other user devices on other homeservers
	//
	// key: (ServerName, version)
	// value: types.ToDevice (as msgpack []byte)
	remoteServerMessages subspace.Subspace
}

func NewToDeviceDirectory(logger zerolog.Logger, db fdb.Database, parentDir directory.Directory) *ToDeviceDirectory {
	toDeviceDir, err := parentDir.CreateOrOpen(db, []string{"todevice"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "todevice").Logger()
	log.Debug().
		Bytes("prefix", toDeviceDir.Bytes()).
		Msg("Init transient/todevice directory")

	return &ToDeviceDirectory{
		log: log,

		versionToMessage:     toDeviceDir.Sub("vtm"),
		localUserMessages:    toDeviceDir.Sub("lum"),
		localUserMessageTxns: toDeviceDir.Sub("lut"),
		remoteServerMessages: toDeviceDir.Sub("rsm"),
	}
}

func (t *ToDeviceDirectory) RangeForLocalUserVersion(
	userID id.UserID,
	deviceID id.DeviceID,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.ExactRange {
	return types.GetVersionRange(t.localUserMessages, fromVersion, toVersion, userID.String(), deviceID.String())
}

func (t *ToDeviceDirectory) KeyToLocalUserVersion(key fdb.Key) tuple.Versionstamp {
	tup, _ := t.localUserMessages.Unpack(key)
	return tup[2].(tuple.Versionstamp)
}

func (t *ToDeviceDirectory) keyForLocalUserVersion(
	userID id.UserID,
	deviceID id.DeviceID,
	version tuple.Versionstamp,
) fdb.Key {
	tup := tuple.Tuple{userID.String(), deviceID.String(), version}
	key, err := t.localUserMessages.PackWithVersionstamp(tup)
	if err != nil {
		panic(err)
	}
	return key
}

func (t *ToDeviceDirectory) KeyForLocalUserTransaction(
	userID id.UserID,
	deviceID id.DeviceID,
	transactionID string,
) fdb.Key {
	return t.localUserMessageTxns.Pack(tuple.Tuple{userID.String(), deviceID.String(), transactionID})
}

func (t *ToDeviceDirectory) RangeForRemoteServerVersion(
	serverName string,
	fromVersion, toVersion tuple.Versionstamp,
) fdb.ExactRange {
	return types.GetVersionRange(t.remoteServerMessages, fromVersion, toVersion, serverName)
}

func (t *ToDeviceDirectory) KeyToRemoteServerVersion(key fdb.Key) tuple.Versionstamp {
	tup, err := t.remoteServerMessages.Unpack(key)
	if err != nil {
		panic(err)
	}
	return tup[1].(tuple.Versionstamp)
}

func (t *ToDeviceDirectory) keyForRemoteServerVersion(
	serverName string,
	version tuple.Versionstamp,
) fdb.Key {
	tup := tuple.Tuple{serverName, version}
	key, err := t.remoteServerMessages.PackWithVersionstamp(tup)
	if err != nil {
		panic(err)
	}
	return key
}

func (t *ToDeviceDirectory) keyForVersionToMessage(version tuple.Versionstamp) fdb.Key {
	key, err := t.versionToMessage.PackWithVersionstamp(tuple.Tuple{version})
	if err != nil {
		panic(err)
	}
	return key
}

func (t *ToDeviceDirectory) TxnStoreLocalUserVersion(txn fdb.Transaction, toDevice *types.ToDevice, version tuple.Versionstamp, transactionID string) {
	key := t.keyForLocalUserVersion(toDevice.UserID, toDevice.DeviceID, version)
	txn.SetVersionstampedKey(key, toDevice.Bytes())

	versionTuple := tuple.Tuple{"user", toDevice.UserID.String(), toDevice.DeviceID.String(), transactionID}
	txn.SetVersionstampedKey(t.keyForVersionToMessage(version), versionTuple.Pack())
}

func (t *ToDeviceDirectory) TxnStoreRemoteServerVersion(txn fdb.Transaction, toDevice *types.ToDevice, version tuple.Versionstamp) {
	key := t.keyForRemoteServerVersion(toDevice.UserID.Homeserver(), version)
	txn.SetVersionstampedKey(key, toDevice.Bytes())

	versionTuple := tuple.Tuple{"server", toDevice.UserID.Homeserver()}
	txn.SetVersionstampedKey(t.keyForVersionToMessage(version), versionTuple.Pack())
}
