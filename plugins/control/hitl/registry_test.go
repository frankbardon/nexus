package hitl

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/frankbardon/nexus/pkg/events"
)

// fakeBus is a minimal Emit-only stand-in. Records every call and lets a
// test wait for the first event of a given type.
type fakeBus struct {
	mu     sync.Mutex
	events []fakeEvent
	cond   *sync.Cond
}

type fakeEvent struct {
	Type    string
	Payload any
}

func newFakeBus() *fakeBus {
	b := &fakeBus{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *fakeBus) Emit(eventType string, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, fakeEvent{Type: eventType, Payload: payload})
	b.cond.Broadcast()
	return nil
}

func (b *fakeBus) waitFor(t *testing.T, eventType string, timeout time.Duration) fakeEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		for _, ev := range b.events {
			if ev.Type == eventType {
				return ev
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for %s after %s; saw events: %v", eventType, timeout, b.events)
		}
		// Watchdog goroutine broadcasts when the deadline elapses so
		// cond.Wait() returns and we re-check.
		timer := time.AfterFunc(remaining, func() {
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		})
		b.cond.Wait()
		timer.Stop()
	}
}

// stubBus is a non-blocking no-op emitter for tests that don't care about
// observed events.
type stubBus struct{}

func (stubBus) Emit(_ string, _ any) error { return nil }

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRegistryPersistRequestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	reg, err := newRegistry(dir, newTestLogger(), stubBus{})
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	defer reg.Close()

	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "hitl-turn-1-call-1",
		SessionID:       "2026-05-03-001",
		TurnID:          "turn-1",
		RequesterPlugin: pluginID,
		ActionKind:      "tool.invoke",
		ActionRef:       map[string]any{"tool": "shell", "args": map[string]any{"command": "echo hi"}},
		Mode:            events.HITLModeChoices,
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Approve", Kind: events.ChoiceAllow},
			{ID: "reject", Label: "Reject", Kind: events.ChoiceReject},
		},
		DefaultChoiceID: "reject",
		Prompt:          "approve shell command?",
	}
	if err := reg.persistRequest(req); err != nil {
		t.Fatalf("persistRequest: %v", err)
	}

	path := filepath.Join(dir, req.ID+requestSuffix)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request file: %v", err)
	}
	var rec requestFile
	if err := yaml.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.RequestID != req.ID {
		t.Fatalf("RequestID = %q; want %q", rec.RequestID, req.ID)
	}
	if rec.SessionID != req.SessionID {
		t.Fatalf("SessionID = %q; want %q", rec.SessionID, req.SessionID)
	}
	if rec.Mode != string(req.Mode) {
		t.Fatalf("Mode = %q; want %q", rec.Mode, req.Mode)
	}
	if len(rec.Choices) != 2 || rec.Choices[0].ID != "allow" || rec.Choices[1].ID != "reject" {
		t.Fatalf("Choices = %+v; want allow, reject", rec.Choices)
	}
	if rec.Choices[0].Kind != string(events.ChoiceAllow) {
		t.Fatalf("Choices[0].Kind = %q; want allow", rec.Choices[0].Kind)
	}
	if rec.CreatedAt.IsZero() {
		t.Fatal("CreatedAt should be populated")
	}
	if rec.ActionRef["tool"] != "shell" {
		t.Fatalf("ActionRef.tool = %v; want shell", rec.ActionRef["tool"])
	}
}

func TestRegistryPersistRequestRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	reg, err := newRegistry(dir, newTestLogger(), stubBus{})
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	defer reg.Close()

	bad := []string{"", "../escape", "a/b", `c\d`, ".", ".."}
	for _, id := range bad {
		req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: id, Prompt: "x"}
		if err := reg.persistRequest(req); err == nil {
			t.Fatalf("persistRequest(%q) succeeded; want error", id)
		}
	}
}

func TestRegistryWatchEmitsResponse(t *testing.T) {
	dir := t.TempDir()
	bus := newFakeBus()
	reg, err := newRegistry(dir, newTestLogger(), bus)
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	defer reg.Close()

	req := events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "hitl-r1",
		Prompt: "ok?",
		Mode:   events.HITLModeChoices,
		Choices: []events.HITLChoice{
			{ID: "allow", Label: "Approve"},
		},
	}
	if err := reg.persistRequest(req); err != nil {
		t.Fatalf("persistRequest: %v", err)
	}

	respPath, err := writeResponseFile(dir, req.ID, events.HITLResponse{SchemaVersion: events.HITLResponseVersion, ChoiceID: "allow",
		FreeText: "lgtm",
	})
	if err != nil {
		t.Fatalf("writeResponseFile: %v", err)
	}

	ev := bus.waitFor(t, "hitl.responded", 2*time.Second)
	resp, ok := ev.Payload.(events.HITLResponse)
	if !ok {
		t.Fatalf("payload type = %T; want events.HITLResponse", ev.Payload)
	}
	if resp.RequestID != req.ID {
		t.Fatalf("RequestID = %q; want %q", resp.RequestID, req.ID)
	}
	if resp.ChoiceID != "allow" {
		t.Fatalf("ChoiceID = %q; want allow", resp.ChoiceID)
	}
	if resp.FreeText != "lgtm" {
		t.Fatalf("FreeText = %q; want lgtm", resp.FreeText)
	}

	// Both files must be removed once the response is dispatched.
	if waitForRemoval(filepath.Join(dir, req.ID+requestSuffix)) != nil {
		t.Fatal("request file was not removed after response dispatch")
	}
	if waitForRemoval(respPath) != nil {
		t.Fatal("response file was not removed after dispatch")
	}
}

func TestRegistryCloseRemovesPendingRequests(t *testing.T) {
	dir := t.TempDir()
	reg, err := newRegistry(dir, newTestLogger(), stubBus{})
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}

	if err := reg.persistRequest(events.HITLRequest{SchemaVersion: events.HITLRequestVersion, ID: "hitl-r2", Prompt: "x"}); err != nil {
		t.Fatalf("persistRequest: %v", err)
	}
	path := filepath.Join(dir, "hitl-r2"+requestSuffix)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-close stat: %v", err)
	}
	reg.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("request file still present after Close: err=%v", err)
	}
	// Calling Close twice must be safe.
	reg.Close()
}

func TestListRequestFiles(t *testing.T) {
	dir := t.TempDir()
	reg, err := newRegistry(dir, newTestLogger(), stubBus{})
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	defer reg.Close()

	reqs := []events.HITLRequest{
		{ID: "hitl-a", Prompt: "first", ActionKind: "tool.invoke"},
		{ID: "hitl-b", Prompt: "second", ActionKind: "free_text"},
	}
	for _, r := range reqs {
		if err := reg.persistRequest(r); err != nil {
			t.Fatalf("persistRequest(%s): %v", r.ID, err)
		}
	}
	// Drop a stray non-request file to confirm it is ignored.
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write ignore: %v", err)
	}

	got, err := listRequestFiles(dir)
	if err != nil {
		t.Fatalf("listRequestFiles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d; want 2", len(got))
	}
	gotIDs := map[string]bool{got[0].RequestID: true, got[1].RequestID: true}
	if !gotIDs["hitl-a"] || !gotIDs["hitl-b"] {
		t.Fatalf("missing IDs: %v", gotIDs)
	}
}

func TestReadResponseFileCancelled(t *testing.T) {
	dir := t.TempDir()
	path, err := writeResponseFile(dir, "hitl-cancel", events.HITLResponse{SchemaVersion: events.HITLResponseVersion, Cancelled: true,
		CancelReason: "operator override",
	})
	if err != nil {
		t.Fatalf("writeResponseFile: %v", err)
	}
	resp, err := readResponseFile(path)
	if err != nil {
		t.Fatalf("readResponseFile: %v", err)
	}
	if !resp.Cancelled || resp.CancelReason != "operator override" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// waitForRemoval polls for a file to disappear within ~2s. Returns nil when
// the file is gone, or the last stat error if still present.
func waitForRemoval(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			lastErr = err
		} else {
			lastErr = nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		return os.ErrExist
	}
	return lastErr
}
