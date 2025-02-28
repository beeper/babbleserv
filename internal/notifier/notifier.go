package notifier

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"github.com/rs/xid"
	"github.com/rs/zerolog"
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/util"
)

type Subscription struct {
	// Subscribe by user IDs or room IDs
	UserIDs []id.UserID
	RoomIDs []id.RoomID

	// Subscribe by type
	AllEvents,
	AllServers,
	AllUsers bool
}

type subscription struct {
	Subscription
	// The callback channel to send results to - we key subscriptions by this
	channel chan any
}

// A change represents one or more changes to entities
type Change struct {
	// Random ID of this babbleserv instance to ignore changes send by ourselves
	InstanceID string `msgpack:"i"`

	EventIDs []id.EventID `msgpack:"e,omitempty"`
	RoomIDs  []id.RoomID  `msgpack:"r,omitempty"`
	UserIDs  []id.UserID  `msgpack:"u,omitempty"`
	Servers  []string     `msgpack:"s,omitempty"`
}

// The notifier allows components to subscribe to and receive change notifications
// from each other, both within a single Babbleserv process and across multiple
// as changes as propagated over Redis pubsub.
type Notifier struct {
	log zerolog.Logger

	redis        *redis.Client
	redisChannel string
	instanceID   string

	ctx    context.Context
	cancel context.CancelFunc

	// Subscribe/unsubscribe channels
	subscribeCh   chan subscription
	unsubscribeCh chan chan any
	// Send change channels
	userChangeCh   chan id.UserID
	roomChangeCh   chan id.RoomID
	eventsChangeCh chan id.EventID
	serverChangeCh chan string
	// Map channels to subscriptions
	chanToSubscription map[chan any]subscription
	// Map user/room/event IDs to channels
	userIDToChan map[id.UserID]map[chan any]struct{}
	roomIDToChan map[id.RoomID]map[chan any]struct{}
	// Map channels for all event/server subscribers
	eventChs  map[chan any]struct{}
	serverChs map[chan any]struct{}
	userChs   map[chan any]struct{}
}

func NewNotifier(name string, cfg config.NotifierConfig, logger zerolog.Logger) *Notifier {
	log := logger.With().
		Str("notifier", name).
		Logger()

	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
	})

	// Generate a small and process specific instance ID from XID's machine ID + PID
	uid := xid.New()
	instanceID := util.Base64Encode(uid.Machine()) + strconv.Itoa(int(uid.Pid()))

	return &Notifier{
		log:          log,
		redis:        rdb,
		redisChannel: cfg.RedisChannel,
		instanceID:   instanceID,

		subscribeCh:    make(chan subscription),
		unsubscribeCh:  make(chan chan any),
		userChangeCh:   make(chan id.UserID),
		roomChangeCh:   make(chan id.RoomID),
		eventsChangeCh: make(chan id.EventID),
		serverChangeCh: make(chan string),

		chanToSubscription: make(map[chan any]subscription),
		userIDToChan:       make(map[id.UserID]map[chan any]struct{}),
		roomIDToChan:       make(map[id.RoomID]map[chan any]struct{}),
		eventChs:           make(map[chan any]struct{}),
		serverChs:          make(map[chan any]struct{}),
	}
}

func (n *Notifier) Start() {
	n.log.Info().Msg("Starting notifier...")
	n.ctx, n.cancel = context.WithCancel(n.log.WithContext(context.Background()))
	go n.internalLoop()
	go n.redisLoop()
}

func (n *Notifier) Stop() {
	n.log.Info().Msg("Stopping notifier...")
	n.cancel()
}

// Subscribe for notifier changes, which will be sent to the channel provided,
// delivery is not guaranteed if the channel is blocked as the notifier cannot
// wait for any downstream work.
func (n *Notifier) Subscribe(ch chan any, req Subscription) {
	n.log.Trace().Any("subscription", req).Msg("Subscribe")
	n.subscribeCh <- subscription{req, ch}
}

func (n *Notifier) Unsubscribe(ch chan any) {
	n.unsubscribeCh <- ch
}

func (n *Notifier) SendChange(change Change) {
	n.log.Trace().Any("change", change).Msg("Sending change")
	go n.sendRedisChange(change)
	n.sendInternalChange(change)
}

func (n *Notifier) sendInternalChange(change Change) {
	for _, evID := range change.EventIDs {
		n.eventsChangeCh <- evID
	}
	for _, roomID := range change.RoomIDs {
		n.roomChangeCh <- roomID
	}
	for _, userID := range change.UserIDs {
		n.userChangeCh <- userID
	}
	for _, server := range change.Servers {
		n.serverChangeCh <- server
	}
}

func (n *Notifier) sendRedisChange(change Change) {
	change.InstanceID = n.instanceID
	data, err := msgpack.Marshal(change)
	if err != nil {
		panic(fmt.Errorf("failed to msgpack change: %w", err))
	}
	if err := n.redis.Publish(n.ctx, n.redisChannel, data).Err(); err != nil {
		n.log.Err(err).Msg("Failed to publish Redis message")
	}
}

func (n *Notifier) redisLoop() {
	pubsub := n.redis.Subscribe(n.ctx, n.redisChannel)
	defer pubsub.Close()

	for msg := range pubsub.Channel() {
		var change Change
		if err := msgpack.Unmarshal([]byte(msg.Payload), &change); err != nil {
			n.log.Err(err).Str("payload", msg.Payload).Msg("Invalid msgpack data over pubsub")
			continue
		}
		if change.InstanceID == n.instanceID {
			// Skip changes sent from ourselves
			continue
		}
		n.sendInternalChange(change)
	}
}

func (n *Notifier) internalLoop() {
	for {
		select {
		case <-n.ctx.Done():
			return
		// Handle subscription/unsubscription
		case sub := <-n.subscribeCh:
			n.unlockedSubscribe(sub)
		case ch := <-n.unsubscribeCh:
			n.unlockedUnusbscribe(ch)
		// Handle subscriptions
		case eventID := <-n.eventsChangeCh:
			// All event subscribers
			n.unlockedSendChanges(n.eventChs, eventID)
		case server := <-n.serverChangeCh:
			// All server subscribers
			n.unlockedSendChanges(n.serverChs, server)
		case userID := <-n.userChangeCh:
			// All user subscribers
			n.unlockedSendChanges(n.userChs, userID)
			// Per-user subscribers
			if chs, found := n.userIDToChan[userID]; found {
				n.unlockedSendChanges(chs, userID)
			}
		case roomID := <-n.roomChangeCh:
			// Per-room subscribers
			if chs, found := n.roomIDToChan[roomID]; found {
				n.unlockedSendChanges(chs, roomID)
			}
		}
	}
}

func (n *Notifier) unlockedSendChanges(chs map[chan any]struct{}, item any) {
	for ch := range chs {
		select {
		case ch <- item:
		default:
			n.log.Warn().Any("item", item).Msg("Failed to send notification")
		}
	}
}

func (n *Notifier) unlockedSubscribe(sub subscription) {
	// Unsubscribe using channel reference
	n.chanToSubscription[sub.channel] = sub

	// Add subscribe to all channels
	if sub.AllEvents {
		n.eventChs[sub.channel] = struct{}{}
	}
	if sub.AllServers {
		n.serverChs[sub.channel] = struct{}{}
	}
	if sub.AllUsers {
		n.userChs[sub.channel] = struct{}{}
	}

	// Add specific subscription channels
	for _, userID := range sub.UserIDs {
		if _, found := n.userIDToChan[userID]; !found {
			n.userIDToChan[userID] = make(map[chan any]struct{})
		}
		n.userIDToChan[userID][sub.channel] = struct{}{}
	}
	for _, roomID := range sub.RoomIDs {
		if _, found := n.roomIDToChan[roomID]; !found {
			n.roomIDToChan[roomID] = make(map[chan any]struct{})
		}
		n.roomIDToChan[roomID][sub.channel] = struct{}{}
	}
}

func (n *Notifier) unlockedUnusbscribe(ch chan any) {
	sub, found := n.chanToSubscription[ch]
	if !found {
		n.log.Warn().Msg("Unsubscribe using non-existent channel")
		return
	}

	if sub.AllEvents {
		delete(n.eventChs, ch)
	}
	if sub.AllServers {
		delete(n.serverChs, ch)
	}
	if sub.AllUsers {
		delete(n.userChs, ch)
	}

	for _, userID := range sub.UserIDs {
		delete(n.userIDToChan[userID], ch)
	}
	for _, roomID := range sub.RoomIDs {
		delete(n.roomIDToChan[roomID], ch)
	}
}
