//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/providers/anthropic"
)

// TestJournalReplay_FunctionalEquivalence hand-crafts a source session
// journal, boots a fresh engine, replays the journal, and asserts:
//
//  1. The Anthropic provider's live-call counter stays at 0 — every
//     llm.request was served from the replay stash, not the API.
//  2. The journaled llm.response payloads were emitted on the live bus
//     during replay (verified via a wildcard subscription in the test).
//  3. The replayed io.input payload reaches the live bus as a typed
//     events.UserInput, not the raw map[string]any from JSON unmarshal.
//
// The test does not invoke the live HTTP path because there is no API
// key — short-circuit failure would manifest as an auth error from
// Anthropic, not a silent stash miss.
func TestJournalReplay_FunctionalEquivalence(t *testing.T) {
	sessionsRoot := t.TempDir()
	sourceID := "src-replay-session"
	sourceDir := filepath.Join(sessionsRoot, sourceID)

	if err := os.MkdirAll(filepath.Join(sourceDir, "metadata"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-craft a 2-turn journal. agent.turn.start/end frame each turn so
	// the coordinator's turn-sync subscription advances.
	journalDir := filepath.Join(sourceDir, "journal")
	w, err := journal.NewWriter(journalDir, journal.WriterOptions{
		FsyncMode:  journal.FsyncEveryEvent,
		BufferSize: 16,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	envelopes := []journal.Envelope{
		{Seq: 1, Type: "io.session.start", Payload: map[string]any{"session_id": sourceID}},
		{Seq: 2, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "first message"}},
		{Seq: 3, Type: "agent.turn.start"},
		{Seq: 4, Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "first reply from journal",
			Model:        "mock",
			FinishReason: "end_turn",
		}},
		{Seq: 5, Type: "agent.turn.end"},
		{Seq: 6, Type: "io.input", Payload: events.UserInput{SchemaVersion: events.UserInputVersion, Content: "second message"}},
		{Seq: 7, Type: "agent.turn.start"},
		{Seq: 8, Type: "llm.response", Payload: events.LLMResponse{SchemaVersion: events.LLMResponseVersion, Content: "second reply from journal",
			Model:        "mock",
			FinishReason: "end_turn",
		}},
		{Seq: 9, Type: "agent.turn.end"},
	}
	for i := range envelopes {
		w.Append(&envelopes[i])
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := w.Close(closeCtx); err != nil {
		t.Fatalf("Close source journal: %v", err)
	}

	// Build a YAML config rooted at the test's temp dir. The active plugin
	// list mirrors a minimal mock-mode config but omits mock_responses on
	// nexus.io.test — we want the live llm.request to reach the Anthropic
	// provider so its replay short-circuit can engage. Coordinator drives
	// io.input directly; the test plugin's empty inputs list keeps it
	// quiescent.
	cfgYAML := fmt.Sprintf(`
core:
  log_level: warn
  tick_interval: 5s
  max_concurrent_events: 100
  models:
    default: mock
    mock:
      provider: nexus.llm.anthropic
      model: mock
      max_tokens: 1024
  sessions:
    root: %s
    retention: 30d
    id_format: timestamp

plugins:
  active:
    - nexus.llm.anthropic
    - nexus.agent.react
    - nexus.memory.capped

  nexus.llm.anthropic:
    api_key: "sk-mock-not-used"

  nexus.agent.react:
    system_prompt: "Test."

  nexus.memory.capped:
    max_messages: 10
    persist: false
`, sessionsRoot)

	eng, err := engine.NewFromBytes([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	allplugins.RegisterAll(eng.Registry)

	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	// Collect llm.response and io.input emits during replay.
	var (
		mu             sync.Mutex
		llmResponses   []events.LLMResponse
		inputEmits     []events.UserInput
		ioInputBadType int
	)
	unsubResp := eng.Bus.Subscribe("llm.response", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.LLMResponse); ok {
			mu.Lock()
			llmResponses = append(llmResponses, r)
			mu.Unlock()
		}
	})
	defer unsubResp()
	unsubInput := eng.Bus.Subscribe("io.input", func(ev engine.Event[any]) {
		mu.Lock()
		defer mu.Unlock()
		switch p := ev.Payload.(type) {
		case events.UserInput:
			inputEmits = append(inputEmits, p)
		default:
			ioInputBadType++
		}
	})
	defer unsubInput()

	replayCtx, replayCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer replayCancel()
	if err := eng.ReplaySession(replayCtx, sourceID); err != nil {
		t.Fatalf("ReplaySession: %v", err)
	}

	// Locate the Anthropic plugin instance to read LiveCalls.
	var anthropicPlugin *anthropic.Plugin
	for _, p := range eng.Lifecycle.Plugins() {
		if p.ID() == "nexus.llm.anthropic" {
			if ap, ok := p.(*anthropic.Plugin); ok {
				anthropicPlugin = ap
			}
		}
	}
	if anthropicPlugin == nil {
		t.Fatal("anthropic plugin not found")
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	_ = eng.Stop(stopCtx)

	// 1. No API calls reached the wire.
	if got := anthropicPlugin.LiveCalls(); got != 0 {
		t.Errorf("anthropic.LiveCalls() = %d, want 0 — provider hit the API during replay", got)
	}

	// 2. io.input emits arrived as typed UserInput, not raw maps.
	mu.Lock()
	defer mu.Unlock()
	if ioInputBadType > 0 {
		t.Errorf("%d io.input emits had non-events.UserInput payloads — converter regression", ioInputBadType)
	}
	if len(inputEmits) != 2 {
		t.Errorf("io.input emits = %d, want 2 (got %+v)", len(inputEmits), inputEmits)
	} else {
		if inputEmits[0].Content != "first message" {
			t.Errorf("input[0].Content = %q", inputEmits[0].Content)
		}
		if inputEmits[1].Content != "second message" {
			t.Errorf("input[1].Content = %q", inputEmits[1].Content)
		}
	}

	// 3. The journaled llm.response content emitted on the live bus.
	want := map[string]bool{
		"first reply from journal":  false,
		"second reply from journal": false,
	}
	for _, r := range llmResponses {
		if _, ok := want[r.Content]; ok {
			want[r.Content] = true
		}
	}
	for content, seen := range want {
		if !seen {
			t.Errorf("expected llm.response %q to be replayed; collected = %+v", content, llmResponses)
		}
	}
}
