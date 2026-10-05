package events

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

var (
	// ErrAuthEventNotFinalized fails finalizing an event whose auth chain reaches a header neither
	// finalized nor finalizable, such as a rejected or unknown event's, and reading the header of an
	// event neither finalized nor pending.
	ErrAuthEventNotFinalized = errors.New("auth event header not finalized")
	// ErrAuthEventPending fails finalizing an event whose auth chain reaches, through stored events
	// not finalized, a pending event later in the batch, which may be finalized once that one is.
	ErrAuthEventPending = errors.New("auth event pending in the batch")

	errNoAuthHeader = errors.New("no auth header")
)

// maxAuthEdges is the most auth events a finalized event may cite. The auth events selection that
// step 4 enforces allows at most seven, so only a rejected event can cite more.
const maxAuthEdges = 64

// Rows per batch of an artifact job's reads, so a batch finishes well within FoundationDB's five
// seconds and loses little to a retry: auth headers of about 400 bytes, chains whose links are read,
// links of about 60 bytes and chain positions of about 100 bytes, about 1 MB a batch at most.
const (
	headerBatch    = 1000
	linkChainBatch = 1000
	linkRowBatch   = 10000
	positionBatch  = 5000
)

func packAuthHeader(h types.AuthHeader) []byte {
	parents := make(tuple.Tuple, len(h.AuthEventIDs))
	for i, p := range h.AuthEventIDs {
		parents[i] = p.String()
	}
	return tuple.Tuple{int64(1), int64(h.Chain), int64(h.Sequence), parents}.Pack()
}

func unpackAuthHeader(raw []byte) (types.AuthHeader, error) {
	t, err := tuple.Unpack(raw)
	if err != nil {
		return types.AuthHeader{}, err
	}
	var h types.AuthHeader
	if len(t) != 4 {
		return h, fmt.Errorf("invalid auth header length")
	} else if version, ok := t[0].(int64); !ok || version != 1 {
		return h, fmt.Errorf("invalid auth header version")
	}
	chain, ok1 := t[1].(int64)
	seq, ok2 := t[2].(int64)
	parents, ok3 := t[3].(tuple.Tuple)
	if !ok1 || chain < 0 || chain > math.MaxUint32 || !ok2 || seq < 0 || seq > math.MaxUint32 || !ok3 || (chain == 0) != (seq == 0) {
		return h, fmt.Errorf("invalid auth header fields")
	}
	h.Chain, h.Sequence = uint32(chain), uint32(seq)
	for _, v := range parents {
		p, ok := v.(string)
		if !ok || p == "" {
			return h, fmt.Errorf("invalid auth parent")
		}
		h.AuthEventIDs = append(h.AuthEventIDs, id.EventID(p))
	}
	// Events rejected at step 4 for citing too many auth events keep an unfinalized header.
	if h.Finalized() && len(h.AuthEventIDs) > maxAuthEdges {
		return h, fmt.Errorf("too many auth events for a finalized header")
	}
	return h, nil
}

// packChainEntry is the value of an event's chain position
func packChainEntry(eventID id.EventID) []byte {
	return tuple.Tuple{eventID.String()}.Pack()
}

func unpackChainEntry(raw []byte) (id.EventID, error) {
	t, err := tuple.Unpack(raw)
	if err != nil || len(t) != 1 {
		return "", fmt.Errorf("invalid auth chain entry")
	}
	eventID, ok := t[0].(string)
	if !ok || eventID == "" {
		return "", fmt.Errorf("invalid auth chain entry")
	}
	return id.EventID(eventID), nil
}

type eventReader interface {
	WillGet(...id.EventID)
	Get(id.EventID) (*types.Event, error)
}

// AuthGraph holds the auth headers one transaction read and finalized, pending events' included: a
// send's preparation, whose publish writes them under its guard, or a staging transaction. An
// artifact job's graph reads finalized headers across transactions instead, see Renew.
type AuthGraph struct {
	cover    *chainCover
	do       func(func() error) error
	prefetch func([]id.EventID)
	load     func([]id.EventID) ([]types.AuthHeader, error)
	headers  map[id.EventID]types.AuthHeader
	dirty    map[id.EventID]types.AuthHeader
	pending  map[id.EventID]*types.Event
	// The position of each pending event in the batch, the order they are finalized in
	order  map[id.EventID]int
	dir    *EventsDirectory
	txn    fdb.ReadTransaction
	roomID id.RoomID
	// Only a graph finalizing headers within one transaction has an event reader.
	events eventReader
}

// authHeaderReader batches reads of stored auth headers. Every stored event has one, so only
// the pending events, which are not stored yet, may lack one. A failed load returns the headers
// read before the failure.
func (e *EventsDirectory) authHeaderReader(
	txn fdb.ReadTransaction,
	pending map[id.EventID]*types.Event,
) (prefetch func([]id.EventID), load func([]id.EventID) ([]types.AuthHeader, error)) {
	futures := make(map[id.EventID]fdb.FutureByteSlice)
	prefetch = func(ids []id.EventID) {
		for _, eventID := range ids {
			if _, ok := futures[eventID]; !ok {
				futures[eventID] = txn.Get(e.keyForAuthEventIDs(eventID))
			}
		}
	}
	load = func(ids []id.EventID) ([]types.AuthHeader, error) {
		prefetch(ids)
		out := make([]types.AuthHeader, 0, len(ids))
		for _, eventID := range ids {
			raw, err := futures[eventID].Get()
			delete(futures, eventID)
			if err != nil {
				return out, err
			}
			var h types.AuthHeader
			if raw != nil {
				if h, err = unpackAuthHeader(raw); err != nil {
					return out, fmt.Errorf("auth header %s: %w", eventID, err)
				}
			} else if ev := pending[eventID]; ev != nil {
				h.AuthEventIDs = slices.Clone(ev.AuthEventIDs)
			} else {
				return out, fmt.Errorf("%w for %s", errNoAuthHeader, eventID)
			}
			out = append(out, h)
		}
		return out, nil
	}
	return prefetch, load
}

// NewAuthGraph returns a graph reading in one transaction, which finalizes the headers it needs.
func (e *EventsDirectory) NewAuthGraph(txn fdb.ReadTransaction, roomID id.RoomID, provider *TxnEventsProvider) *AuthGraph {
	g := e.newAuthGraph(roomID, func(reads func() error) error { return reads() })
	g.txn, g.events = txn, provider
	g.prefetch, g.load = e.authHeaderReader(txn, g.pending)
	return g
}

// NewJobAuthGraph returns a graph for an artifact job, reading in the transactions Renew gives it.
func (e *EventsDirectory) NewJobAuthGraph(roomID id.RoomID, do func(func() error) error) *AuthGraph {
	return e.newAuthGraph(roomID, do)
}

func (e *EventsDirectory) newAuthGraph(roomID id.RoomID, do func(func() error) error) *AuthGraph {
	g := &AuthGraph{headers: make(map[id.EventID]types.AuthHeader), dirty: make(map[id.EventID]types.AuthHeader), pending: make(map[id.EventID]*types.Event), order: make(map[id.EventID]int), dir: e, roomID: roomID}
	g.do = do
	g.cover = g.newChainCover()
	return g
}

// Renew moves the graph's reads to another transaction, for an artifact job whose reads span
// transactions. What the graph holds stays: headers, which never change once finalized, pending
// events with their chain positions, and the chains read so far. From then on it finalizes nothing.
func (g *AuthGraph) Renew(txn fdb.ReadTransaction, do func(func() error) error) {
	g.txn, g.do, g.events = txn, do, nil
	g.prefetch, g.load = g.dir.authHeaderReader(txn, g.pending)
}

// Add makes pending events available to the graph, in batch order, and starts reading any header
// already stored for their state events, which alone can be finalized.
func (g *AuthGraph) Add(evs ...*types.Event) {
	var stateIDs []id.EventID
	for _, ev := range evs {
		g.pending[ev.ID] = ev
		if _, added := g.order[ev.ID]; !added {
			g.order[ev.ID] = len(g.order)
		}
		if _, loaded := g.headers[ev.ID]; !loaded && ev.StateKey != nil {
			stateIDs = append(stateIDs, ev.ID)
		}
	}
	if g.prefetch != nil {
		g.prefetch(stateIDs)
	}
}

// read keeps the headers loaded before a failure, so reading again loads only the rest.
func (g *AuthGraph) read(ids []id.EventID) error {
	var missing []id.EventID
	seen := make(map[id.EventID]bool, len(ids))
	for _, eventID := range ids {
		if _, ok := g.headers[eventID]; !ok && !seen[eventID] {
			seen[eventID] = true
			missing = append(missing, eventID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	hs, err := g.load(missing)
	for i, h := range hs {
		g.headers[missing[i]] = h
	}
	return err
}

func (g *AuthGraph) event(eventID id.EventID) (*types.Event, error) {
	if ev := g.pending[eventID]; ev != nil {
		return ev, nil
	}
	return g.events.Get(eventID)
}

func (g *AuthGraph) willGet(ids []id.EventID) {
	for _, eventID := range ids {
		if _, ok := g.pending[eventID]; !ok {
			g.events.WillGet(eventID)
		}
	}
}

// Headers returns the events' headers, finalizing in batch order those of pending events that are
// not, labelling them on the chain cover. An event is finalized after what it cites, which must be
// finalized, pending earlier in the batch, or a stored event finalizeCited finalizes ahead of it;
// otherwise Headers fails with ErrAuthEventPending for a pending event and ErrAuthEventNotFinalized
// for anything else.
func (g *AuthGraph) Headers(ids []id.EventID) ([]types.AuthHeader, error) {
	if err := g.read(ids); err != nil {
		return nil, err
	}
	var unfinalized []id.EventID
	seen := make(map[id.EventID]bool, len(ids))
	for _, eventID := range ids {
		if g.headers[eventID].Finalized() || seen[eventID] {
			continue
		} else if g.events == nil {
			return nil, fmt.Errorf("%w: %s, which a graph spanning transactions does not finalize", ErrAuthEventNotFinalized, eventID)
		} else if _, pending := g.pending[eventID]; !pending {
			return nil, fmt.Errorf("%w: %s", ErrAuthEventNotFinalized, eventID)
		}
		seen[eventID] = true
		unfinalized = append(unfinalized, eventID)
	}
	if err := g.finalize(unfinalized); err != nil {
		return nil, err
	}
	out := make([]types.AuthHeader, len(ids))
	for i, eventID := range ids {
		out[i] = g.headers[eventID]
	}
	return out, nil
}

// HeadersInBatches is Headers reading at most headerBatch events per batch of reads.
func (g *AuthGraph) HeadersInBatches(ids []id.EventID) ([]types.AuthHeader, error) {
	for part := range slices.Chunk(ids, headerBatch) {
		if err := g.do(func() error {
			_, err := g.Headers(part)
			return err
		}); err != nil {
			return nil, err
		}
	}
	out := make([]types.AuthHeader, len(ids))
	for i, eventID := range ids {
		out[i] = g.headers[eventID]
	}
	return out, nil
}

// finalize finalizes pending events in batch order, reading the headers and events of the auth
// events they cite together first.
func (g *AuthGraph) finalize(ids []id.EventID) error {
	if len(ids) == 0 {
		return nil
	}
	slices.SortFunc(ids, func(a, b id.EventID) int { return cmp.Compare(g.order[a], g.order[b]) })
	var parents []id.EventID
	for _, eventID := range ids {
		authIDs := g.headers[eventID].AuthEventIDs
		if len(authIDs) > maxAuthEdges {
			return fmt.Errorf("too many auth edges for %s", eventID)
		}
		parents = append(parents, authIDs...)
	}
	g.willGet(parents)
	if err := g.read(parents); err != nil {
		return err
	}
	for _, eventID := range ids {
		if err := g.finalizeEvent(eventID, make(map[id.EventID]bool)); err != nil {
			return err
		}
	}
	return nil
}

// finalizeCited finalizes, ahead of the event citing it, a stored event the graph does not hold as
// pending whose header is not finalized but could be, as an outlier's: a state event of the room
// that is not rejected, citing only stored or pending events. Its auth events are read first. A
// pending event is finalized in batch order instead, and an event on a cycle of citations, among
// those finalizing, never.
func (g *AuthGraph) finalizeCited(eventID id.EventID, finalizing map[id.EventID]bool) error {
	if _, pending := g.pending[eventID]; pending || g.headers[eventID].Finalized() || finalizing[eventID] {
		return nil
	}
	ev, err := g.events.Get(eventID)
	if err != nil {
		return err
	}
	authIDs := g.headers[eventID].AuthEventIDs
	if ev == nil || ev.Rejected || ev.StateKey == nil || ev.RoomID != g.roomID || len(authIDs) > maxAuthEdges {
		return nil
	}
	g.willGet(authIDs)
	if err := g.read(authIDs); errors.Is(err, errNoAuthHeader) {
		return nil
	} else if err != nil {
		return err
	}
	return g.finalizeEvent(eventID, finalizing)
}

func (g *AuthGraph) finalizeEvent(eventID id.EventID, finalizing map[id.EventID]bool) error {
	finalizing[eventID] = true
	for _, p := range g.headers[eventID].AuthEventIDs {
		if err := g.finalizeCited(p, finalizing); err != nil {
			return err
		}
	}
	h := g.headers[eventID]
	for _, p := range h.AuthEventIDs {
		if pending := g.pending[p]; !g.headers[p].Finalized() && pending != nil && !pending.Rejected && !finalizing[p] {
			return fmt.Errorf("%w: %s cites %s", ErrAuthEventPending, eventID, p)
		} else if !g.headers[p].Finalized() {
			return fmt.Errorf("%w: %s cites %s", ErrAuthEventNotFinalized, eventID, p)
		}
	}

	ev, err := g.event(eventID)
	if err != nil {
		return err
	}
	if ev == nil || ev.StateKey == nil || ev.RoomID != g.roomID {
		return fmt.Errorf("invalid chain event %s", eventID)
	}
	parents := make([]*types.Event, len(h.AuthEventIDs))
	headers := make([]types.AuthHeader, len(parents))
	for i, p := range h.AuthEventIDs {
		if parents[i], err = g.event(p); err != nil {
			return err
		}
		if parents[i] == nil || parents[i].StateKey == nil || parents[i].RoomID != g.roomID {
			return fmt.Errorf("invalid chain parent %s", p)
		}
		headers[i] = g.headers[p]
	}
	pos, err := g.assign(ev, parents, headers)
	if err != nil {
		return err
	}
	h.Chain, h.Sequence = pos.chain, pos.seq
	g.headers[eventID], g.dirty[eventID] = h, h
	return nil
}

func (g *AuthGraph) Write(txn fdb.Transaction) {
	g.writeChains(txn)
	for eventID, h := range g.dirty {
		txn.Set(g.dir.keyForAuthEventIDs(eventID), packAuthHeader(h))
	}
}
