package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// This file exercises the lifecycle behind the A2A ingress: which contextId
// spawns an instance, which one reuses the running instance, and which one
// re-spawns a released session with -recall.
//
// The fake here is deliberately shallow in exactly one place — no process is
// exec()d — and real everywhere it matters. The spawn goes through
// ClaimServer.spawnInstance, so the capacity slot, the lease record, the ready
// wait and the session-id report are the production ones; the instance is bound
// to the lease with a real wsConn and its payloads reach the mapping through
// Registry.ioSink, which is the hook the gateway's read pump calls.

// ---- harness ----

// a2aSpawnHarness is one broker's worth of A2A lifecycle wiring over a fake
// instance runner.
type a2aSpawnHarness struct {
	t        *testing.T
	registry *Registry
	claims   *ClaimServer
	manager  *a2aLeaseManager
	runner   *a2aFakeRunner
	profile  AgentProfile
	contexts *a2aContextIndex
}

// a2aSpawnOption tweaks the harness before it is built.
type a2aSpawnOption func(*a2aSpawnSettings)

type a2aSpawnSettings struct {
	stateDir     string
	readyTimeout time.Duration
	stall        bool
	spawnDelay   time.Duration
	binaries     map[string]BinaryEntry
	profileBin   string
	configPath   string
}

// withA2AStateDir gives the harness a durable context index.
func withA2AStateDir(dir string) a2aSpawnOption {
	return func(s *a2aSpawnSettings) { s.stateDir = dir }
}

// withA2AStalledInstance makes every spawn produce a process that never signals
// ready, so the bounded ready wait is reachable.
func withA2AStalledInstance(readyTimeout time.Duration) a2aSpawnOption {
	return func(s *a2aSpawnSettings) {
		s.stall = true
		s.readyTimeout = readyTimeout
	}
}

// withA2ASpawnDelay slows every boot, widening the window two concurrent first
// messages race in.
func withA2ASpawnDelay(d time.Duration) a2aSpawnOption {
	return func(s *a2aSpawnSettings) { s.spawnDelay = d }
}

// withA2AProfileBinary points the profile at a registry entry by name.
func withA2AProfileBinary(name string) a2aSpawnOption {
	return func(s *a2aSpawnSettings) { s.profileBin = name }
}

// withA2AProfileConfig points the profile at a config path.
func withA2AProfileConfig(path string) a2aSpawnOption {
	return func(s *a2aSpawnSettings) { s.configPath = path }
}

func newA2ASpawnHarness(t *testing.T, opts ...a2aSpawnOption) *a2aSpawnHarness {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(configPath, []byte("engine:\n  name: fake\n"), 0o600); err != nil {
		t.Fatalf("write agent config: %v", err)
	}

	settings := a2aSpawnSettings{
		readyTimeout: 5 * time.Second,
		binaries:     testBinaryRegistry("/bin/true"),
		profileBin:   reservedBinaryName,
		configPath:   configPath,
	}
	for _, opt := range opts {
		opt(&settings)
	}

	registry := NewRegistry(testLogger(), 0)
	runner := &a2aFakeRunner{
		registry: registry,
		stall:    settings.stall,
		delay:    settings.spawnDelay,
	}
	cfg := Config{Binaries: settings.binaries, ListenAddr: "127.0.0.1:8080", StateDir: settings.stateDir}
	claims := NewClaimServer(testLogger(), registry, cfg, runner, nil)
	claims.readyTimeout = settings.readyTimeout
	claims.sessionReportGrace = 2 * time.Second

	var contexts *a2aContextIndex
	if settings.stateDir != "" {
		var err error
		contexts, err = openA2AContextIndex(testLogger(), cfg)
		if err != nil {
			t.Fatalf("openA2AContextIndex: %v", err)
		}
		t.Cleanup(func() { _ = contexts.Close() })
	}

	return &a2aSpawnHarness{
		t:        t,
		registry: registry,
		claims:   claims,
		manager:  newA2ALeaseManager(testLogger(), registry, claims, contexts),
		runner:   runner,
		contexts: contexts,
		profile: AgentProfile{
			Binary:         settings.profileBin,
			Config:         settings.configPath,
			ResolvedConfig: settings.configPath,
		},
	}
}

// acquire runs one acquisition for a context, with a recording set of hooks.
func (h *a2aSpawnHarness) acquire(contextID string, owner nexusauth.Principal) (a2aInstance, *recordingHooks, error) {
	h.t.Helper()
	rec := &recordingHooks{}
	inst, err := h.manager.Acquire(context.Background(), a2aLeaseRequest{
		profile:   h.profile,
		name:      "support",
		contextID: contextID,
		owner:     owner,
		hooks:     rec.hooks(),
	})
	return inst, rec, err
}

// mustAcquire fails the test if the acquisition did not produce an instance.
func (h *a2aSpawnHarness) mustAcquire(contextID string) (a2aInstance, *recordingHooks) {
	h.t.Helper()
	inst, rec, err := h.acquire(contextID, anonymousOwner())
	if err != nil {
		h.t.Fatalf("Acquire(%q): %v", contextID, err)
	}
	return inst, rec
}

// recordingHooks captures what a leased instance reported.
type recordingHooks struct {
	mu       sync.Mutex
	payloads []brokerIOMessage
	gone     []string
}

func (r *recordingHooks) hooks() a2aInstanceHooks {
	return a2aInstanceHooks{
		Deliver: func(msg brokerIOMessage) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.payloads = append(r.payloads, msg)
		},
		Gone: func(reason string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.gone = append(r.gone, reason)
		},
	}
}

func (r *recordingHooks) delivered() []brokerIOMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]brokerIOMessage(nil), r.payloads...)
}

func (r *recordingHooks) goneReasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.gone...)
}

// a2aFakeRunner stands in for exec: it records every spawn, binds a real wsConn
// to the lease as the instance connection, and drives the ready/session-report
// handshake the claim spine waits on.
type a2aFakeRunner struct {
	registry *Registry
	stall    bool
	delay    time.Duration
	startErr error

	mu    sync.Mutex
	specs []spawnSpec
	insts []*a2aFakeInstance
	seq   int
}

// a2aFakeInstance is one spawned "process": its lease, its socket and its
// process handle.
type a2aFakeInstance struct {
	leaseID   string
	sessionID string
	conn      *wsConn
	proc      *fakeProcess
}

// exit makes the process report as exited, which is the signal every teardown
// path in the broker converges on.
func (i *a2aFakeInstance) exit() { i.proc.exit() }

// sent decodes every frame the broker queued for this instance.
func (i *a2aFakeInstance) sent(t *testing.T) []brokerIOMessage {
	t.Helper()
	var out []brokerIOMessage
	for {
		select {
		case data := <-i.conn.send:
			frame, err := brokerframe.Decode(data)
			if err != nil {
				t.Fatalf("decode outbound frame: %v", err)
			}
			if frame.Signal != brokerframe.SignalIO {
				continue
			}
			msg, err := decodeIOPayload(frame.Payload)
			if err != nil {
				t.Fatalf("decode outbound io payload: %v", err)
			}
			out = append(out, msg)
		default:
			return out
		}
	}
}

func (r *a2aFakeRunner) start(_ context.Context, spec spawnSpec) (processHandle, error) {
	if r.startErr != nil {
		return nil, r.startErr
	}
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.specs = append(r.specs, spec)
	r.mu.Unlock()

	proc := newFakeProcess(9000 + seq)
	sessionID := spec.recallSessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("session-%d", seq)
	}
	inst := &a2aFakeInstance{
		leaseID:   spec.leaseID,
		sessionID: sessionID,
		conn:      newWSConn(nil),
		proc:      proc,
	}
	r.mu.Lock()
	r.insts = append(r.insts, inst)
	r.mu.Unlock()

	if r.stall {
		// Never dials back: the claim spine's bounded ready wait is what ends this.
		return proc, nil
	}

	go func() {
		if r.delay > 0 {
			time.Sleep(r.delay)
		}
		// The secret is minted and recorded by the claim spine before the runner
		// is invoked, and handed to this fake in the spawnSpec, so echoing it is
		// exactly what a real instance does with its environment.
		if err := r.registry.AttachInstance(spec.leaseID, inst.conn, spec.spawnSecret); err != nil {
			return
		}
		r.registry.MarkReady(spec.leaseID)
		r.registry.MarkSessionID(spec.leaseID, sessionID)
	}()
	return proc, nil
}

func (r *a2aFakeRunner) spawns() []spawnSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]spawnSpec(nil), r.specs...)
}

func (r *a2aFakeRunner) instances() []*a2aFakeInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*a2aFakeInstance(nil), r.insts...)
}

// ---- the four acquisition cases ----

// TestA2AUnknownContextColdSpawns pins the headline behaviour: a message on a
// context nobody has seen boots an instance, with no -recall, and the payload
// reaches it over the instance socket.
func TestA2AUnknownContextColdSpawns(t *testing.T) {
	h := newA2ASpawnHarness(t)

	inst, _ := h.mustAcquire("ctx-cold")

	spawns := h.runner.spawns()
	if len(spawns) != 1 {
		t.Fatalf("spawns = %d, want exactly one cold spawn", len(spawns))
	}
	if spawns[0].recallSessionID != "" {
		t.Errorf("cold spawn carried -recall %q; a context nobody has seen has no session to resume",
			spawns[0].recallSessionID)
	}
	if spawns[0].binaryName != reservedBinaryName {
		t.Errorf("spawned binary = %q, want the profile's entry %q", spawns[0].binaryName, reservedBinaryName)
	}

	if err := inst.SendIO(brokerIOMessage{Type: ioTypeInput, Content: "hello"}); err != nil {
		t.Fatalf("SendIO: %v", err)
	}
	sent := h.runner.instances()[0].sent(t)
	if len(sent) != 1 || sent[0].Type != ioTypeInput || sent[0].Content != "hello" {
		t.Fatalf("instance received %+v, want one input payload", sent)
	}
}

// TestA2AKnownContextReusesLiveInstance pins that a second message on a live
// conversation joins the running engine rather than booting a second one — which
// is what keeps the history intact without any replay.
func TestA2AKnownContextReusesLiveInstance(t *testing.T) {
	h := newA2ASpawnHarness(t)

	first, _ := h.mustAcquire("ctx-live")
	first.Release()
	second, _ := h.mustAcquire("ctx-live")

	if got := len(h.runner.spawns()); got != 1 {
		t.Fatalf("spawns = %d, want 1: a live conversation must not boot a second instance", got)
	}
	// Both handles drive the same instance.
	if err := second.SendIO(brokerIOMessage{Type: ioTypeInput, Content: "again"}); err != nil {
		t.Fatalf("SendIO: %v", err)
	}
	sent := h.runner.instances()[0].sent(t)
	if len(sent) != 1 || sent[0].Content != "again" {
		t.Fatalf("instance received %+v, want the second turn's input", sent)
	}
}

// TestA2AReleaseDoesNotEndTheLease pins the release semantics this ingress
// needs: a task finishing hands its instance back to the conversation, it does
// not tear the lease down. Releasing per turn would make every message a cold
// boot.
func TestA2AReleaseDoesNotEndTheLease(t *testing.T) {
	h := newA2ASpawnHarness(t)

	inst, _ := h.mustAcquire("ctx-release")
	leaseID := h.runner.instances()[0].leaseID
	inst.Release()

	if !h.registry.InstanceAttached(leaseID) {
		t.Fatal("releasing a task tore its lease down; the conversation's instance must survive the turn")
	}
}

// TestA2ADeadLeaseRespawnsWithRecall is the transparent-resume case: the
// conversation's instance is released (as the idle sweeper would), and the next
// message boots a new one with -recall <session id> so the engine replays the
// history. The client is told nothing.
func TestA2ADeadLeaseRespawnsWithRecall(t *testing.T) {
	h := newA2ASpawnHarness(t)

	inst, hooks := h.mustAcquire("ctx-resume")
	first := h.runner.instances()[0]

	// Release the lease the way the idle sweeper does — the shared teardown path,
	// not a bespoke one. The fake process exits at once so no grace elapses.
	first.exit()
	if err := h.registry.releaseLease(first.leaseID, reasonIdle, time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	// The task attached to it is told its instance went away, which is what turns
	// a released instance into a settled task rather than a hung stream.
	waitForWithin(t, time.Second, func() bool { return len(hooks.goneReasons()) > 0 })
	inst.Release()

	// The next message on the same context resumes.
	if _, _ = h.mustAcquire("ctx-resume"); true {
		spawns := h.runner.spawns()
		if len(spawns) != 2 {
			t.Fatalf("spawns = %d, want 2: a dead lease must be re-spawned", len(spawns))
		}
		if spawns[1].recallSessionID != first.sessionID {
			t.Errorf("re-spawn carried -recall %q, want %q: history must be replayed, not lost",
				spawns[1].recallSessionID, first.sessionID)
		}
	}
}

// TestA2ACrashedInstanceRespawnsWithRecall covers the other way a lease dies:
// the instance exits on its own and the crash watcher removes the lease. The
// next message resumes exactly as it does after an idle release.
func TestA2ACrashedInstanceRespawnsWithRecall(t *testing.T) {
	h := newA2ASpawnHarness(t)

	inst, hooks := h.mustAcquire("ctx-crash")
	first := h.runner.instances()[0]

	// The crash watcher is armed by the claim spine; the exit is the crash.
	go h.registry.watchExit(first.leaseID)
	first.exit()
	waitForWithin(t, 2*time.Second, func() bool { return !h.registry.Has(first.leaseID) })
	waitForWithin(t, time.Second, func() bool { return len(hooks.goneReasons()) > 0 })
	inst.Release()

	h.mustAcquire("ctx-crash")
	spawns := h.runner.spawns()
	if len(spawns) != 2 {
		t.Fatalf("spawns = %d, want 2 after a crash", len(spawns))
	}
	if spawns[1].recallSessionID != first.sessionID {
		t.Errorf("post-crash re-spawn carried -recall %q, want %q",
			spawns[1].recallSessionID, first.sessionID)
	}
}

// TestA2AConcurrentFirstMessagesSpawnOneInstance is the classic race in this
// design: several clients (or one client's retries) hitting a brand new context
// at once must produce ONE instance, not one each.
func TestA2AConcurrentFirstMessagesSpawnOneInstance(t *testing.T) {
	// A deliberately slow boot widens the window the race lives in.
	h := newA2ASpawnHarness(t, withA2ASpawnDelay(30*time.Millisecond))

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	insts := make([]a2aInstance, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			insts[i], _, errs[i] = h.acquire("ctx-race", anonymousOwner())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: Acquire: %v", i, err)
		}
		if insts[i] == nil {
			t.Fatalf("caller %d got no instance", i)
		}
	}
	if got := len(h.runner.spawns()); got != 1 {
		t.Fatalf("spawns = %d, want exactly 1 for %d concurrent first messages on one context", got, callers)
	}
	if got := len(h.registry.Snapshot().Leases); got != 1 {
		t.Fatalf("leases = %d, want 1: a duplicate spawn also leaks a capacity slot", got)
	}
}

// TestA2ASeparateContextsSpawnSeparateInstances is the other half of the
// isolation claim: two conversations are two instances and two sessions.
func TestA2ASeparateContextsSpawnSeparateInstances(t *testing.T) {
	h := newA2ASpawnHarness(t)

	h.mustAcquire("ctx-a")
	h.mustAcquire("ctx-b")

	if got := len(h.runner.spawns()); got != 2 {
		t.Fatalf("spawns = %d, want 2: separate contexts must not share an instance", got)
	}
	insts := h.runner.instances()
	if insts[0].sessionID == insts[1].sessionID {
		t.Error("two contexts were given one session")
	}
}

// TestA2AContextsAreScopedByPrincipal pins that a contextId names a conversation
// PER CALLER. A2A lets a client choose its own contextId, so without this a
// caller could name someone else's context and be handed their session.
func TestA2AContextsAreScopedByPrincipal(t *testing.T) {
	h := newA2ASpawnHarness(t)

	if _, _, err := h.acquire("shared-ctx", nexusauth.Principal{ID: "alice"}); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if _, _, err := h.acquire("shared-ctx", nexusauth.Principal{ID: "bob"}); err != nil {
		t.Fatalf("bob: %v", err)
	}

	if got := len(h.runner.spawns()); got != 2 {
		t.Fatalf("spawns = %d, want 2: one caller's context must not resolve to another's instance", got)
	}
}

// ---- durability ----

// TestA2AContextBindingSurvivesBrokerRestart is the durability criterion: a new
// manager over the same state_dir resumes a conversation whose instance is gone,
// which is what a restarted broker is.
func TestA2AContextBindingSurvivesBrokerRestart(t *testing.T) {
	stateDir := t.TempDir()
	first := newA2ASpawnHarness(t, withA2AStateDir(stateDir))

	inst, _ := first.mustAcquire("ctx-durable")
	original := first.runner.instances()[0]
	inst.Release()
	// The broker goes away; so does its instance.
	original.exit()
	if err := first.registry.releaseLease(original.leaseID, "manual release", time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	if err := first.contexts.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	// A fresh broker: new registry, new manager, same state_dir.
	second := newA2ASpawnHarness(t, withA2AStateDir(stateDir))
	second.mustAcquire("ctx-durable")

	spawns := second.runner.spawns()
	if len(spawns) != 1 {
		t.Fatalf("spawns after restart = %d, want 1", len(spawns))
	}
	if spawns[0].recallSessionID != original.sessionID {
		t.Errorf("post-restart spawn carried -recall %q, want %q: the context → session binding must be durable",
			spawns[0].recallSessionID, original.sessionID)
	}
}

// TestA2AWithoutStateDirForgetsAcrossRestart states the documented cost of
// running with no state_dir, so it is a decision rather than a surprise.
func TestA2AWithoutStateDirForgetsAcrossRestart(t *testing.T) {
	first := newA2ASpawnHarness(t)
	inst, _ := first.mustAcquire("ctx-volatile")
	original := first.runner.instances()[0]
	inst.Release()
	original.exit()
	if err := first.registry.releaseLease(original.leaseID, "manual release", time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}

	second := newA2ASpawnHarness(t)
	second.mustAcquire("ctx-volatile")
	if got := second.runner.spawns()[0].recallSessionID; got != "" {
		t.Errorf("spawn carried -recall %q; with no state_dir a restarted broker knows no context", got)
	}
}

// TestA2AAdoptsALiveLeaseAlreadyRunningTheSession pins the guard against booting
// a second engine over one session directory — the shape a broker restart with a
// surviving instance takes.
func TestA2AAdoptsALiveLeaseAlreadyRunningTheSession(t *testing.T) {
	stateDir := t.TempDir()
	first := newA2ASpawnHarness(t, withA2AStateDir(stateDir))
	inst, _ := first.mustAcquire("ctx-survivor")
	inst.Release()
	if err := first.contexts.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	// A new manager over the SAME registry: the instance survived, the manager's
	// in-memory table did not.
	survivorSession := first.runner.instances()[0].sessionID
	reborn := newA2ALeaseManager(testLogger(), first.registry, first.claims, mustOpenContextIndex(t, stateDir))
	rec := &recordingHooks{}
	if _, err := reborn.Acquire(context.Background(), a2aLeaseRequest{
		profile:   first.profile,
		name:      "support",
		contextID: "ctx-survivor",
		owner:     anonymousOwner(),
		hooks:     rec.hooks(),
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if got := len(first.runner.spawns()); got != 1 {
		t.Fatalf("spawns = %d, want 1: a live instance already running session %s must be adopted, not duplicated",
			got, survivorSession)
	}
}

func mustOpenContextIndex(t *testing.T, dir string) *a2aContextIndex {
	t.Helper()
	idx, err := openA2AContextIndex(testLogger(), Config{StateDir: dir})
	if err != nil {
		t.Fatalf("openA2AContextIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// ---- failure classification ----

// TestA2AUnknownBinaryRejectsTheTask pins that a profile naming a registry entry
// this broker does not offer settles the task at REJECTED — a refusal, not a
// hang and not a five-hundred.
func TestA2AUnknownBinaryRejectsTheTask(t *testing.T) {
	h := newA2ASpawnHarness(t, withA2AProfileBinary("vision"))

	_, _, err := h.acquire("ctx-bad-binary", anonymousOwner())
	if err == nil {
		t.Fatal("Acquire succeeded for a profile naming an unknown binary")
	}
	state, reason, classified := a2aSpawnOutcome(err)
	if !classified {
		t.Fatalf("failure was not classified onto a task state: %v", err)
	}
	if state != a2a.TaskStateRejected {
		t.Errorf("state = %s, want REJECTED for a request this broker refused", state)
	}
	if !strings.Contains(reason, "vision") {
		t.Errorf("reason %q does not name the rejected binary", reason)
	}
	if got := len(h.registry.Snapshot().Leases); got != 0 {
		t.Errorf("leases = %d after a refused spawn, want 0: a refusal must consume nothing", got)
	}
}

// TestA2AUnreadableConfigRejectsTheTask covers the other refusal: a profile
// whose config file cannot be read.
func TestA2AUnreadableConfigRejectsTheTask(t *testing.T) {
	h := newA2ASpawnHarness(t, withA2AProfileConfig(filepath.Join(t.TempDir(), "absent.yaml")))

	_, _, err := h.acquire("ctx-bad-config", anonymousOwner())
	if err == nil {
		t.Fatal("Acquire succeeded for a profile whose config does not exist")
	}
	state, _, classified := a2aSpawnOutcome(err)
	if !classified || state != a2a.TaskStateRejected {
		t.Errorf("state = %s (classified=%v), want REJECTED", state, classified)
	}
	if got := len(h.runner.spawns()); got != 0 {
		t.Errorf("spawns = %d, want 0: nothing may be exec()d for an unreadable config", got)
	}
}

// TestA2AReadyTimeoutFailsTheTask pins the bounded ready wait: an instance that
// never comes up settles the task at FAILED rather than leaving the client on a
// stream that never ends.
func TestA2AReadyTimeoutFailsTheTask(t *testing.T) {
	h := newA2ASpawnHarness(t, withA2AStalledInstance(150*time.Millisecond))

	started := time.Now()
	_, _, err := h.acquire("ctx-stalled", anonymousOwner())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Acquire succeeded despite an instance that never signalled ready")
	}
	state, _, classified := a2aSpawnOutcome(err)
	if !classified || state != a2a.TaskStateFailed {
		t.Errorf("state = %s (classified=%v), want FAILED for a boot that never came up", state, classified)
	}
	if elapsed > 5*time.Second {
		t.Errorf("acquisition took %s; a failed boot must be bounded by the ready timeout, never a hang", elapsed)
	}
	if got := len(h.registry.Snapshot().Leases); got != 0 {
		t.Errorf("leases = %d after a failed boot, want 0", got)
	}
}

// TestA2AUnwiredProviderStaysAProtocolError pins the boundary the classification
// draws: a provider that is not wired at all is NOT a task state, because "this
// broker cannot run agents" is a property of the deployment, not of the task. A
// client told its task FAILED would go looking at its own request.
func TestA2AUnwiredProviderStaysAProtocolError(t *testing.T) {
	if _, _, classified := a2aSpawnOutcome(errNoLeaseProvider); classified {
		t.Error("an unwired provider was classified as a task state; it must stay a protocol error")
	}
	// And a classified failure still unwraps to its cause, so a log or an
	// errors.Is check upstream sees what actually went wrong.
	cause := errors.New("boom")
	if !errors.Is(a2aFailedSpawn("could not start", cause), cause) {
		t.Error("a classified spawn failure lost its cause")
	}
}

// ---- payload routing ----

// TestA2AInstancePayloadsReachTheAttachedTasks pins that the registry IO sink —
// the hook the gateway's instance read pump calls — reaches every task attached
// to the lease.
func TestA2AInstancePayloadsReachTheAttachedTasks(t *testing.T) {
	h := newA2ASpawnHarness(t)

	instA, recA := h.mustAcquire("ctx-fanout")
	_, recB := h.mustAcquire("ctx-fanout")
	leaseID := h.runner.instances()[0].leaseID

	sink := h.registry.ioSink(leaseID)
	if sink == nil {
		t.Fatal("the lease has no io sink; instance payloads would never reach a task")
	}
	sink(brokerIOMessage{Type: ioTypeOutput, Content: "hi", TurnID: "t1"})

	for name, rec := range map[string]*recordingHooks{"first": recA, "second": recB} {
		got := rec.delivered()
		if len(got) != 1 || got[0].Content != "hi" {
			t.Errorf("%s observer received %+v, want the output payload", name, got)
		}
	}

	// A detached task stops being fed.
	instA.Release()
	sink(brokerIOMessage{Type: ioTypeOutput, Content: "second", TurnID: "t1"})
	if got := len(recA.delivered()); got != 1 {
		t.Errorf("a released task received %d payloads, want 1: Release must detach it", got)
	}
	if got := len(recB.delivered()); got != 2 {
		t.Errorf("the still-attached task received %d payloads, want 2", got)
	}
}

// TestA2ASendBumpsLeaseActivity pins that A2A traffic resets the idle timer, so
// idle release keeps working for A2A-created leases WITHOUT reaping one that is
// in active use.
func TestA2ASendBumpsLeaseActivity(t *testing.T) {
	h := newA2ASpawnHarness(t)
	inst, _ := h.mustAcquire("ctx-activity")
	leaseID := h.runner.instances()[0].leaseID

	before := h.registry.Snapshot().Leases[0].LastActivity
	time.Sleep(2 * time.Millisecond)
	if err := inst.SendIO(brokerIOMessage{Type: ioTypeInput, Content: "still here"}); err != nil {
		t.Fatalf("SendIO: %v", err)
	}
	after := h.registry.Snapshot().Leases[0].LastActivity
	if !after.After(before) {
		t.Errorf("last activity %s did not advance past %s; an A2A conversation would be reaped mid-turn",
			after, before)
	}

	// And the lease is still an ordinary lease the idle sweeper can reap.
	if ids := h.registry.idleLeases(time.Now().Add(time.Hour)); len(ids) != 1 || ids[0] != leaseID {
		t.Errorf("idleLeases = %v, want the A2A lease to be reapable like any other", ids)
	}
}

// waitForWithin polls until cond holds or the deadline passes. It is the
// bounded sibling of gateway_test.go's waitFor, which uses a fixed window.
func waitForWithin(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}
