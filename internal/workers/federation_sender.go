package workers

import (
	"context"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/databases/rooms"
	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

type FederationSender struct {
	log      zerolog.Logger
	config   config.BabbleConfig
	db       *databases.Databases
	notifier *notifier.Notifier
	fclient  fclient.FederationClient

	// Internal map + lock of active senders we have running in this process
	lock          sync.RWMutex
	serverSenders map[string]chan struct{}

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

func NewFederationSender(
	logger zerolog.Logger,
	cfg config.BabbleConfig,
	db *databases.Databases,
	notif *notifier.Notifier,
	fclient fclient.FederationClient,
) *FederationSender {
	log := logger.With().
		Str("worker", "FederationSender").
		Logger()

	return &FederationSender{
		log:           log,
		config:        cfg,
		db:            db,
		notifier:      notif,
		fclient:       fclient,
		serverSenders: make(map[string]chan struct{}),
	}
}

func (fs *FederationSender) Start() {
	fs.ctx, fs.cancel = context.WithCancel(fs.log.WithContext(context.Background()))
	fs.log.Info().Msg("Starting federation sender...")
	go fs.handleServersLoop()
}

func (fs *FederationSender) Stop() {
	fs.cancel()
	fs.wg.Wait()
	fs.log.Info().Msg("Federation sender stopped")
}

func (fs *FederationSender) handleServersLoop() {
	fs.wg.Add(1)
	defer fs.wg.Done()

	newServersCh := make(chan any, 1000)
	fs.notifier.Subscribe(newServersCh, notifier.Subscription{AllServers: true})
	defer fs.notifier.Unsubscribe(newServersCh)

	for {
		select {
		case <-fs.ctx.Done():
			return
		case server := <-newServersCh:
			serverName := server.(string)

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
				fs.log.Trace().
					Str("server", serverName).
					Msg("We are already running this server sender")
			} else {
				go fs.maybeRunServerSender(serverName)
			}
		}
	}
}

const (
	serverSenderLockNamePrefix = "FederationServerSenderLock:"
	serverSenderLockRefresh    = time.Second * 30
	serverSenderLockTimeout    = time.Second * 60
)

func (fs *FederationSender) maybeRunServerSender(serverName string) {
	lockName := serverSenderLockNamePrefix + serverName
	var hadLock bool

	if err := lock.WithLockIfAvailable(fs.ctx, fs.db.Rooms, lockName, lock.LockOptions{
		RefreshInterval: serverSenderLockRefresh,
		Timeout:         serverSenderLockTimeout,
	}, func(lock lock.Lock) {
		fs.wg.Add(1)
		defer fs.wg.Done()
		hadLock = true

		log := fs.log.With().
			Str("server", serverName).
			Logger()

		wakeCh := make(chan struct{}, 1)

		// Store internal flag that we're running this sender
		fs.lock.Lock()
		fs.serverSenders[serverName] = wakeCh
		fs.lock.Unlock()

		log.Info().Msg("Starting server sender")
		fs.sendEventsToServerLoop(serverName, lock, log, wakeCh)

		// Remove the internal flag on sender
		fs.lock.Lock()
		delete(fs.serverSenders, serverName)
		fs.lock.Unlock()

		lock.Release()
		log.Info().Msg("Server sender stopped without error")
	}); err != nil {
		fs.log.Err(err).Msg("Error starting server sender")
		return
	} else if !hadLock {
		fs.log.Trace().
			Str("server", serverName).
			Msg("Someone else is already running this server sender")
	}
}

func (fs *FederationSender) sendEventsToServerLoop(
	serverName string,
	lock lock.Lock,
	log zerolog.Logger,
	wakeCh chan struct{},
) {
	var noSends int

	trySend := func() {
		if fs.sendEventsToServer(serverName, lock, log) {
			noSends = 0
		} else {
			noSends++
		}
	}

	trySend()

	for {
		select {
		case <-fs.ctx.Done():
			return
		case <-wakeCh:
			trySend()
		case <-time.After(serverSenderLockRefresh):
			trySend()
		}
		if noSends >= 10 {
			// After 10 refreshes without sends, exit the server sender. If new
			// events come in relevant to this server we'll start again.
			return
		}
	}
}

func (fs *FederationSender) sendEventsToServer(serverName string, lock lock.Lock, log zerolog.Logger) bool {
	serverVersions, err := fs.db.Rooms.GetServerPositions(fs.ctx, serverName)
	if err != nil {
		log.Err(err).Msg("Failed to get current server positions")
		return false
	} else if serverVersions == nil {
		serverVersions = make(types.VersionMap)
	}

	var sent bool

	for {
		lock.Refresh()

		roomsVersion, found := serverVersions[types.RoomsVersionKey]
		if !found {
			roomsVersion = types.ZeroVersionstamp
		}

		nextVersion, events, err := fs.db.Rooms.SyncRoomEventsForServer(fs.ctx, serverName, rooms.SyncOptions{
			Limit: 50, // hardcoded spec limit
			From:  roomsVersion,
		})
		if err != nil {
			log.Err(err).Msg("Failed to sync events for server")
			return sent
		}

		if nextVersion == roomsVersion {
			return sent
		}
		sent = true

		allEvs := make([]*types.Event, 0, 50)
		for _, evs := range events {
			allEvs = append(allEvs, evs...)
		}
		transactionID := util.Base64EncodeURLSafe(roomsVersion.Bytes())

		log.Info().
			Int("pdus", len(allEvs)).
			Str("transaction_id", transactionID).
			Msg("Sending transaction to server")

		if resp, err := fs.fclient.SendTransaction(fs.ctx, gomatrixserverlib.Transaction{
			TransactionID:  gomatrixserverlib.TransactionID(transactionID),
			Origin:         spec.ServerName(fs.config.ServerName),
			Destination:    spec.ServerName(serverName),
			OriginServerTS: spec.Timestamp(time.Now().UnixMilli()),
			PDUs:           util.EventsToJSONs(allEvs),
		}); err != nil {
			log.Err(err).Msg("Failed to send transaction")
			return sent
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
				Int("success", success).
				Int("error", error).
				Str("transaction_id", transactionID).
				Msg("Sent transaction to server")
		}

		serverVersions[types.RoomsVersionKey] = nextVersion

		err = fs.db.Rooms.UpdateServerPositions(fs.ctx, serverName, serverVersions, lock.TxnRefresh)
		if err != nil {
			log.Err(err).Msg("Failed to update current server positions")
			return sent
		}
	}
}
