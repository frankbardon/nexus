package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
)

// Write-through push for the session's content-addressed blob store.
//
// # What this buys, stated honestly
//
// Not bandwidth. objectStoreImmutable already recognises blobs/<xx>/<sha>.bin
// and .meta by *identity* and skips them on every snapshot after the first, so
// the repeated per-turn upload this could have removed was already gone before
// this file existed. What is left is the window between a blob landing on disk
// and the next agent.turn.end:
//
//   - Durability. A turn that fetches a PDF, renders a screenshot and embeds an
//     image holds all of those bytes on local disk until the turn ends. A
//     process killed mid-turn loses them, and what survives is a conversation
//     history full of nexus-blob: URIs that resolve to nothing after a resume
//     on a fresh host. Write-through shrinks that window from "one turn" to
//     "one queue drain".
//   - Latency shape. Blobs are the largest single objects in a session tree by
//     a wide margin. Moving them off the turn-boundary critical path spreads
//     the upload across the turn instead of spiking at the end of it.
//
// That is a narrow win and it is the whole win. It is also why this path is
// deliberately best-effort: every failure mode here — a full queue, a Put
// error, a drain that timed out at shutdown — costs the delay it was trying to
// remove and nothing else, because the turn-boundary snapshot still walks the
// whole tree and still re-uploads any immutable file the store does not
// already hold at the right size (listImmutableRemote). Correctness lives
// there; this is an optimisation in front of it.
//
// # Why this is safe to do without a barrier
//
// Blobs are the one subtree where "push it the instant it lands" needs no
// coordination at all. The key is derived from the sha256 of the content, so
// the same key can only ever carry the same bytes: a write-through Put racing
// a snapshot Put of the same blob is two identical uploads, not a conflict.
// There is no read-modify-write, no window in which a partial object is
// visible under a key another writer will overwrite with different content,
// and nothing for a generation stamp to arbitrate. Contrast context/
// conversation.jsonl, which is rewritten and appended to constantly and is the
// reason the general push waits for a boundary.
//
// # Why it is not on the bus
//
// pkg/engine/blobs has no bus dependency and keeps none: BlobStore passes it a
// plain func (blobs.PutHook). An event per blob was the rejected alternative.
// It would have put an emission on the hottest tool paths in a session — every
// read_image, every fetch_page_image, every MCP binary payload — to carry a
// fact the object key already encodes, and it would have made every existing
// session.file.* subscriber (nexus.io.tui, nexus.tool.fileio) react to blob
// writes they have no use for. E2-S3 recorded the no-bus-dependency property
// as the reason blob push stayed out of scope; this preserves it rather than
// spending it.
//
// # Why a worker rather than an inline upload
//
// The hook runs on the goroutine that called Put, which is a tool goroutine
// mid-tool-call. Uploading inline would block that tool for the length of a
// network round trip and would move the cost rather than overlap it, which
// buys nothing over waiting for the turn boundary. One worker goroutine with a
// bounded queue overlaps the upload with the rest of the turn, bounds
// concurrency against the backend to one in-flight request, and preserves
// order. It exists only when a backend is configured.

const (
	// blobWriteThroughQueue bounds the backlog. Full means blobs are being
	// produced faster than they upload, and the right response is to drop the
	// push rather than to block a tool call or to grow memory: the snapshot
	// covers the dropped blob at the turn boundary, which is exactly where it
	// would have been covered without this file. Sized for a burst of tool
	// calls within one turn rather than for a whole session.
	blobWriteThroughQueue = 256

	// blobWriteThroughPutTimeout bounds one blob's upload. Generous because a
	// blob can be tens of megabytes and the alternative to a slow upload here
	// is the same slow upload at the turn boundary; short enough that a wedged
	// backend cannot pin the worker for the life of the session.
	blobWriteThroughPutTimeout = 2 * time.Minute

	// blobWriteThroughDrainTimeout bounds the wait for the queue to empty at
	// shutdown. Anything still queued when this expires is left to the
	// shutdown snapshot, which runs immediately afterwards.
	blobWriteThroughDrainTimeout = 30 * time.Second
)

// blobWriteThrough is the queue and the worker. One per run, created only when
// a backend is configured.
type blobWriteThrough struct {
	queue chan blobPushItem
	// stop is closed to ask the worker to drain and exit. The queue itself is
	// never closed: the sender is an arbitrary tool goroutine that cannot be
	// synchronised with shutdown, and a send on a closed channel is a panic in
	// the middle of a tool call. Signalling on a second channel makes a late
	// send a no-op that lands in the buffer and is never read, which is the
	// same outcome as a drop.
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// Counters for the shutdown summary. Pushed and failed are worker-side,
	// dropped is producer-side, so all three are atomic.
	pushed  atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64
}

type blobPushItem struct {
	binPath  string
	metaPath string
}

// installBlobWriteThrough starts the worker and wires the session's blob hook
// to it. A no-op with no backend configured or no session — no goroutine, no
// queue, and SessionWorkspace.blobPush stays nil, so a blob Put costs one
// atomic load more than it did before this existed.
//
// Called from Boot as soon as a session exists, which is before plugin Init
// and therefore before any plugin opens a blob store. It emits nothing on the
// bus, deliberately: an event dispatched before startJournal subscribes the
// journal's wildcard still consumes a dispatch sequence number, and the
// journal writer only flushes envelopes in contiguous order — one early
// emission stalls the drain and the journal comes out empty. Installing here
// rather than beside the snapshot handlers is what lets a blob written during
// plugin Ready still be pushed.
func (e *Engine) installBlobWriteThrough() {
	if e.objectStore == nil || e.Session == nil {
		return
	}
	w := &blobWriteThrough{
		queue: make(chan blobPushItem, blobWriteThroughQueue),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	e.blobPushes = w
	// The backend handle and the session are captured once, not re-read from
	// the Engine on every push. Stop nils e.objectStore after waiting for the
	// worker to exit, but that wait is bounded — a drain that times out would
	// otherwise leave the worker reading a field the shutdown goroutine is
	// writing, which is a data race whose only symptom is a rare crash under
	// -race. Neither value changes for the life of a run.
	go e.runBlobWriteThrough(w, e.objectStore, e.Session)

	e.Session.setBlobPush(func(binPath, metaPath string) {
		select {
		case w.queue <- blobPushItem{binPath: binPath, metaPath: metaPath}:
		default:
			// Never block the tool goroutine that produced the blob. A dropped
			// push is a blob that uploads at the turn boundary instead of now,
			// which is the behaviour this whole file is an improvement on, not
			// a loss of data.
			w.dropped.Add(1)
		}
	})
}

// runBlobWriteThrough is the worker loop.
func (e *Engine) runBlobWriteThrough(w *blobWriteThrough, store *sessionObjectStore, session *SessionWorkspace) {
	defer close(w.done)

	// Counted rather than flushed per blob: Flush is a whole-backend barrier,
	// and a backend that batches would have every batch reduced to one object
	// if this called it after every Put. Flushing once the queue has gone
	// quiet gives a burst of blobs one barrier instead of N while still
	// making them durable within the turn, which is the point.
	unflushed := 0
	for {
		select {
		case item := <-w.queue:
			if e.pushBlobNow(w, store, session, item) {
				unflushed++
			}
			if unflushed > 0 && len(w.queue) == 0 {
				e.flushBlobPushes(store)
				unflushed = 0
			}
		case <-w.stop:
			for {
				select {
				case item := <-w.queue:
					if e.pushBlobNow(w, store, session, item) {
						unflushed++
					}
				default:
					if unflushed > 0 {
						e.flushBlobPushes(store)
					}
					return
				}
			}
		}
	}
}

// pushBlobNow uploads one blob's two files. Reports whether anything was
// stored, so the caller knows whether a flush is owed.
//
// Errors are logged and swallowed: this is the best-effort half of the seam,
// and the failure policy that decides whether an unpersisted turn is fatal
// belongs to the snapshot, which will retry these objects at the boundary.
func (e *Engine) pushBlobNow(w *blobWriteThrough, store *sessionObjectStore, session *SessionWorkspace, item blobPushItem) bool {
	if store == nil || session == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), blobWriteThroughPutTimeout)
	defer cancel()

	prefix := sessionObjectKeyPrefix(session.ID)
	stored := false
	for _, path := range []string{item.binPath, item.metaPath} {
		// announceRel is reused rather than reimplemented: it resolves a path
		// to the session-relative slash form, refuses anything that escapes
		// the tree, and refuses anything objectStoreExcluded rejects. Reusing
		// it means there is exactly one definition of "what may cross the
		// seam" whether the bytes travel by event or by hook. It emits
		// nothing itself.
		rel, ok := session.announceRel(path)
		if !ok {
			continue
		}
		key := prefix + "/" + rel
		if err := objectstore.ValidateKey(key); err != nil {
			e.Logger.Warn("object store: blob write-through skipped an invalid key",
				"key", key, "error", err)
			continue
		}
		if err := store.backend.Put(ctx, key, path); err != nil {
			// A blob swept by a concurrent LRU eviction between the hook and
			// this Put lands here as a missing local file. It is not worth
			// distinguishing: either way the snapshot is the authority, and
			// either way the right response is a warning rather than an error
			// that stops a turn.
			w.failed.Add(1)
			e.Logger.Warn("object store: blob write-through failed, the turn-boundary snapshot will retry",
				"backend", store.cfg.BackendName, "key", key, "error", err)
			return stored
		}
		w.pushed.Add(1)
		stored = true
	}
	return stored
}

// flushBlobPushes makes everything pushed since the last flush durable.
func (e *Engine) flushBlobPushes(store *sessionObjectStore) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), objectStoreFlushTimeout)
	defer cancel()
	if err := store.backend.Flush(ctx); err != nil {
		e.Logger.Warn("object store: blob write-through flush failed, the turn-boundary snapshot will retry",
			"backend", store.cfg.BackendName, "error", err)
	}
}

// stopBlobWriteThrough detaches the hook, drains what is queued and waits for
// the worker to exit. Idempotent, and a no-op when nothing was installed.
//
// The hook is removed before the drain so a blob written during shutdown
// cannot be queued behind the barrier it is supposed to be inside, and well
// before finalizeObjectStore closes the backend so no upload can race a closed
// handle.
func (e *Engine) stopBlobWriteThrough() {
	w := e.blobPushes
	if w == nil {
		return
	}
	e.blobPushes = nil
	if e.Session != nil {
		e.Session.setBlobPush(nil)
	}

	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
	case <-time.After(blobWriteThroughDrainTimeout):
		e.Logger.Warn("object store: blob write-through drain timed out; " +
			"anything still queued is covered by the shutdown snapshot")
	}

	pushed, failed, dropped := w.pushed.Load(), w.failed.Load(), w.dropped.Load()
	if pushed == 0 && failed == 0 && dropped == 0 {
		return
	}
	e.Logger.Info("object store: blob write-through summary",
		"objects_pushed", pushed, "failed", failed, "dropped", dropped)
}
