package transient

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/rs/zerolog"
	"golang.org/x/exp/maps"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type RejectedToDeviceEvent struct {
	ToDeviceEvent *types.ToDevice
	Error         error
}

type SendToDeviceResults struct {
	versionstampFut fdb.FutureKey
	change          notifier.Change

	Allowed  []*types.ToDevice
	Rejected []RejectedToDeviceEvent
}

type SendToDeviceOptions struct {
	// Both TransactionID + DeviceID must be set together to act as the idempotency token
	TransactionID string
	DeviceID      id.DeviceID
}

func (t *TransientDatabase) SendToDeviceEvents(
	ctx context.Context,
	tds []*types.ToDevice,
	options SendToDeviceOptions,
) (*SendToDeviceResults, error) {
	log := zerolog.Ctx(ctx).With().
		Str("component", "database").
		Str("database", "todevice").
		Str("transaction", "SendToDeviceEvents").
		Int("events", len(tds)).
		Logger()

	res, err := util.DoWriteTransactionWithVersion(ctx, t.db, func(txn fdb.Transaction) (*SendToDeviceResults, error) {
		allowedEvents := make([]*types.ToDevice, 0, len(tds))
		rejectedEvents := make([]RejectedToDeviceEvent, 0)

		changedUsers := make(map[id.UserID]struct{}, len(tds))
		changedServers := make(map[string]struct{}, len(tds))

		for i, tdev := range tds {
			log.Trace().Any("to_device", tdev).Msg("Storing to-device event")

			if options.TransactionID != "" {
				key := t.todevice.KeyForLocalUserTransaction(tdev.Sender, options.DeviceID, options.TransactionID)
				if txn.Get(key).MustGet() != nil {
					log.Warn().Msg("Ignored duplicate to-device transaction ID")
					// allowedEvents = append(allowedEvents, tdev)
					continue
				}
			}

			tdBytes := tdev.Bytes()
			version := tuple.IncompleteVersionstamp(uint16(i))

			serverName := tdev.UserID.Homeserver()
			if serverName == t.config.ServerName {
				if tdev.DeviceID == "*" {
					// This would require crossing the transient/accounts database boundary, should
					// be performed when generating the todevice input.
					panic("cannot user * DeviceID in SendToDeviceEvents")
				}
				changedUsers[tdev.UserID] = struct{}{}
				key := t.todevice.KeyForLocalUserVersion(tdev.UserID, tdev.DeviceID, version)
				txn.SetVersionstampedKey(key, tdBytes)
			} else {
				changedServers[serverName] = struct{}{}
				// For remote servers we just send it as-is
				key := t.todevice.KeyForRemoteServerVersion(serverName, version)
				txn.SetVersionstampedKey(key, tdBytes)
			}

			allowedEvents = append(allowedEvents, tdev)
		}

		if options.TransactionID != "" {
			for _, tdev := range tds {
				key := t.todevice.KeyForLocalUserTransaction(tdev.Sender, options.DeviceID, options.TransactionID)
				txn.Set(key, []byte{})
			}
		}

		change := notifier.Change{
			UserIDs: maps.Keys(changedUsers),
			Servers: maps.Keys(changedServers),
		}

		return &SendToDeviceResults{
			versionstampFut: txn.GetVersionstamp(),
			change:          change,

			Allowed:  allowedEvents,
			Rejected: rejectedEvents,
		}, nil
	})
	if err != nil {
		return nil, err
	}

	t.notifier.SendChange(res.change)

	for _, ev := range res.Rejected {
		log.Warn().Err(ev.Error).
			Str("user_id", ev.ToDeviceEvent.UserID.String()).
			Str("device_id", ev.ToDeviceEvent.DeviceID.String()).
			Msg("To-device rejected")
	}

	rlog := log.Info().
		Object("change", res.change).
		Int("events_allowed", len(res.Allowed)).
		Int("events_rejected", len(res.Rejected))
	if len(res.Allowed) > 0 {
		rlog = rlog.Str("versionstamp", res.versionstampFut.MustGet().String())
	}
	rlog.Msg("Sent to-device events")

	return res, nil
}
