package remotedevices

import (
	"fmt"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/directory"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// RemoteDevicesDirectory caches the devices and cross-signing keys of remote users whose keys local
// clients have queried while sharing an encrypted room with them. A cache is created on the first such
// query and evicted once no snapshot or delta has been written to it for
// types.RemoteDeviceCacheRetention; key queries never write.
type RemoteDevicesDirectory struct {
	log        zerolog.Logger
	db         fdb.Database
	serverName string

	// Never deleted: the generation must outlive wipes and evictions
	//
	// key: id.UserID
	// value: meta
	meta subspace.Subspace

	// key: (id.UserID, id.DeviceID)
	// value: storedDevice
	devices subspace.Subspace

	// Separate rows keep each key object within FoundationDB's value limit
	//
	// key: (id.UserID, masterKeyRow | selfSigningKeyRow)
	// value: canonical JSON
	signingKeys subspace.Subspace

	// Recently accepted device-list stream IDs, including the snapshot anchor. The acceptance
	// sequence orders them, since stream IDs are opaque and one batch shares a millisecond.
	//
	// key: (id.UserID, stream ID)
	// value: (accepted at milliseconds, meta.AcceptSeq)
	streamIDs subspace.Subspace

	// key: id.UserID
	// value: (due milliseconds, attempts)
	jobs subspace.Subspace

	// key: (due milliseconds, id.UserID)
	// value: empty
	jobsByDue subspace.Subspace
}

func NewRemoteDevicesDirectory(
	logger zerolog.Logger,
	db fdb.Database,
	parentDir directory.Directory,
	serverName string,
) *RemoteDevicesDirectory {
	remoteDevicesDir, err := parentDir.CreateOrOpen(db, []string{"remotedevices"}, nil)
	if err != nil {
		panic(err)
	}

	log := logger.With().Str("directory", "remotedevices").Logger()
	log.Debug().
		Bytes("prefix", remoteDevicesDir.Bytes()).
		Msg("Init accounts/remotedevices directory")

	return &RemoteDevicesDirectory{
		log:        log,
		db:         db,
		serverName: serverName,

		meta:        remoteDevicesDir.Sub("met"),
		devices:     remoteDevicesDir.Sub("dev"),
		signingKeys: remoteDevicesDir.Sub("sgk"),
		streamIDs:   remoteDevicesDir.Sub("sid"),
		jobs:        remoteDevicesDir.Sub("job"),
		jobsByDue:   remoteDevicesDir.Sub("jdu"),
	}
}

func (r *RemoteDevicesDirectory) checkRemote(userID id.UserID) error {
	if userID.Homeserver() == r.serverName {
		return fmt.Errorf("userid is not remote: %s", userID)
	}
	return nil
}

func (r *RemoteDevicesDirectory) keyForMeta(userID id.UserID) fdb.Key {
	return r.meta.Pack(tuple.Tuple{userID.String()})
}

func (r *RemoteDevicesDirectory) keyForDevice(userID id.UserID, deviceID id.DeviceID) fdb.Key {
	return r.devices.Pack(tuple.Tuple{userID.String(), deviceID.String()})
}

func (r *RemoteDevicesDirectory) rangeForDevices(userID id.UserID) subspace.Subspace {
	return r.devices.Sub(userID.String())
}

func (r *RemoteDevicesDirectory) keyForSigningKey(userID id.UserID, row string) fdb.Key {
	return r.signingKeys.Pack(tuple.Tuple{userID.String(), row})
}

func (r *RemoteDevicesDirectory) rangeForSigningKeys(userID id.UserID) subspace.Subspace {
	return r.signingKeys.Sub(userID.String())
}

func (r *RemoteDevicesDirectory) keyForStreamID(userID id.UserID, streamID int64) fdb.Key {
	return r.streamIDs.Pack(tuple.Tuple{userID.String(), streamID})
}

func (r *RemoteDevicesDirectory) rangeForStreamIDs(userID id.UserID) subspace.Subspace {
	return r.streamIDs.Sub(userID.String())
}

func (r *RemoteDevicesDirectory) keyForJob(userID id.UserID) fdb.Key {
	return r.jobs.Pack(tuple.Tuple{userID.String()})
}

func (r *RemoteDevicesDirectory) keyForJobDue(job types.RemoteDeviceJob) fdb.Key {
	return r.jobsByDue.Pack(tuple.Tuple{job.DueAt.UnixMilli(), job.UserID.String()})
}
