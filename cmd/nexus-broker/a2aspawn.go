package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// This file wires the A2A ingress to the broker's instance lifecycle. It is the
// implementation of a2aLeaseProvider, and it is where "an A2A request spins up
// an isolated Nexus instance" actually happens.
//
// THE ONE IDEA. An A2A client holds a contextId. The broker holds leases. The
// client must never learn the second thing exists, so this file owns the whole
// of the mapping between them:
//
//	contextId ──(this file, durable)──▶ engine session id ──(claim spine)──▶ lease
//
// The middle term is what makes the trick work. A lease is mortal — it is
// released when the conversation goes quiet, and it dies when its instance
// crashes — but an engine session is a directory on disk that outlives every
// process that ever opened it. So a message arriving on a context whose lease is
// gone is not an error to report; it is a session to resume. The broker spawns a
// new instance with `-recall <session id>`, the engine replays the history, and
// the client's next turn continues the conversation it was having. It is told
// nothing.
//
// WHAT IS DELIBERATELY NOT HERE. Nothing in this file translates anything: the
// A2A frames are a2atask.go's business and the IO envelope is a2aio.go's. The
// seam between them is a2alease.go, and it exists precisely so this file can be
// replaced (a warm pool, a remote spawner) without a line of translation
// changing.
//
// CONCURRENCY. Acquisition for ONE context is serialized by that context's own
// mutex, held across the whole spawn. That is what makes two simultaneous first
// messages on a new context produce exactly one instance: the second waits, then
// finds the first one's instance live and reuses it. Contexts do not contend
// with each other — the manager's own lock is held only for map bookkeeping,
// never across a spawn.

// maxA2AContextEntries bounds the in-memory context table.
//
// The table's keys are conversations, and a conversation has no retirement
// event, so without a cap the map would grow one entry per context this broker
// ever serves. It matches maxA2AContextBindings because the two hold the same
// key space, one in memory and one on disk.
//
// Eviction only ever removes an entry with NO live instance and NO acquisition
// in flight, so it can never orphan a running process or race a spawn. An
// evicted context is not lost when a state_dir is configured — the durable index
// still names its session. With no state_dir it degrades to what a broker with
// no state_dir already does across a restart: the next message starts a fresh
// conversation.
const maxA2AContextEntries = 4096

// a2aContextEntry is one A2A conversation's continuity state.
//
// It is the in-memory half of the contextId → session → lease chain; the durable
// half is a2aContextIndex. The two answer different questions: the entry knows
// which instance is running RIGHT NOW, the index knows which session to recall
// when none is.
type a2aContextEntry struct {
	// key is the composite (owner, profile, context) key this entry is filed
	// under, kept so eviction can delete it without recomputing.
	key string

	// mu serializes ACQUISITION for this context and is held across the entire
	// spawn — up to readyTimeout.
	//
	// Holding a lock across a process spawn is normally a smell; here it is the
	// mechanism. Two concurrent first messages on one context must produce ONE
	// instance, and the only way to guarantee that without a lock spanning the
	// spawn is a reservation protocol that would have to handle a reservation
	// whose spawn then failed. The blast radius is one conversation: this lock is
	// per context, and the manager's own lock is never held while waiting on it.
	mu sync.Mutex

	// sessionID is the engine session serving this context, guarded by mu. It is
	// what `-recall` is handed when the instance has to be started again.
	//
	// Empty means unknown: a context that has never run, or one whose instance
	// never reported a session id inside the grace window. Unknown is not an
	// error — it starts a fresh session, exactly as the first message did.
	sessionID string

	// binding is the live instance serving this context, guarded by mu. Nil means
	// there is none: never started, released while idle, or crashed. All three
	// are the same instruction — start one.
	binding *a2aLeaseBinding

	// refs and lastUsed are the eviction bookkeeping, guarded by the MANAGER's
	// lock rather than by mu. They are deliberately not under mu so that pruning
	// never has to wait on an acquisition that is in the middle of a 30-second
	// boot.
	refs     int
	lastUsed time.Time

	// bound mirrors "binding != nil" under the manager's lock, for the same
	// reason: eviction must be able to see that an entry is serving a live
	// instance without taking mu.
	bound bool
}

// a2aLeaseManager is the broker's A2A lease provider: it maps conversations onto
// engine sessions, and engine sessions onto spawned instances, reusing the claim
// path's spawn spine for every process it starts.
type a2aLeaseManager struct {
	logger   *slog.Logger
	registry *Registry

	// claims owns the spawn spine. The manager calls ClaimServer.spawnInstance
	// rather than exec()ing anything itself, so an A2A-created instance gets the
	// same capacity slot, the same spawn secret, the same recorded-binary
	// reconciliation, the same bounded ready wait and the same crash watcher a
	// POST /claim instance gets. A parallel spawn path is precisely how those five
	// things drift apart.
	claims *ClaimServer

	// contexts is the durable context → session index. Nil is fully supported and
	// means "no state_dir": continuity then lasts as long as this process does.
	contexts *a2aContextIndex

	mu      sync.Mutex
	entries map[string]*a2aContextEntry
}

// newA2ALeaseManager builds the provider. A nil contexts index disables durable
// continuity; everything else still works.
func newA2ALeaseManager(logger *slog.Logger, registry *Registry, claims *ClaimServer, contexts *a2aContextIndex) *a2aLeaseManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &a2aLeaseManager{
		logger:   logger,
		registry: registry,
		claims:   claims,
		contexts: contexts,
		entries:  make(map[string]*a2aContextEntry),
	}
}

// Acquire returns the instance a turn on req.contextID runs on, starting one if
// there is not already a live one.
//
// The four cases it resolves, in the order it resolves them:
//
//  1. A LIVE INSTANCE for this context — the common case for the second and
//     every later message in a conversation. The turn joins it and the history is
//     whatever the running engine holds, which is the whole conversation.
//  2. A LIVE LEASE ALREADY RUNNING THIS CONTEXT'S SESSION that this manager has
//     lost track of. That is the shape of a broker restart: recovery restored the
//     lease, the surviving instance reattached, and the manager's table is empty.
//     It is adopted rather than duplicated, because two engines on one session
//     directory interleave into one history and the damage surfaces much later.
//  3. A KNOWN SESSION WITH NO LIVE LEASE — the conversation whose instance was
//     idle-released or crashed. A new instance is spawned with `-recall <session
//     id>` so the engine replays the history. The client is told nothing: it sent
//     a message and it gets an answer, which is all A2A promises it.
//  4. NOTHING KNOWN — a new conversation. A fresh instance, a fresh session, and
//     the binding recorded so cases 1 to 3 can apply next time.
//
// Every failure it can return is CLASSIFIED (see a2aSpawnError) so the ingress
// can settle the task at REJECTED or FAILED rather than hanging or answering
// with a protocol error about machinery the client does not know about.
func (m *a2aLeaseManager) Acquire(ctx context.Context, req a2aLeaseRequest) (a2aInstance, error) {
	entry := m.entryFor(a2aContextKey(req.owner.ID, req.name, req.contextID))
	defer m.releaseEntry(entry)

	// Held across the whole of what follows, spawn included. See a2aContextEntry.mu.
	entry.mu.Lock()
	defer entry.mu.Unlock()

	// 1. The instance this manager already started for the context.
	if b := entry.binding; b != nil {
		if !b.gone() && m.registry.InstanceAttached(b.leaseID) {
			return b.attach(req.hooks), nil
		}
		// The lease is gone or its instance dropped. Forget it here as well as in
		// the watcher: the watcher is what normally clears this, but a socket that
		// dropped without the process exiting would otherwise leave a binding that
		// can never deliver anything.
		entry.binding = nil
		m.setBound(entry, false)
	}

	// The session to continue. The entry's own memory wins over the index: it is
	// the fresher of the two and is what THIS process last saw.
	sessionID := entry.sessionID
	if sessionID == "" {
		sessionID, _ = m.contexts.lookup(req.owner.ID, req.name, req.contextID)
		entry.sessionID = sessionID
	}

	// 2. A live lease already running that session — adopt it rather than spawn a
	//    second engine over the same session directory.
	if sessionID != "" {
		leaseID, attached, found := m.registry.LeaseForSession(sessionID)
		switch {
		case found && attached:
			b := m.bindLease(leaseID)
			entry.binding = b
			m.setBound(entry, true)
			m.watch(entry, b)
			m.logger.Info("a2a turn adopted a live instance already running its session",
				"profile", req.name, "context_id", req.contextID,
				"session_id", sessionID, "lease_id", leaseID)
			return b.attach(req.hooks), nil
		case found:
			// The lease exists but its instance has not dialled back. This is the
			// restart window, and it is the one case where refusing beats acting:
			// spawning now would put a second engine on a session directory the
			// reattaching instance is about to open. FAILED rather than REJECTED
			// because a retry a moment later succeeds.
			return nil, a2aFailedSpawn(fmt.Sprintf(
				"the agent for profile %q is restarting and is not reachable yet; send the message again",
				req.name), nil)
		}
	}

	// 3 and 4. Start an instance — resuming sessionID when there is one, fresh
	//          when there is not. One call covers both, because the claim spine
	//          already treats an empty session id as "new session".
	spawn, err := m.spawn(ctx, req, sessionID)
	if err != nil {
		return nil, err
	}

	if spawn.sessionID != "" {
		entry.sessionID = spawn.sessionID
		// Recorded here rather than at the end of the turn: the binding is knowable
		// now, and a turn that never finishes (a crash mid-answer) must still leave
		// the conversation resumable. Recording is best-effort and never fails the
		// turn.
		m.contexts.record(req.owner.ID, req.name, req.contextID, spawn.sessionID)
	}

	b := m.bindLease(spawn.leaseID)
	entry.binding = b
	m.setBound(entry, true)
	m.watch(entry, b)

	m.logger.Info("a2a turn started an agent instance",
		"profile", req.name, "context_id", req.contextID,
		"session_id", spawn.sessionID, "lease_id", spawn.leaseID,
		"pid", spawn.pid, "binary", spawn.binary, "resumed", sessionID != "")
	return b.attach(req.hooks), nil
}

// spawn boots one instance for a profile through the shared claim spine.
//
// The profile's config file is read at spawn time rather than cached at boot,
// which matches POST /claim: a claim carries the config text in its body, so
// what a claim spawns is whatever the caller sent at that moment. Reading here
// gives an operator the same property for a profile — edit the file, and the
// next conversation boots the new one — without any reload machinery.
func (m *a2aLeaseManager) spawn(ctx context.Context, req a2aLeaseRequest, sessionID string) (instanceSpawn, error) {
	configPath := req.profile.ResolvedConfig
	if configPath == "" {
		// A Config parsed without touching the filesystem (LoadConfigFromBytes)
		// carries no resolved path. Fall back to the declared one so such a config
		// is usable rather than mysteriously unspawnable.
		configPath = req.profile.Config
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		// REJECTED, not FAILED: nothing was attempted, and no retry of the same
		// message will help — the operator has to fix the profile.
		return instanceSpawn{}, a2aRejectedSpawn(fmt.Sprintf(
			"the configuration for agent profile %q could not be read, so no instance was started",
			req.name), err)
	}
	if len(config) == 0 {
		return instanceSpawn{}, a2aRejectedSpawn(fmt.Sprintf(
			"the configuration for agent profile %q is empty, so no instance was started", req.name), nil)
	}

	// The claim body an A2A turn implies. `Binary` is the profile's entry name,
	// which on a resume is reconciled against the one recorded for the session —
	// so a conversation whose session was created by another build is refused
	// here exactly as a /claim resume would be, rather than being replayed under
	// a foreign engine.
	spawn, failure := m.claims.spawnInstance(ctx, claimRequest{
		Config:    string(config),
		SessionID: sessionID,
		Binary:    req.profile.Binary,
	}, req.owner)
	if failure != nil {
		m.logger.Warn("a2a turn could not start an agent instance",
			"profile", req.name, "context_id", req.contextID,
			"session_id", sessionID, "status", failure.status, "error", failure)
		return instanceSpawn{}, classifyA2ASpawnFailure(req.name, failure)
	}
	return spawn, nil
}

// classifyA2ASpawnFailure maps the claim spine's failure onto the A2A task state
// the client is answered with.
//
// The split is the claim spine's own 4xx/5xx classification, reused rather than
// re-derived: a request this broker refused (an unknown binary, a session bound
// to a build that is gone) is a REJECTED task, and a spawn that was attempted
// and did not come up (capacity, a boot that died, a ready timeout) is a FAILED
// one. Re-deriving it here would be a second opinion that could disagree with
// the status /claim returns for the identical condition.
//
// The per-principal admission caps land on the REJECTED side, because they are
// 429s: the broker had capacity and refused THIS caller, which is a statement
// about the request rather than about the spawn. The broker-wide capacity
// refusals stay 503s and therefore FAILED.
func classifyA2ASpawnFailure(profile string, f *claimFailure) error {
	if f.refusedByCaller() {
		return a2aRejectedSpawn(fmt.Sprintf(
			"this broker refused to start an agent instance for profile %q: %s", profile, f.Error()), f)
	}
	return a2aFailedSpawn(fmt.Sprintf(
		"an agent instance for profile %q could not be started: %s", profile, f.Error()), f)
}

// bindLease creates the per-lease binding and registers it as the lease's
// in-process IO observer.
func (m *a2aLeaseManager) bindLease(leaseID string) *a2aLeaseBinding {
	b := &a2aLeaseBinding{
		manager:   m,
		leaseID:   leaseID,
		observers: make(map[uint64]a2aInstanceHooks),
	}
	m.registry.SetIOSink(leaseID, b.deliver)
	return b
}

// watch starts the goroutine that notices the instance going away.
//
// The signal it waits on is the lease's process-exit channel, and that is the
// right one because EVERY way an instance stops passes through it: a graceful
// release sends a shutdown frame and waits for the exit, the idle sweeper funnels
// into that same release, and a crash is nothing but an unexpected exit. One
// channel therefore covers idle release, manual release, crash and broker
// shutdown, and no teardown path has to remember to notify anything.
//
// The channel is read once and never re-armed: a lease's process exits once.
func (m *a2aLeaseManager) watch(entry *a2aContextEntry, b *a2aLeaseBinding) {
	exited := m.registry.ExitedChan(b.leaseID)
	if exited == nil {
		// The lease is already gone. Settle whatever attached to it rather than
		// leaving a binding nothing will ever close.
		b.markGone("the agent instance is no longer available")
		return
	}
	// One extra reference for the watcher, so the entry cannot be evicted while
	// an instance is running under it.
	m.retainEntry(entry)
	go func() {
		defer m.releaseEntry(entry)
		<-exited

		// The entry is cleared BEFORE the observers are told, and the two locks are
		// taken in sequence rather than nested. Ordering matters twice over: a
		// message arriving during the teardown must find no binding (so it
		// re-spawns rather than sending into a dead socket), and Gone reaches
		// handlers that call back into the binding to detach, so it must not run
		// under the binding's lock.
		entry.mu.Lock()
		if entry.binding == b {
			entry.binding = nil
			m.setBound(entry, false)
		}
		entry.mu.Unlock()

		// WAIT FOR THE FRAMES BEFORE DECLARING THE INSTANCE GONE. The exit channel
		// above says the process was reaped; it says NOTHING about the frames it
		// wrote on its way out, because those bytes are in a socket buffer the
		// broker has not read yet. markGone settles every attached task, and a
		// task's terminal latch then DROPS whatever the read pump translates next
		// — so acting on the exit alone loses a `status: thinking` that had
		// already arrived (the task never reaches WORKING) and, worse, loses an
		// `output` plus `status: idle` that had already arrived, reporting FAILED
		// with no answer for a turn that COMPLETED.
		//
		// The socket EOF is the signal that IS ordered after the last frame, so
		// that is what this waits on. It is bounded and, because the process is
		// already gone, normally instant. See Registry.awaitInstanceDrain.
		m.registry.awaitInstanceDrain(b.leaseID)

		b.markGone(a2aInstanceGoneReason)
		m.logger.Info("a2a agent instance stopped", "lease_id", b.leaseID)
	}()
}

// ---- entry table ----

// entryFor returns the context's entry, creating it if needed, and takes a
// reference so it cannot be evicted while in use.
func (m *a2aLeaseManager) entryFor(key string) *a2aContextEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		entry = &a2aContextEntry{key: key}
		m.entries[key] = entry
		m.pruneLocked()
	}
	entry.refs++
	entry.lastUsed = time.Now()
	return entry
}

// retainEntry takes an additional reference.
func (m *a2aLeaseManager) retainEntry(entry *a2aContextEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.refs++
}

// releaseEntry drops a reference. It does NOT delete the entry: a context with
// no live instance still remembers which session to recall, which is the whole
// point for a broker running without a state_dir. Eviction is pruneLocked's job
// and is bounded by the table cap instead.
func (m *a2aLeaseManager) releaseEntry(entry *a2aContextEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.refs > 0 {
		entry.refs--
	}
	entry.lastUsed = time.Now()
}

// setBound records whether an entry is currently serving a live instance, for
// eviction's benefit. Caller holds entry.mu; this takes the manager's lock, which
// is the only nesting order this file ever uses.
func (m *a2aLeaseManager) setBound(entry *a2aContextEntry, bound bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.bound = bound
}

// pruneLocked evicts the least recently used entries that hold nothing, until
// the table fits under the cap. Caller MUST hold m.mu.
//
// An entry with a live instance or an acquisition in flight is NEVER evicted, at
// any table size: dropping one would let a concurrent message spawn a second
// instance for a conversation that already has one, which is the exact failure
// the per-entry lock exists to prevent. A table entirely full of live entries
// therefore exceeds the cap rather than breaking, and the cap is generous enough
// that reaching that state means max_concurrent is far larger.
func (m *a2aLeaseManager) pruneLocked() {
	if len(m.entries) <= maxA2AContextEntries {
		return
	}
	evictable := make([]*a2aContextEntry, 0, len(m.entries))
	for _, entry := range m.entries {
		if entry.refs == 0 && !entry.bound {
			evictable = append(evictable, entry)
		}
	}
	sort.Slice(evictable, func(i, j int) bool {
		if evictable[i].lastUsed.Equal(evictable[j].lastUsed) {
			return evictable[i].key < evictable[j].key
		}
		return evictable[i].lastUsed.Before(evictable[j].lastUsed)
	})
	for _, entry := range evictable {
		if len(m.entries) <= maxA2AContextEntries {
			return
		}
		delete(m.entries, entry.key)
	}
}

// ---- one lease, many tasks ----

// a2aLeaseBinding is the broker's view of one leased instance as an A2A
// participant: the fan-out of its IO payloads to whatever tasks are running on
// it, and the one place a payload is sent back to it.
//
// It is per LEASE and not per task because a conversation's instance outlives
// any single turn — that is the entire reason a second message on a context is
// cheap — so the thing that talks to the instance has to outlive a turn too.
//
// FAN-OUT, not routing. Every observer is handed every payload, and each task
// decides for itself which ones are its own (see a2aTask.bindTurn).
//
// That is correct rather than approximate because of what sits above it: the
// ingress admits ONE ACTIVE TASK per conversation (a2aqueue.go), and a task only
// attaches here once it has been admitted. So the set of observers on a live
// instance is at most one task plus, briefly, one that is detaching as it
// settles — never two tasks watching two different turns and guessing which
// output is theirs. Fan-out then costs nothing and loses nothing, and the
// turn-id binding a task does is a consistency check rather than a router.
type a2aLeaseBinding struct {
	manager *a2aLeaseManager
	leaseID string

	mu        sync.Mutex
	observers map[uint64]a2aInstanceHooks
	nextID    uint64
	// dead latches once the instance is known to be unreachable, so a late
	// payload or a late send is a no-op rather than a panic or a hang.
	dead bool
}

// attach registers one task's hooks and returns the handle it drives the
// instance through.
func (b *a2aLeaseBinding) attach(hooks a2aInstanceHooks) *a2aLeaseInstance {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.observers[id] = hooks
	dead := b.dead
	b.mu.Unlock()

	inst := &a2aLeaseInstance{binding: b, id: id}
	if dead {
		// Attaching to an instance that died between the liveness check and here.
		// The task is told at once rather than left waiting for a turn that will
		// never start.
		if hooks.Gone != nil {
			hooks.Gone(a2aInstanceGoneReason)
		}
	}
	return inst
}

// detach removes one task's hooks. It is what Release means for a broker lease:
// the TASK is finished with the instance, and the instance carries on serving
// the conversation until it is released for idleness or dies.
//
// This is deliberately not a lease teardown. Releasing the lease at the end of
// every turn would make each message in a conversation a cold boot, which is
// both slow and — because history would then have to be replayed from disk every
// time — a different conversation from the one a single instance holds.
func (b *a2aLeaseBinding) detach(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.observers, id)
}

// deliver fans one instance payload out to every attached task.
//
// It runs on the gateway's instance read-pump goroutine. The hooks are collected
// under the lock and CALLED OUTSIDE it, which is required rather than tidy: a
// payload that ends a turn runs the task's terminal sequence, which calls
// Release, which calls detach — so calling under the lock would deadlock on the
// first turn that ever completed.
func (b *a2aLeaseBinding) deliver(msg brokerIOMessage) {
	for _, hooks := range b.snapshot() {
		if hooks.Deliver != nil {
			hooks.Deliver(msg)
		}
	}
}

// markGone latches the binding dead and tells every attached task, once.
//
// It is idempotent because more than one thing can notice an instance is gone: a
// dropped socket, a reaped process, a broker shutting down. A task that has
// already settled treats a late Gone as a no-op of its own (see
// a2aTask.instanceGone), so a duplicate costs nothing.
func (b *a2aLeaseBinding) markGone(reason string) {
	b.mu.Lock()
	if b.dead {
		b.mu.Unlock()
		return
	}
	b.dead = true
	hooks := make([]a2aInstanceHooks, 0, len(b.observers))
	for _, h := range b.observers {
		hooks = append(hooks, h)
	}
	b.mu.Unlock()

	// Outside the lock, for the same reason deliver is: Gone settles a task, and
	// settling a task detaches it.
	for _, h := range hooks {
		if h.Gone != nil {
			h.Gone(reason)
		}
	}
}

// gone reports whether the instance is known to be unreachable.
func (b *a2aLeaseBinding) gone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dead
}

// snapshot copies the attached hooks so they can be invoked without the lock.
func (b *a2aLeaseBinding) snapshot() []a2aInstanceHooks {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]a2aInstanceHooks, 0, len(b.observers))
	for _, h := range b.observers {
		out = append(out, h)
	}
	return out
}

// send delivers one IO payload to the instance over its dial-back socket.
//
// It never blocks on the network: the frame is queued on the gateway's existing
// per-connection send buffer, and a full buffer is reported rather than waited
// on — a slow instance must not be able to stall the HTTP goroutine driving a
// turn.
func (b *a2aLeaseBinding) send(msg brokerIOMessage) error {
	if b.gone() {
		return fmt.Errorf("the agent instance for lease %s is no longer running", b.leaseID)
	}
	conn := b.manager.registry.InstanceConn(b.leaseID)
	if conn == nil {
		return fmt.Errorf("the agent instance for lease %s is not connected", b.leaseID)
	}
	data, err := encodeIOFrame(b.leaseID, msg)
	if err != nil {
		return err
	}
	if !conn.queue(data) {
		return fmt.Errorf("the agent instance for lease %s is not reading its input fast enough", b.leaseID)
	}
	// Stamp last activity exactly as the gateway's CLIENT read pump does, and for
	// the same reason: everything this ingress sends is real user input, so an
	// A2A-driven conversation resets the idle timer just as a WebSocket-driven one
	// does. Without this an A2A lease would be reaped mid-conversation while its
	// client was actively talking to it.
	b.manager.registry.markActivity(b.leaseID)
	return nil
}

// a2aLeaseInstance is one task's handle on a leased instance: the a2aInstance
// the ingress was handed.
//
// It is per TASK rather than per lease so that Release is scoped to the task
// that finished, not to the conversation. Several of them may point at one
// binding at once.
type a2aLeaseInstance struct {
	binding *a2aLeaseBinding
	id      uint64
	once    sync.Once
}

// SendIO delivers one payload to the instance.
func (i *a2aLeaseInstance) SendIO(msg brokerIOMessage) error { return i.binding.send(msg) }

// Release detaches this task from the instance. The lease survives: the
// conversation may well get another message, and the idle sweeper — not the end
// of a turn — is what decides when the instance has been quiet long enough to
// stop.
func (i *a2aLeaseInstance) Release() {
	i.once.Do(func() { i.binding.detach(i.id) })
}
