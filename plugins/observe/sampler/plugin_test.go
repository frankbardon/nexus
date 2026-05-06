package sampler

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
)

// fixtureSession creates a fake session workspace on disk: RootDir + an
// honestly-shaped journal directory + metadata/session.json with the supplied
// status. The status drives the failure_capture predicate; using a real
// journal layout means the snapshot path runs end-to-end.
func fixtureSession(t *testing.T, status string) *engine.SessionWorkspace {
	t.Helper()
	root := t.TempDir()
	sessionID := "fake-" + strings.ReplaceAll(t.Name(), "/", "_") + "-" + randomTag()
	rootDir := filepath.Join(root, sessionID)

	// Layout: <rootDir>/journal/{header.json,events.jsonl}, <rootDir>/metadata/session.json
	if err := os.MkdirAll(filepath.Join(rootDir, "journal"), 0o755); err != nil {
		t.Fatalf("mkdir journal: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "metadata"), 0o755); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}

	header := map[string]any{
		"schema_version": journal.SchemaVersion,
		"created_at":     time.Now().UTC().Format(time.RFC3339Nano),
		"fsync_mode":     "turn-boundary",
		"session_id":     sessionID,
	}
	hb, _ := json.MarshalIndent(header, "", "  ")
	if err := os.WriteFile(filepath.Join(rootDir, "journal", "header.json"), hb, 0o644); err != nil {
		t.Fatalf("write header: %v", err)
	}

	events := []journal.Envelope{
		{Seq: 1, Ts: time.Now().UTC(), Type: "io.session.start", Payload: map[string]any{"session_id": sessionID}},
		{Seq: 2, Ts: time.Now().UTC(), Type: "io.input", Payload: map[string]any{"content": "secret value"}},
		{Seq: 3, Ts: time.Now().UTC(), Type: "io.output", Payload: map[string]any{"content": "ok", "role": "assistant"}},
	}
	var buf strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(e)
		buf.Write(raw)
		buf.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(rootDir, "journal", "events.jsonl"), []byte(buf.String()), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}

	meta := map[string]any{
		"id":         sessionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		"status":     status,
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(rootDir, "metadata", "session.json"), mb, 0o644); err != nil {
		t.Fatalf("write session.json: %v", err)
	}

	return &engine.SessionWorkspace{
		ID:        sessionID,
		RootDir:   rootDir,
		StartedAt: time.Now(),
	}
}

func randomTag() string {
	b := make([]byte, 4)
	_, _ = io.ReadFull(rand.New(rand.NewSource(time.Now().UnixNano())), b)
	return hex.EncodeToString(b)
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initPlugin wires a sampler with the given config map and returns it
// ready to receive io.session.end events. Tests subscribe their own
// collector to the bus to inspect emissions.
func initPlugin(t *testing.T, cfgRaw map[string]any, sess *engine.SessionWorkspace) (*Plugin, engine.EventBus, func()) {
	t.Helper()
	bus := engine.NewEventBus()

	p := New().(*Plugin)
	if err := p.Init(engine.PluginContext{
		Config:  cfgRaw,
		Bus:     bus,
		Logger:  newTestLogger(),
		Session: sess,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cleanup := func() { _ = p.Shutdown(context.Background()) }
	t.Cleanup(cleanup)

	return p, bus, cleanup
}

// TestPlugin_Disabled_NoSubscriptions_NoCaptures confirms the off-by-default
// contract: a missing or disabled config produces an inert plugin even when
// io.session.end fires.
func TestPlugin_Disabled_NoSubscriptions_NoCaptures(t *testing.T) {
	for _, cfgRaw := range []map[string]any{
		nil,
		{},
		{"enabled": false, "rate": 1.0, "failure_capture": true},
	} {
		sess := fixtureSession(t, "completed")
		p := New().(*Plugin)
		bus := engine.NewEventBus()
		var captured atomic.Int32
		bus.Subscribe(EvalCandidateEventType, func(_ engine.Event[any]) {
			captured.Add(1)
		}, engine.WithSource("test"))

		if err := p.Init(engine.PluginContext{
			Config:  cfgRaw,
			Bus:     bus,
			Logger:  newTestLogger(),
			Session: sess,
		}); err != nil {
			t.Fatalf("Init disabled: %v", err)
		}
		if subs := p.Subscriptions(); len(subs) != 0 {
			t.Errorf("disabled plugin Subscriptions=%d, want 0", len(subs))
		}
		if em := p.Emissions(); len(em) != 0 {
			t.Errorf("disabled plugin Emissions=%v, want empty", em)
		}
		// Emit io.session.end. The plugin should ignore it.
		_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})
		if got := captured.Load(); got != 0 {
			t.Errorf("disabled plugin emitted %d candidates, want 0", got)
		}
		_ = p.Shutdown(context.Background())
	}
}

// TestPlugin_RateOne_AllSessionsCaptured: rate=1.0 → every io.session.end is
// snapshotted, the on-disk layout matches the contract, and exactly one
// eval.candidate fires.
func TestPlugin_RateOne_AllSessionsCaptured(t *testing.T) {
	sess := fixtureSession(t, "completed")
	outDir := t.TempDir()
	p, bus, _ := initPlugin(t, map[string]any{
		"enabled": true,
		"rate":    1.0,
		"out_dir": outDir,
	}, sess)

	var got []EvalCandidate
	var mu sync.Mutex
	bus.Subscribe(EvalCandidateEventType, func(e engine.Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		if c, ok := e.Payload.(EvalCandidate); ok {
			got = append(got, c)
		}
	}, engine.WithSource("test-collector"))

	_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Reason != "sampled" {
		t.Errorf("reason=%q, want sampled", got[0].Reason)
	}
	if got[0].SessionID != sess.ID {
		t.Errorf("session_id=%q, want %q", got[0].SessionID, sess.ID)
	}

	expectedCaseDir := filepath.Join(outDir, sess.ID)
	if got[0].CaseDir != expectedCaseDir {
		t.Errorf("case_dir=%q, want %q", got[0].CaseDir, expectedCaseDir)
	}
	if _, err := os.Stat(filepath.Join(expectedCaseDir, "journal", "header.json")); err != nil {
		t.Errorf("expected journal/header.json under %s: %v", expectedCaseDir, err)
	}
	if _, err := os.Stat(filepath.Join(expectedCaseDir, "journal", "events.jsonl")); err != nil {
		t.Errorf("expected journal/events.jsonl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(expectedCaseDir, "metadata.json")); err != nil {
		t.Errorf("expected metadata.json sibling: %v", err)
	}

	// metadata.json fields.
	mb, err := os.ReadFile(filepath.Join(expectedCaseDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var md snapshotMetadata
	if err := json.Unmarshal(mb, &md); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if md.Reason != "sampled" {
		t.Errorf("metadata.reason=%q, want sampled", md.Reason)
	}
	if md.SamplingRateAtCapture != 1.0 {
		t.Errorf("sampling_rate_at_capture=%v, want 1.0", md.SamplingRateAtCapture)
	}
	if md.SessionStatus != "completed" {
		t.Errorf("session_status=%q, want completed", md.SessionStatus)
	}
	if md.EngineVersion == "" {
		t.Errorf("engine_version is empty")
	}
	if md.CapturedAt == "" {
		t.Errorf("captured_at is empty")
	}

	_ = p
}

// TestPlugin_RateZero_FailureCaptureOnly: rate=0 + failure_capture=true →
// only failed sessions are captured.
func TestPlugin_RateZero_FailureCaptureOnly(t *testing.T) {
	t.Run("completed_skipped", func(t *testing.T) {
		sess := fixtureSession(t, "completed")
		outDir := t.TempDir()
		_, bus, _ := initPlugin(t, map[string]any{
			"enabled":         true,
			"rate":            0.0,
			"failure_capture": true,
			"out_dir":         outDir,
		}, sess)

		var captured atomic.Int32
		bus.Subscribe(EvalCandidateEventType, func(_ engine.Event[any]) { captured.Add(1) }, engine.WithSource("test"))

		_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

		if captured.Load() != 0 {
			t.Errorf("completed session captured %d times, want 0", captured.Load())
		}
		if _, err := os.Stat(filepath.Join(outDir, sess.ID)); !os.IsNotExist(err) {
			t.Errorf("expected no case dir for completed session, got err=%v", err)
		}
	})

	t.Run("failed_captured", func(t *testing.T) {
		sess := fixtureSession(t, "failed")
		outDir := t.TempDir()
		_, bus, _ := initPlugin(t, map[string]any{
			"enabled":         true,
			"rate":            0.0,
			"failure_capture": true,
			"out_dir":         outDir,
		}, sess)

		var got []EvalCandidate
		var mu sync.Mutex
		bus.Subscribe(EvalCandidateEventType, func(e engine.Event[any]) {
			mu.Lock()
			defer mu.Unlock()
			if c, ok := e.Payload.(EvalCandidate); ok {
				got = append(got, c)
			}
		}, engine.WithSource("test"))

		_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("got %d candidates, want 1", len(got))
		}
		if got[0].Reason != "failure_capture" {
			t.Errorf("reason=%q, want failure_capture", got[0].Reason)
		}
	})
}

// TestPlugin_RateZero_NoFailureCapture: rate=0 + failure_capture=false →
// nothing is ever captured, even on failed sessions.
func TestPlugin_RateZero_NoFailureCapture(t *testing.T) {
	for _, status := range []string{"completed", "failed", "errored"} {
		sess := fixtureSession(t, status)
		outDir := t.TempDir()
		_, bus, _ := initPlugin(t, map[string]any{
			"enabled":         true,
			"rate":            0.0,
			"failure_capture": false,
			"out_dir":         outDir,
		}, sess)
		var captured atomic.Int32
		bus.Subscribe(EvalCandidateEventType, func(_ engine.Event[any]) { captured.Add(1) }, engine.WithSource("test"))

		_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

		if captured.Load() != 0 {
			t.Errorf("status=%q captured %d times, want 0", status, captured.Load())
		}
	}
}

// TestPlugin_RateValidation: rate>1 errors out at Init.
func TestPlugin_RateValidation(t *testing.T) {
	sess := fixtureSession(t, "completed")
	p := New().(*Plugin)
	err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"enabled": true,
			"rate":    1.5,
			"out_dir": t.TempDir(),
		},
		Bus:     engine.NewEventBus(),
		Logger:  newTestLogger(),
		Session: sess,
	})
	if err == nil {
		t.Fatal("Init with rate=1.5 should fail validation")
	}
	if !strings.Contains(err.Error(), "rate") {
		t.Errorf("err=%v, want rate validation message", err)
	}
}

// TestPlugin_DeterministicRng_SeedReproducible: a seeded rng produces a
// stable accept/reject sequence at rate=0.5 — protects the rate path from
// flake.
func TestPlugin_DeterministicRng_SeedReproducible(t *testing.T) {
	sess := fixtureSession(t, "completed")
	outDir := t.TempDir()
	p := New().(*Plugin)
	bus := engine.NewEventBus()
	p.SetRandSource(rand.New(rand.NewSource(42)))
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"enabled":         true,
			"rate":            0.5,
			"failure_capture": false,
			"out_dir":         outDir,
		},
		Bus:     bus,
		Logger:  newTestLogger(),
		Session: sess,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// Roll the same seeded rng directly to know the answer.
	expectAccept := rand.New(rand.NewSource(42)).Float64() < 0.5
	var captured atomic.Int32
	bus.Subscribe(EvalCandidateEventType, func(_ engine.Event[any]) { captured.Add(1) }, engine.WithSource("test"))

	_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

	if expectAccept && captured.Load() != 1 {
		t.Errorf("seed=42 expected accept but no capture (got %d)", captured.Load())
	}
	if !expectAccept && captured.Load() != 0 {
		t.Errorf("seed=42 expected reject but got %d captures", captured.Load())
	}
}

// TestPlugin_Redactor_StubDropsPayload: a non-identity redactor replaces the
// payload of every line in the active events.jsonl segment.
func TestPlugin_Redactor_StubDropsPayload(t *testing.T) {
	sess := fixtureSession(t, "completed")
	outDir := t.TempDir()
	p := New().(*Plugin)
	p.SetRedactor(dropRedactor{})
	bus := engine.NewEventBus()
	if err := p.Init(engine.PluginContext{
		Config: map[string]any{
			"enabled": true,
			"rate":    1.0,
			"out_dir": outDir,
		},
		Bus:     bus,
		Logger:  newTestLogger(),
		Session: sess,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

	out, err := os.Open(filepath.Join(outDir, sess.ID, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("open snapshot events.jsonl: %v", err)
	}
	defer out.Close()
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lines := 0
	for scanner.Scan() {
		lines++
		var env journal.Envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			t.Fatalf("parse line %d: %v", lines, err)
		}
		if env.Payload != nil {
			t.Errorf("line %d (type=%s): payload should be dropped, got %v", lines, env.Type, env.Payload)
		}
		if env.Seq == 0 || env.Type == "" {
			t.Errorf("line %d: envelope metadata wiped (seq=%d type=%q)", lines, env.Seq, env.Type)
		}
	}
	if lines != 3 {
		t.Errorf("snapshot events.jsonl had %d lines, want 3", lines)
	}
}

// TestPlugin_Redactor_IdentityPreservesBytes: with the default IdentityRedactor
// the snapshot events.jsonl is byte-identical to the source.
func TestPlugin_Redactor_IdentityPreservesBytes(t *testing.T) {
	sess := fixtureSession(t, "completed")
	outDir := t.TempDir()
	_, bus, _ := initPlugin(t, map[string]any{
		"enabled": true,
		"rate":    1.0,
		"out_dir": outDir,
	}, sess)

	_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

	src, err := os.ReadFile(filepath.Join(sess.RootDir, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("read source events.jsonl: %v", err)
	}
	dst, err := os.ReadFile(filepath.Join(outDir, sess.ID, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("read snapshot events.jsonl: %v", err)
	}
	if string(src) != string(dst) {
		t.Errorf("identity redactor changed bytes\n  src=%q\n  dst=%q", src, dst)
	}
}

// TestPlugin_Concurrent_TwoSessions: two parallel sampler instances against
// distinct sessions both produce isolated samples without crossing files.
// Mirrors the production case where one engine has one sampler — but the
// helper exercises the no-shared-state contract just in case.
func TestPlugin_Concurrent_TwoSessions(t *testing.T) {
	outDir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	run := func(idx int) {
		defer wg.Done()
		sess := fixtureSession(t, "completed")
		_, bus, _ := initPlugin(t, map[string]any{
			"enabled": true,
			"rate":    1.0,
			"out_dir": outDir,
		}, sess)

		var got []EvalCandidate
		var mu sync.Mutex
		bus.Subscribe(EvalCandidateEventType, func(e engine.Event[any]) {
			mu.Lock()
			defer mu.Unlock()
			if c, ok := e.Payload.(EvalCandidate); ok {
				got = append(got, c)
			}
		}, engine.WithSource("test"))

		_ = bus.Emit("io.session.end", events.SessionInfo{SchemaVersion: events.SessionInfoVersion, ID: sess.ID, Transport: "test"})

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			errs <- &concurrentErr{idx: idx, msg: "missing candidate"}
			return
		}
		expect := filepath.Join(outDir, sess.ID, "journal", "events.jsonl")
		if _, err := os.Stat(expect); err != nil {
			errs <- &concurrentErr{idx: idx, msg: err.Error()}
			return
		}
	}
	wg.Add(2)
	go run(0)
	go run(1)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

type concurrentErr struct {
	idx int
	msg string
}

func (e *concurrentErr) Error() string { return "" + e.msg }
