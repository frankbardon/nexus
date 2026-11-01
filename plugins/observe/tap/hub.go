package tap

import (
	"sync"
)

// frame is one marshaled envelope as it sits in the ring and in client
// queues. Marshaled once, shared by every client.
type frame struct {
	// cursor is the tap-local delivery position: gap-free, monotonic, in the
	// order the wildcard handler saw events. It is the SSE id, NOT the bus
	// seq — see hub.
	cursor uint64
	// seq is the bus seq (0 for journal-excluded types). Carried for journal
	// dedupe; the consumer reads it from the envelope.
	seq  uint64
	typ  string
	data []byte
}

// client is one connected consumer's live queue.
type client struct {
	filter typeFilter
	ch     chan *frame
	// gone is closed when the hub drops the client: queue overflow or hub
	// shutdown. dropReason says which.
	gone       chan struct{}
	dropReason string
}

const (
	dropOverflow = "overflow"
	dropShutdown = "shutdown"
)

// hub fans one wildcard subscription out to every connected client and keeps
// a bounded ring of recent frames for resume and ring backfill.
//
// Frames are delivered in ARRIVAL order, not bus-seq order. The bus runs
// wildcard handlers after typed handlers, so a nested emit (child) reaches the
// wildcard before the event whose handler emitted it (parent), and concurrent
// emitters interleave. The journal writer reorders by seq because it is a
// durable log; a live tap cannot, because holding children until the parent's
// handlers return would buffer every llm.stream.chunk until the llm.request
// that produced it finished. Hence the cursor: a resume position that is exact
// in delivery order, where a seq-based Last-Event-ID would lose any parent
// still in flight when the client dropped.
type hub struct {
	epoch string

	mu      sync.Mutex
	cursor  uint64
	ring    []*frame // circular, len == capacity once full
	head    int      // index of the oldest frame when full
	full    bool
	clients map[*client]struct{}
	closed  bool

	// minSeq is the lowest non-zero seq the hub has seen; evictedMaxSeq the
	// highest seq pushed out of the ring. Together they bound what the hub
	// can no longer serve: anything below minSeq predates the subscription
	// (beyond the bus's replay ring) and anything at or below evictedMaxSeq
	// fell off this ring. Journal backfill uses them to report a gap honestly.
	minSeq        uint64
	evictedMaxSeq uint64
}

func newHub(epoch string, ringSize int) *hub {
	return &hub{
		epoch:   epoch,
		ring:    make([]*frame, 0, ringSize),
		clients: make(map[*client]struct{}),
	}
}

// publish assigns the next cursor, appends to the ring, and enqueues to every
// matching client without blocking. A client whose queue is full is dropped
// rather than waited on: dispatch must never stall on a consumer, and a
// dropped client resumes exactly from its last cursor while the ring still
// holds it.
func (h *hub) publish(f *frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.cursor++
	f.cursor = h.cursor
	if f.seq != 0 && (h.minSeq == 0 || f.seq < h.minSeq) {
		h.minSeq = f.seq
	}

	if cap(h.ring) > 0 {
		if !h.full {
			h.ring = append(h.ring, f)
			h.full = len(h.ring) == cap(h.ring)
		} else {
			old := h.ring[h.head]
			if old.seq > h.evictedMaxSeq {
				h.evictedMaxSeq = old.seq
			}
			h.ring[h.head] = f
			h.head = (h.head + 1) % len(h.ring)
		}
	}

	for c := range h.clients {
		if !c.filter.match(f.typ) {
			continue
		}
		select {
		case c.ch <- f:
		default:
			h.dropLocked(c, dropOverflow)
		}
	}
}

// snapshotLocked returns the ring oldest-first.
func (h *hub) snapshotLocked() []*frame {
	if !h.full {
		out := make([]*frame, len(h.ring))
		copy(out, h.ring)
		return out
	}
	out := make([]*frame, 0, len(h.ring))
	out = append(out, h.ring[h.head:]...)
	return append(out, h.ring[:h.head]...)
}

// registration is what a new client gets atomically with its queue: the ring
// as of registration and the hub's coverage bounds. Every frame published
// after registration lands in the queue; every frame before it is in the
// snapshot (or was evicted). Nothing is seen twice or skipped at the seam.
type registration struct {
	client        *client
	snapshot      []*frame
	minSeq        uint64
	evictedMaxSeq uint64
	// oldestCursor is the cursor of the oldest ring frame, 0 if empty;
	// headCursor the last cursor assigned.
	oldestCursor uint64
	headCursor   uint64
}

func (h *hub) register(filter typeFilter, buffer int) (registration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return registration{}, false
	}
	c := &client{
		filter: filter,
		ch:     make(chan *frame, buffer),
		gone:   make(chan struct{}),
	}
	h.clients[c] = struct{}{}
	snap := h.snapshotLocked()
	reg := registration{
		client:        c,
		snapshot:      snap,
		minSeq:        h.minSeq,
		evictedMaxSeq: h.evictedMaxSeq,
		headCursor:    h.cursor,
	}
	if len(snap) > 0 {
		reg.oldestCursor = snap[0].cursor
	}
	return reg, true
}

// canResume reports whether every frame after cursor is still available:
// either in the snapshot or, when cursor is the head, nothing was missed.
func (r registration) canResume(cursor uint64) bool {
	if cursor > r.headCursor {
		return false
	}
	if cursor == r.headCursor {
		return true
	}
	return r.oldestCursor != 0 && cursor+1 >= r.oldestCursor
}

func (h *hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.gone)
	}
}

func (h *hub) dropLocked(c *client, reason string) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	c.dropReason = reason
	close(c.gone)
}

// close drops every client and refuses further publishes and registrations.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		h.dropLocked(c, dropShutdown)
	}
}

func (h *hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
