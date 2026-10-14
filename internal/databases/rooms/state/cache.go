package state

import (
	"container/list"
	"sync"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const defaultCacheBytes = 64 << 20

// Rough per item bookkeeping cost on top of the value: map slot, list element, key and item.
const cacheItemOverhead = 256

// A decoded leaf holds a 48 byte entry per item plus separate key and value copies.
const decodedEntryCost = 64

type cacheKey struct {
	roomID id.RoomID
	hash   types.StateHash
}

type cacheItem struct {
	key   cacheKey
	value any
	cost  int
}

// cache is an LRU of decoded pages and contexts that are committed, bounded by bytes. Both are
// immutable. Each room stores its own records, so entries are keyed by room: a hash read in one room
// says nothing about another room's storage.
type cache struct {
	mu       sync.Mutex
	maxBytes int
	bytes    int
	items    map[cacheKey]*list.Element
	order    *list.List
}

func newCache(maxBytes int) *cache {
	return &cache{
		maxBytes: maxBytes,
		items:    make(map[cacheKey]*list.Element),
		order:    list.New(),
	}
}

func (c *cache) get(key cacheKey) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheItem).value
}

func (c *cache) getPage(roomID id.RoomID, h types.StateHash) *page {
	p, _ := c.get(cacheKey{roomID, h}).(*page)
	return p
}

func (c *cache) getContext(roomID id.RoomID, h types.StateHash) (stateContext, bool) {
	ctx, ok := c.get(cacheKey{roomID, h}).(stateContext)
	return ctx, ok
}

func (c *cache) add(key cacheKey, value any, size int) {
	cost := size + cacheItemOverhead
	if cost > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
	} else {
		c.items[key] = c.order.PushFront(&cacheItem{key: key, value: value, cost: cost})
		c.bytes += cost
	}
	for c.bytes > c.maxBytes {
		evicted := c.order.Remove(c.order.Back()).(*cacheItem)
		delete(c.items, evicted.key)
		c.bytes -= evicted.cost
	}
}
