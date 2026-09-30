package notifier

import (
	"context"
	"fmt"
	"strconv"
	"sync"

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
	channel chan Change
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

func (c Change) IsEmpty() bool {
	return len(c.EventIDs) == 0 &&
		len(c.RoomIDs) == 0 &&
		len(c.UserIDs) == 0 &&
		len(c.Servers) == 0
}

func (c Change) MarshalZerologObject(ev *zerolog.Event) {
	for _, eventID := range c.EventIDs {
		ev.Str("event_id", eventID.String())
	}
	for _, roomID := range c.RoomIDs {
		ev.Str("room_id", roomID.String())
	}
	for _, userID := range c.UserIDs {
		ev.Str("user_id", userID.String())
	}
	for _, serverName := range c.Servers {
		ev.Str("server", serverName)
	}
}

// The notifier allows components to subscribe to and receive change notifications from each other,
// both within a single Babbleserv process and across multiple as changes as propagated over Redis
// pubsub. Pubsub means some updates might get missed, but it shouldn't have major impact.
type Notifier struct {
	log zerolog.Logger

	redis        *redis.Client
	redisPubSub  *redis.PubSub
	redisChannel string
	instanceID   string

	wg        sync.WaitGroup
	cancelCtx context.CancelFunc

	// Subscribe/unsubscribe channels
	subscribeCh   chan subscription
	unsubscribeCh chan chan Change
	// Send change channels
	userChangeCh   chan Change
	roomChangeCh   chan Change
	eventsChangeCh chan Change
	serverChangeCh chan Change
	// Map channels to subscriptions
	chanToSubscription map[chan Change]subscription
	// Map user/room/event IDs to channels
	userIDToChan map[id.UserID]map[chan Change]struct{}
	roomIDToChan map[id.RoomID]map[chan Change]struct{}
	// Map channels for all event/server subscribers
	eventChs  map[chan Change]struct{}
	serverChs map[chan Change]struct{}
	userChs   map[chan Change]struct{}
}

func NewNotifier(name string, cfg config.NotifierConfig, logger zerolog.Logger) *Notifier {
	log := logger.With().
		Str("notifier", name).
		Logger()

	var rdb *redis.Client
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr: cfg.RedisAddr,
		})
	}

	// Generate a small and process specific instance ID from XID's machine ID + PID
	uid := xid.New()
	instanceID := util.Base64Encode(uid.Machine()) + strconv.Itoa(int(uid.Pid()))

	return &Notifier{
		log:          log,
		redis:        rdb,
		redisChannel: cfg.RedisChannel,
		instanceID:   instanceID,

		subscribeCh:    make(chan subscription),
		unsubscribeCh:  make(chan chan Change),
		userChangeCh:   make(chan Change),
		roomChangeCh:   make(chan Change),
		eventsChangeCh: make(chan Change),
		serverChangeCh: make(chan Change),

		chanToSubscription: make(map[chan Change]subscription),
		userIDToChan:       make(map[id.UserID]map[chan Change]struct{}),
		roomIDToChan:       make(map[id.RoomID]map[chan Change]struct{}),
		eventChs:           make(map[chan Change]struct{}),
		serverChs:          make(map[chan Change]struct{}),
		userChs:            make(map[chan Change]struct{}),
	}
}

func (n *Notifier) Start() {
	n.log.Info().Msg("Starting notifier...")

	ctx, cancel := context.WithCancel(n.log.WithContext(context.Background()))
	n.cancelCtx = cancel

	n.wg.Add(1)
	go func() {
		n.internalLoop(ctx)
		n.wg.Done()
	}()

	if n.redis != nil {
		n.wg.Add(1)
		go func() {
			n.redisLoop(ctx)
			n.wg.Done()
		}()
	}
}

func (n *Notifier) Stop() {
	n.log.Info().Msg("Stopping notifier...")
	n.cancelCtx()
	if n.redisPubSub != nil {
		n.redisPubSub.Close()
	}
	n.wg.Wait()
}

// Subscribe for notifier changes, which will be sent to the channel provided,
// delivery is not guaranteed if the channel is blocked as the notifier cannot
// wait for any downstream work.
func (n *Notifier) subscribe(ch chan Change, req Subscription) {
	n.log.Trace().Any("subscription", req).Msg("Subscribe")
	n.subscribeCh <- subscription{req, ch}
}

func (n *Notifier) unsubscribe(ch chan Change) {
	n.unsubscribeCh <- ch
}

func (n *Notifier) SendChange(change Change) {
	if change.IsEmpty() {
		return
	}
	n.log.Trace().Any("change", change).Msg("Sending change")
	n.sendInternalChange(change)
	// Fire of the Redis change asynchronously, as pubsub is best-effort + unordered
	if n.redis != nil {
		go n.sendRedisChange(n.log.WithContext(context.Background()), change)
	}
}

func (n *Notifier) sendInternalChange(change Change) {
	if len(change.EventIDs) > 0 {
		n.eventsChangeCh <- change
	}
	if len(change.RoomIDs) > 0 {
		n.roomChangeCh <- change
	}
	if len(change.UserIDs) > 0 {
		n.userChangeCh <- change
	}
	if len(change.Servers) > 0 {
		n.serverChangeCh <- change
	}
}

func (n *Notifier) sendRedisChange(ctx context.Context, change Change) {
	change.InstanceID = n.instanceID
	data, err := msgpack.Marshal(change)
	if err != nil {
		panic(fmt.Errorf("failed to msgpack change: %w", err))
	}
	if err := n.redis.Publish(ctx, n.redisChannel, data).Err(); err != nil {
		n.log.Err(err).Msg("Failed to publish Redis message")
	}
}

func (n *Notifier) redisLoop(ctx context.Context) {
	n.redisPubSub = n.redis.Subscribe(ctx, n.redisChannel)
	defer n.redisPubSub.Close()

	for msg := range n.redisPubSub.Channel() {
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

func (n *Notifier) internalLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		// Handle subscription/unsubscription
		case sub := <-n.subscribeCh:
			n.unlockedSubscribe(sub)
		case ch := <-n.unsubscribeCh:
			n.unlockedUnusbscribe(ch)
		// Handle subscriptions
		case change := <-n.eventsChangeCh:
			// All event subscribers
			n.unlockedSendChanges(n.eventChs, change)
		case change := <-n.serverChangeCh:
			// All server subscribers
			n.unlockedSendChanges(n.serverChs, change)
		case change := <-n.userChangeCh:
			// All user subscribers
			n.unlockedSendChanges(n.userChs, change)
			// Per-user subscribers
			for _, userID := range change.UserIDs {
				if chs, found := n.userIDToChan[userID]; found {
					n.unlockedSendChanges(chs, change)
				}
			}
		case change := <-n.roomChangeCh:
			// Per-room subscribers
			for _, roomID := range change.RoomIDs {
				if chs, found := n.roomIDToChan[roomID]; found {
					n.unlockedSendChanges(chs, change)
				}
			}
		}
	}
}

func (n *Notifier) unlockedSendChanges(chs map[chan Change]struct{}, item Change) {
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
			n.userIDToChan[userID] = make(map[chan Change]struct{})
		}
		n.userIDToChan[userID][sub.channel] = struct{}{}
	}
	for _, roomID := range sub.RoomIDs {
		if _, found := n.roomIDToChan[roomID]; !found {
			n.roomIDToChan[roomID] = make(map[chan Change]struct{})
		}
		n.roomIDToChan[roomID][sub.channel] = struct{}{}
	}
}

func (n *Notifier) unlockedUnusbscribe(ch chan Change) {
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
