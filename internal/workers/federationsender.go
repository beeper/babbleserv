package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"go.mau.fi/util/exerrors"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

type FederationSender struct {
	log       zerolog.Logger
	config    config.BabbleConfig
	db        *databases.Databases
	notifiers *notifier.Notifiers
	fclient   fclient.FederationClient

	// Internal map + lock of active senders we have running in this process
	lock          sync.RWMutex
	serverSenders map[string]chan struct{}

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func NewFederationSender(
	logger zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notifiers *notifier.Notifiers,
	fclient fclient.FederationClient,
) *FederationSender {
	log := logger.With().
		Str("worker", "FederationSender").
		Logger()

	return &FederationSender{
		log:           log,
		config:        cfg,
		db:            db,
		notifiers:     notifiers,
		fclient:       fclient,
		serverSenders: make(map[string]chan struct{}),
	}
}

func (fs *FederationSender) Start() {
	var ctx context.Context
	ctx, fs.cancel = context.WithCancel(fs.log.WithContext(context.Background()))

	initialServerNames, err := fs.db.System.GetServerNamesWithPositions(ctx)
	if err != nil {
		panic(fmt.Errorf("failed to get initial servers: %w", err))
	}

	fs.log.Info().
		Int("initial_servers", len(initialServerNames)).
		Msg("Starting federation sender...")

	go fs.handleServersLoop(ctx, initialServerNames)
}

func (fs *FederationSender) Stop() {
	fs.log.Debug().Msg("Stopping federation sender")
	fs.cancel()
	fs.wg.Wait()
	fs.log.Info().Msg("Federation sender stopped")
}

func (fs *FederationSender) handleServersLoop(ctx context.Context, initialServerNames []string) {
	fs.wg.Add(1)
	defer fs.wg.Done()

	newServersCh := make(chan any, 1000)
	fs.notifiers.SubscribeWithChannel(newServersCh, notifier.Subscription{AllServers: true})
	defer fs.notifiers.Unsubscribe(newServersCh)

	// Kick off a goroutine to push our initial servers into the queue
	go func() {
		for _, name := range initialServerNames {
			newServersCh <- name
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case server := <-newServersCh:
			serverName := server.(string)

			log := fs.log.With().
				Str("server", serverName).
				Logger()
			srvCtx := log.WithContext(ctx)

			if serverName == fs.config.ServerName {
				log.Warn().Str("server", serverName).Msg("Ignoring change from ourselves")
				continue
			}

			// First check our in memory map of active senders, avoid the FDB lock
			// entirely if we're already running this sender.
			fs.lock.RLock()
			ch, found := fs.serverSenders[serverName]
			select {
			// Wakeup the sender if needed
			case ch <- struct{}{}:
			default:
			}
			fs.lock.RUnlock()

			if found {
				log.Debug().
					Str("server", serverName).
					Msg("We are already running this server sender")
			} else {
				fs.wg.Add(1)
				go func() {
					fs.maybeRunServerSender(srvCtx, serverName)
					fs.wg.Done()
				}()
			}
		}
	}
}

const (
	serverSenderLockNamePrefix = "FederationServerSenderLock:"
	serverSenderLockTimeout    = time.Second * 60
)

func (fs *FederationSender) maybeRunServerSender(ctx context.Context, serverName string) {
	log := zerolog.Ctx(ctx)

	lockName := serverSenderLockNamePrefix + serverName

	lockOpts := lock.LockOptions{
		Timeout: serverSenderLockTimeout,
	}

	if hadLock, err := lock.WithLockIfAvailable(ctx, fs.db.System, lockName, lockOpts, func(lock lock.Lock) {
		wakeCh := make(chan struct{}, 1)

		// Store internal flag that we're running this sender
		fs.lock.Lock()
		fs.serverSenders[serverName] = wakeCh
		fs.lock.Unlock()

		log.Info().Msg("Starting server sender")
		fs.sendTransactionLoop(ctx, serverName, lock, wakeCh)

		// Remove the internal flag on sender
		fs.lock.Lock()
		delete(fs.serverSenders, serverName)
		fs.lock.Unlock()

		lock.Release()
		log.Info().Msg("Server sender stopped without error")
	}); err != nil {
		log.Err(err).Msg("Error starting server sender")
		return
	} else if !hadLock {
		log.Debug().
			Str("server", serverName).
			Msg("Someone else is already running this server sender")
	}
}

func (fs *FederationSender) sendTransactionLoop(
	ctx context.Context,
	serverName string,
	lock lock.Lock,
	wakeCh chan struct{},
) {
	var noSends int
	var errCount int
	var delayS time.Duration = 1

	log := zerolog.Ctx(ctx)

	trySend := func() {
		sent, err := fs.sendTransaction(ctx, serverName, lock)
		if err != nil {
			log.Err(err).
				Dur("delay", delayS*time.Second).
				Msg("Failed to send events to server, will retry")
			errCount++
			delayS = time.Duration(errCount)
			return
		}
		errCount = 0
		if sent {
			noSends = 0
		} else {
			noSends++
		}
	}

	trySend()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wakeCh:
			trySend()
		case <-time.After(delayS * time.Second):
			trySend()
		}
		// TODO: this is stupid
		if noSends >= 10 {
			// After 10 refreshes without sends, exit the server sender. If new
			// events come in relevant to this server we'll start again.
			return
		}
	}
}

func (fs *FederationSender) sendTransaction(ctx context.Context, serverName string, lock lock.Lock) (bool, error) {
	serverVersions, err := fs.db.System.GetServerPositions(ctx, serverName)
	if err != nil {
		return false, err
	} else if serverVersions == nil {
		serverVersions = make(types.VersionMap)
	}

	// Flip between rooms + transient syncs
	isRooms := true
	didSend := true

	for {
		lock.Refresh()

		var sent bool
		var err error
		if isRooms {
			sent, err = fs.syncRoomsForServer(ctx, serverName, serverVersions)
		} else {
			sent, err = fs.syncTransientForServer(ctx, serverName, serverVersions)
		}
		if err != nil {
			return false, err
		}

		if !didSend && !sent {
			// If we didn't send last time and we didn't send this time, exit
			return false, nil
		}

		err = fs.db.System.UpdateServerPositions(ctx, serverName, serverVersions, lock.TxnRefresh)
		if err != nil {
			return false, err
		}

		isRooms = !isRooms
		didSend = sent
	}
}

func (fs *FederationSender) syncRoomsForServer(
	ctx context.Context,
	serverName string,
	serverVersions types.VersionMap,
) (bool, error) {
	log := zerolog.Ctx(ctx)

	roomsVersion, found := serverVersions[types.RoomsVersionKey]
	if !found {
		roomsVersion = types.ZeroVersionstamp
		// Important to bump this so we only ever do incremental syncs for servers, which get
		// the initial state via the federation join exchange.
		roomsVersion.UserVersion += 1
	}

	log.Debug().
		Any("version_from", roomsVersion).
		Msg("Syncing rooms for server")

	nextVersion, rooms, err := fs.db.Rooms.SyncRoomsForServer(ctx, serverName, roomsVersion, types.SyncOptions{
		// Servers need to send everything, no gaps
		Mode: types.SyncModeStreaming,
	})
	if err != nil {
		return false, err
	}

	if nextVersion == roomsVersion {
		return false, nil
	}

	allEvs := make([]*types.Event, 0, 50)
	allReceipts := make([]*types.EDU, 0, 50)

	for _, room := range rooms {
		allEvs = append(allEvs, room.StateEvents.Events...)
		allEvs = append(allEvs, room.TimelineEvents.Events...)

		for _, rc := range room.Receipts {
			if rc.Type != event.ReceiptTypeRead {
				panic("servers should never see private read receipts")
			}

			content := types.ReceiptEDUContent{
				rc.RoomID: {
					event.ReceiptTypeRead: {
						rc.UserID: {
							EventIDs: []id.EventID{rc.EventID},
							Data: types.ReceiptEDUData{
								ThreadID: rc.ThreadID,
								TS:       rc.Timestamp,
							},
						},
					},
				},
			}
			b, err := json.Marshal(content)
			if err != nil {
				return false, err
			}

			// TODO: we could merge receipts with the same room/type
			allReceipts = append(allReceipts, &types.EDU{
				Type:    types.EDUTypeReceipt,
				Content: b,
			})
		}
	}

	if len(allEvs) > 0 || len(allReceipts) > 0 {
		if err := fs.sendTransactionToServer(ctx, serverName, roomsVersion, allEvs, allReceipts); err != nil {
			return false, fmt.Errorf("failed to send rooms transaction: %w", err)
		}
	}

	serverVersions[types.RoomsVersionKey] = nextVersion
	return true, nil
}

func (fs *FederationSender) syncTransientForServer(
	ctx context.Context,
	serverName string,
	serverVersions types.VersionMap,
) (bool, error) {
	log := zerolog.Ctx(ctx)

	roomsVersion, found := serverVersions[types.TransientVersionKey]
	if !found {
		roomsVersion = types.ZeroVersionstamp
		// Important to bump this so we only ever do incremental syncs for servers, which get
		// the initial state via the federation join exchange.
		roomsVersion.UserVersion += 1
	}

	log.Debug().
		Any("version_from", roomsVersion).
		Msg("Syncing transient for server")

	nextVersion, toDevice, err := fs.db.Transient.SyncTransientForServer(ctx, serverName, roomsVersion, types.SyncOptions{
		// Servers need to send everything, no gaps
		Mode: types.SyncModeStreaming,
	})
	if err != nil {
		return false, err
	}

	if nextVersion == roomsVersion {
		return false, nil
	}

	allEvs := make([]*types.Event, 0)
	allEDUs := make([]*types.EDU, 0, 100)

	for _, td := range toDevice {
		switch td.Type {
		case types.BabbleservRemoteDeviceListUpdate:
			allEDUs = append(allEDUs, &types.EDU{
				Type:    types.EDUTypeDeviceListUpdate,
				Content: td.Content,
			})
			log.Debug().
				Str("user_id", gjson.GetBytes(td.Content, "user_id").String()).
				Str("device_id", gjson.GetBytes(td.Content, "device_id").String()).
				Msg("Sending remote device list update")
			continue
		case types.BabbleservRemoteSigningKeyUpdate:
			allEDUs = append(allEDUs, &types.EDU{
				Type:    types.EDUTypeSigningKeyUpdate,
				Content: td.Content,
			})
			log.Debug().
				Str("user_id", gjson.GetBytes(td.Content, "user_id").String()).
				Msg("Sending remote signing key update")
			continue
		case types.BabbleservRemotePresenceChange:
			allEDUs = append(allEDUs, &types.EDU{
				Type:    types.EDUTypePresence,
				Content: td.Content,
			})
			log.Debug().
				Str("user_id", gjson.GetBytes(td.Content, "push[0].user_id").String()).
				Msg("Sending remote presence update")
			continue
		case types.BabbleservRemoteOutlierEvent:
			var ev *types.Event
			exerrors.PanicIfNotNil(json.Unmarshal(td.Content, &ev))
			allEvs = append(allEvs, ev)
			log.Debug().
				Stringer("event_id", ev.ID).
				Stringer("type", ev.Type).
				Stringer("sender", ev.Sender).
				Msg("Sending remote outlier event")
			continue
		}

		var tdContent map[string]any
		if err := json.Unmarshal(td.Content, &tdContent); err != nil {
			panic(err)
		}
		content := types.ToDeviceEDUContent{
			MessageID: types.MustVersionstampToOrderedString(td.Version),
			Type:      td.Type,
			Sender:    td.Sender,
			Messages: types.ToDeviceEDUMessages{
				td.UserID: {
					td.DeviceID: tdContent,
				},
			},
		}
		b, err := json.Marshal(content)
		if err != nil {
			panic(err)
		}

		allEDUs = append(allEDUs, &types.EDU{
			Type:    types.EDUTypeToDevice,
			Content: b,
		})
		log.Debug().
			Str("user_id", td.UserID.String()).
			Str("device_id", td.DeviceID.String()).
			Str("type", td.Type.String()).
			Str("sender", td.Sender.String()).
			Msg("Sending remote to-device event")
	}

	if len(allEDUs) > 0 || len(allEvs) > 0 {
		if err := fs.sendTransactionToServer(ctx, serverName, roomsVersion, allEvs, allEDUs); err != nil {
			return false, fmt.Errorf("failed to send transient transaction: %w", err)
		}
	}

	serverVersions[types.TransientVersionKey] = nextVersion
	return true, nil
}

func (fs *FederationSender) sendTransactionToServer(
	ctx context.Context,
	serverName string,
	version tuple.Versionstamp,
	pdus []*types.Event,
	edus []*types.EDU,
) error {
	log := zerolog.Ctx(ctx)

	transactionID := types.MustVersionstampToOrderedString(version)

	log.Debug().
		Int("pdus", len(pdus)).
		Int("edus", len(edus)).
		Str("transaction_id", transactionID).
		Msg("Sending transaction to server")

	gedus := make([]gomatrixserverlib.EDU, len(edus))
	for i, edu := range edus {
		data := exerrors.Must(json.Marshal(edu.Content))
		gedus[i] = gomatrixserverlib.EDU{
			Type:        string(edu.Type),
			Content:     spec.RawJSON(data),
			Origin:      fs.config.ServerName,
			Destination: serverName,
		}
	}

	if resp, err := fs.fclient.SendTransaction(ctx, gomatrixserverlib.Transaction{
		TransactionID:  gomatrixserverlib.TransactionID(transactionID),
		Origin:         spec.ServerName(fs.config.ServerName),
		Destination:    spec.ServerName(serverName),
		OriginServerTS: spec.Timestamp(time.Now().UnixMilli()),
		PDUs:           util.EventsToJSONs(pdus),
		EDUs:           gedus,
	}); err != nil {
		return err
	} else {
		var success, error int
		for evID, result := range resp.PDUs {
			if result.Error == "" {
				success++
			} else {
				error++
				log.Warn().Err(err).
					Str("event_id", evID).
					Str("transaction_id", transactionID).
					Msg("Event error from other server")
			}
		}
		log.Info().
			Int("pdu_success", success).
			Int("pdu_error", error).
			Int("edus", len(edus)).
			Str("transaction_id", transactionID).
			Msg("Sent transaction to server")
	}

	return nil
}
