package hitlsynthesizer_test

// External package so the test can boot a real engine — the cost-attribution
// tag seeder is installed by Engine.Boot, not by the contract harness — with
// the plugin registered from outside, without an import cycle.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	hitlsynthesizer "github.com/frankbardon/nexus/plugins/control/hitl_synthesizer"
)

const synthPluginID = "nexus.control.hitl_synthesizer"

func bootWithSynthesizer(t *testing.T) *engine.Engine {
	t.Helper()
	root := t.TempDir()
	cfg := fmt.Sprintf(`core:
  log_level: error
  sessions:
    root: %s
  storage:
    root: %s
plugins:
  active:
    - %s
  %s:
    cache_enabled: false
`, filepath.Join(root, "sessions"), root, synthPluginID, synthPluginID)
	eng, err := engine.NewFromBytes([]byte(cfg))
	if err != nil {
		t.Fatalf("NewFromBytes: %v", err)
	}
	eng.Registry.Register(synthPluginID, hitlsynthesizer.New)
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// stubProvider answers every llm.request synchronously, echoing metadata the
// way the real providers do, and records what it was sent.
type stubProvider struct {
	mu   sync.Mutex
	seen []events.LLMRequest
}

func (s *stubProvider) install(bus engine.EventBus) {
	bus.Subscribe("llm.request", func(e engine.Event[any]) {
		req, ok := e.Payload.(events.LLMRequest)
		if !ok {
			return
		}
		s.mu.Lock()
		s.seen = append(s.seen, req)
		s.mu.Unlock()
		engine.PublishLLMResponse(bus, events.LLMResponse{
			SchemaVersion: events.LLMResponseVersion,
			RequestID:     req.RequestID,
			Content:       "Approve running the shell command?",
			Metadata:      req.Metadata,
		})
	})
}

func (s *stubProvider) requests() []events.LLMRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.LLMRequest(nil), s.seen...)
}

func askForPrompt(t *testing.T, bus engine.EventBus) *events.HITLRequest {
	t.Helper()
	req := &events.HITLRequest{
		SchemaVersion:     events.HITLRequestVersion,
		ID:                "hitl-1",
		ActionKind:        "tool.invoke",
		ActionRef:         map[string]any{"tool": "shell", "command": "ls"},
		PromptSynthesizer: hitlsynthesizer.CapabilityName,
	}
	if _, err := bus.EmitVetoable("before:hitl.requested", req); err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	return req
}

// The synthesizer's request used to be a bare Emit("llm.request"), which the
// engine's tag seeder never saw. Through engine.PublishLLMRequest it carries
// the session dimensions alongside the plugin's own tags.
func TestSynthesizerRequestCarriesSessionTags(t *testing.T) {
	eng := bootWithSynthesizer(t)
	sp := &stubProvider{}
	sp.install(eng.Bus)

	hreq := askForPrompt(t, eng.Bus)

	reqs := sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(reqs))
	}
	if got := reqs[0].Tags["session_id"]; got != eng.Session.ID {
		t.Fatalf("session_id tag = %q, want %q (tags=%v)", got, eng.Session.ID, reqs[0].Tags)
	}
	if got := reqs[0].Tags["source_plugin"]; got != synthPluginID {
		t.Fatalf("source_plugin tag = %q, want %q", got, synthPluginID)
	}
	if hreq.Prompt != "Approve running the shell command?" {
		t.Fatalf("prompt not synthesized: %q", hreq.Prompt)
	}
}

// A request gate now sees the synthesizer's request; a veto keeps it off the
// provider and the plugin falls back to its template prompt.
func TestSynthesizerRequestHonorsVeto(t *testing.T) {
	eng := bootWithSynthesizer(t)
	sp := &stubProvider{}
	sp.install(eng.Bus)
	eng.Bus.Subscribe("before:llm.request", func(e engine.Event[any]) {
		vp := e.Payload.(*engine.VetoablePayload)
		vp.Veto = engine.VetoResult{Vetoed: true, Reason: "test gate"}
	})

	hreq := askForPrompt(t, eng.Bus)

	if n := len(sp.requests()); n != 0 {
		t.Fatalf("vetoed request reached the provider %d time(s)", n)
	}
	if hreq.Prompt == "" {
		t.Fatal("no fallback prompt after veto")
	}
}
